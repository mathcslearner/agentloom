package api

// This file is API key authentication (tickets 6.1/6.2, ADR-007): key
// generation and hashing mechanics, the bearer verifier, and the
// scope-gate middleware that fronts every /v1 route per ADR-007's
// route→scope table (/healthz stays exempt for probes).

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/store"
)

// Scope is one authorization scope (ADR-007). Scope names are wire
// contract, like error codes: renaming one is a breaking change.
type Scope string

// The scope vocabulary. Admin implies every other scope; approve is
// reserved for M15's approval endpoints but assignable now.
const (
	ScopeSubmit  Scope = "submit"
	ScopeRead    Scope = "read"
	ScopeApprove Scope = "approve"
	ScopeAdmin   Scope = "admin"
)

// Scopes lists the vocabulary in display order.
func Scopes() []Scope { return []Scope{ScopeSubmit, ScopeRead, ScopeApprove, ScopeAdmin} }

// The key shape (ADR-007): "sk_" + base64url(32 random bytes, no
// padding). The lookup prefix — the only key material allowed in logs,
// listings, and the database — is the first keyLookupLen characters.
const (
	keyMarker      = "sk_"
	keyRandomBytes = 32
	keyEncodedLen  = 43 // base64.RawURLEncoding.EncodedLen(32)
	keyTotalLen    = len(keyMarker) + keyEncodedLen
	keyLookupLen   = len(keyMarker) + 8
)

// rootKeyID is the key_id logged and reported for the env-provided root
// credential, which has no database row.
const rootKeyID = "root"

// generateKey mints a fresh plaintext key from r, returning the
// plaintext (shown to the caller once, never stored), its lookup prefix,
// and the hex SHA-256 the store keeps.
func generateKey(r io.Reader) (plaintext, prefix, hashHex string, err error) {
	raw := make([]byte, keyRandomBytes)
	if _, err := io.ReadFull(r, raw); err != nil {
		return "", "", "", fmt.Errorf("reading key entropy: %w", err)
	}
	plaintext = keyMarker + base64.RawURLEncoding.EncodeToString(raw)
	return plaintext, plaintext[:keyLookupLen], hashKey(plaintext), nil
}

