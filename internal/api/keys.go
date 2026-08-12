package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// maxKeyNameLen bounds the human label — long enough for any real
// description, short enough that a hostile name can't bloat listings.
const maxKeyNameLen = 200

// createKeyAttempts bounds the regenerate-on-prefix-collision loop. A
// collision is ~impossible (~2⁴⁸ prefix space); hitting the bound three
// times in a row means something else is wrong.
const createKeyAttempts = 3

// handleCreateKey is POST /v1/keys (admin): mint a key, store hash +
// prefix, and return the plaintext — the only response that ever
// carries it. Expiry is requested as a TTL and resolved against the
// server's injected clock (ADR-007: clients don't supply timestamps).
func (h *Handler) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var req CreateKeyRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, ErrorDetail{
			Code: ErrCodeInvalidRequest, Message: "decoding request body: " + err.Error(),
		})
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	switch {
	case req.Name == "":
		writeError(w, http.StatusBadRequest, ErrorDetail{
			Code: ErrCodeInvalidRequest, Message: "name is required",
		})
		return
	case len(req.Name) > maxKeyNameLen:
		writeError(w, http.StatusBadRequest, ErrorDetail{
			Code: ErrCodeInvalidRequest, Message: "name exceeds 200 characters",
		})
		return
	}
	if err := validateScopes(req.Scopes); err != nil {
		writeError(w, http.StatusBadRequest, ErrorDetail{
			Code: ErrCodeInvalidRequest, Message: err.Error(),
		})
		return
	}
	now := h.now()
	var expiresAt *time.Time
	if req.TTL != "" {
		ttl, err := time.ParseDuration(req.TTL)
		if err != nil || ttl <= 0 {
			writeError(w, http.StatusBadRequest, ErrorDetail{
				Code: ErrCodeInvalidRequest, Message: "ttl must be a positive Go duration (e.g. \"720h\")",
			})
			return
		}
		t := now.Add(ttl)
		expiresAt = &t
	}

	// Mint and insert, regenerating on a prefix/hash unique collision.
	var (
		row       gen.ApiKey
		plaintext string
	)
	for attempt := 1; ; attempt++ {
		var prefix, hash string
		var err error
		plaintext, prefix, hash, err = generateKey(h.keyRand)
		if err != nil {
			internalError(w, r, "generating api key", err)
			return
		}
		row, err = h.st.APIKeys().Create(r.Context(), gen.CreateAPIKeyParams{
			Name:      req.Name,
			Prefix:    prefix,
			KeyHash:   hash,
			Scopes:    req.Scopes,
			CreatedAt: now,
			ExpiresAt: expiresAt,
		})
		if err == nil {
			break
		}
		var conflict *store.ConflictError
		if errors.As(err, &conflict) && attempt < createKeyAttempts {
			continue
		}
		internalError(w, r, "storing api key", err)
		return
	}

	log.From(r.Context()).InfoContext(r.Context(), "api key created",
		log.KeyID(row.ID.String()),
		slog.String("prefix", row.Prefix),
		slog.String("name", row.Name),
		slog.Any("scopes", row.Scopes))
	writeJSON(w, http.StatusCreated, CreateKeyResponse{
		KeyView: keyView(row),
		Key:     plaintext,
	})
}

// handleListKeys is GET /v1/keys (admin): every key, newest first,
// revoked and expired included — hashes and plaintext never.
func (h *Handler) handleListKeys(w http.ResponseWriter, r *http.Request) {
	rows, err := h.st.APIKeys().List(r.Context())
	if err != nil {
		internalError(w, r, "listing api keys", err)
		return
	}
	resp := ListKeysResponse{Keys: make([]KeyView, 0, len(rows))}
	for _, row := range rows {
		resp.Keys = append(resp.Keys, keyView(row))
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleRevokeKey is DELETE /v1/keys/{id} (admin): soft revocation.
// Idempotent — revoking an already-revoked key is a 204 no-op keeping
// the original revoked_at.
func (h *Handler) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "keyID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrorDetail{
			Code: ErrCodeInvalidRequest, Message: "key id is not a valid UUID",
		})
		return
	}
	revoked, err := h.st.APIKeys().Revoke(r.Context(), id, h.now())
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, ErrorDetail{
			Code: ErrCodeKeyNotFound, Message: "no api key with id " + id.String(),
		})
		return
	case err != nil:
		internalError(w, r, "revoking api key", err)
		return
	}
	log.From(r.Context()).InfoContext(r.Context(), "api key revoked",
		log.KeyID(id.String()),
		slog.Bool("already_revoked", !revoked))
	w.WriteHeader(http.StatusNoContent)
}

// keyView projects a key row onto the wire, dropping the hash.
func keyView(row gen.ApiKey) KeyView {
	return KeyView{
		ID:        row.ID.String(),
		Prefix:    row.Prefix,
		Name:      row.Name,
		Scopes:    row.Scopes,
		CreatedAt: row.CreatedAt,
		ExpiresAt: row.ExpiresAt,
		RevokedAt: row.RevokedAt,
	}
}