// hashKey is the storage form of a key: hex SHA-256 of the full
// plaintext. A fast hash is deliberate (ADR-007): the secret is 256
// random bits, so a KDF would add per-request cost for no threat-model
// gain.
func hashKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// keyShapeOK reports whether token has the exact sk_ wire shape. Checked
// before any lookup so garbage never reaches the store.
func keyShapeOK(token string) bool {
	if len(token) != keyTotalLen || !strings.HasPrefix(token, keyMarker) {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(token[len(keyMarker):])
	return err == nil
}

// hashEqual compares two hex SHA-256 strings in constant time.
func hashEqual(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// hasScope reports whether the granted set covers want. Admin implies
// every scope (ADR-007), so route checks never enumerate.
func hasScope(granted []string, want Scope) bool {
	return slices.Contains(granted, string(want)) || slices.Contains(granted, string(ScopeAdmin))
}

// validateScopes checks a requested scope set: non-empty, known
// vocabulary, no duplicates. Returns a client-facing message on failure.
func validateScopes(scopes []string) error {
	if len(scopes) == 0 {
		return errors.New("scopes must name at least one scope")
	}
	seen := make(map[string]bool, len(scopes))
	for _, s := range scopes {
		if !slices.Contains(Scopes(), Scope(s)) {
			return fmt.Errorf("unknown scope %q (valid: submit, read, approve, admin)", s)
		}
		if seen[s] {
			return fmt.Errorf("duplicate scope %q", s)
		}
		seen[s] = true
	}
	return nil
}

// identity is the authenticated caller: the key row's id (or "root" for
// the env credential) and its granted scopes.
type identity struct {
	keyID  string
	scopes []string
}

// ctxKeyIdentity keys the authenticated identity in the request context.
type ctxKeyIdentity struct{}

// identityInto returns ctx carrying the authenticated identity for
// downstream handlers and middleware (6.4's per-key rate limiting reads
// it via identityFrom).
func identityInto(ctx context.Context, id identity) context.Context {
	return context.WithValue(ctx, ctxKeyIdentity{}, id)
}

// identityFrom returns the identity requireScope stamped into the
// context, if the request authenticated.
func identityFrom(ctx context.Context) (identity, bool) {
	id, ok := ctx.Value(ctxKeyIdentity{}).(identity)
	return id, ok
}

// errUnauthorized collapses every credential failure — missing header,
// bad shape, unknown prefix, hash mismatch, revoked, expired — into one
// indistinguishable answer (ADR-007: the response never reveals whether
// a prefix exists or a key "almost worked").
var errUnauthorized = errors.New("missing or invalid API key")

// authenticate verifies the request's bearer credential: root-key
// compare first (constant-time against the boot-hashed env value), then
// one indexed read by prefix, constant-time hash compare, and the
// revocation/expiry predicates. It returns errUnauthorized for every
// credential failure and other errors only for server-side faults.
func (h *Handler) authenticate(r *http.Request) (identity, error) {
	header := r.Header.Get("Authorization")
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return identity{}, errUnauthorized
	}
	token = strings.TrimSpace(token)
	if !keyShapeOK(token) {
		return identity{}, errUnauthorized
	}
	presented := hashKey(token)

	if h.rootHash != "" && hashEqual(presented, h.rootHash) {
		return identity{keyID: rootKeyID, scopes: []string{string(ScopeAdmin)}}, nil
	}

	row, err := h.st.APIKeys().GetByPrefix(r.Context(), token[:keyLookupLen])
	switch {
	case errors.Is(err, store.ErrNotFound):
		return identity{}, errUnauthorized
	case err != nil:
		return identity{}, fmt.Errorf("reading api key: %w", err)
	}
	if !hashEqual(presented, row.KeyHash) {
		return identity{}, errUnauthorized
	}
	if row.RevokedAt != nil {
		return identity{}, errUnauthorized
	}
	if row.ExpiresAt != nil && !h.now().Before(*row.ExpiresAt) {
		return identity{}, errUnauthorized
	}
	return identity{keyID: row.ID.String(), scopes: row.Scopes}, nil
}

// requireScope authenticates the request and enforces one scope,
// answering 401 (credential failure) or 403 (valid key, missing scope)
// per ADR-007. Outcomes are logged with key_id and never with key
// material; the authenticated key_id is stamped onto the request logger
// (so downstream handler lines carry it), into the request context (for
// handlers and 6.4's per-key rate limiting), and back up to requestLog's
// per-request line via the authStamp slot.
func (h *Handler) requireScope(want Scope) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			id, err := h.authenticate(r)
			switch {
			case errors.Is(err, errUnauthorized):
				log.From(ctx).InfoContext(ctx, "api: request unauthorized",
					slog.String("path", r.URL.Path))
				w.Header().Set("WWW-Authenticate", "Bearer")
				writeError(w, http.StatusUnauthorized, ErrorDetail{
					Code: ErrCodeUnauthorized, Message: errUnauthorized.Error(),
				})
				return
			case err != nil:
				internalError(w, r, "authenticating request", err)
				return
			}
			if stamp, ok := authStampFrom(ctx); ok {
				stamp.keyID = id.keyID
			}
			if !hasScope(id.scopes, want) {
				log.From(ctx).InfoContext(ctx, "api: request forbidden",
					log.KeyID(id.keyID),
					slog.String("path", r.URL.Path),
					slog.String("missing_scope", string(want)))
				writeError(w, http.StatusForbidden, ErrorDetail{
					Code:    ErrCodeForbidden,
					Message: fmt.Sprintf("key lacks the %q scope", want),
				})
				return
			}
			logger := log.From(ctx).With(log.KeyID(id.keyID))
			ctx = identityInto(log.Into(ctx, logger), id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// cryptoRand is the production entropy source; tests inject their own
// through the Handler's keyRand field.
var cryptoRand io.Reader = rand.Reader
