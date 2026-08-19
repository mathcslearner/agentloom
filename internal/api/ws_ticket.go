package api

// Run-WebSocket tickets (ticket 16.3, ADR-018). A ticket is a short-lived,
// HMAC-signed, opaque token minted at POST /v1/runs/{id}/ws-ticket and passed
// to GET /v1/runs/{id}/ws as a query parameter. It exists so a browser never
// puts a long-lived bearer key in a WebSocket URL (which lands in server logs,
// proxies, and browser history): the ticket is scoped to one run and the read
// scope, and it expires quickly, so leaking it in a URL is low-risk.
//
// The wire form is "<payload-b64url>.<sig-b64url>", payload = compact JSON of
// wsTicketClaims, sig = HMAC-SHA256(secret, payload-b64url). Verification is
// self-contained (no store lookup): a valid signature over unexpired claims
// naming this run is accepted. A ticket is not single-use — a client may reuse
// it across reconnects within the TTL; after that it re-mints. Revocation lag
// is therefore bounded by the TTL, which is the reason to keep the TTL short.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// wsTicketVersion is the ticket schema version; a mismatch is rejected (a
// future breaking change bumps it).
const wsTicketVersion = 1

// Ticket audiences (ticket 16.4). The audience binds a ticket to exactly one
// endpoint: a run ticket carries wsAudRun and a run id; a firehose ticket
// carries wsAudFirehose and no run id. Verification checks the audience, so a
// run ticket presented to the firehose (or vice-versa) is rejected — a ticket
// grants access to exactly the stream it was minted for.
const (
	wsAudRun      = "run"
	wsAudFirehose = "firehose"
)

// wsTicketClaims is the signed payload. Kept minimal: the audience + run it
// authorizes, the minting key's id (for the connection's request log and
// rate-limit bucket), the expiry, and a nonce so two tickets minted in the same
// second differ (defense in depth; the signature already makes them
// unforgeable).
type wsTicketClaims struct {
	Version  int    `json:"v"`
	Audience string `json:"aud"`
	RunID    string `json:"run_id,omitempty"`
	KeyID    string `json:"key_id"`
	Expires  int64  `json:"exp"` // Unix seconds
	Nonce    string `json:"nonce"`
}

// errInvalidTicket collapses every ticket failure — bad shape, bad signature,
// wrong version, expired, wrong run — into one value, so the WS handshake never
// reveals which check failed (the ADR-007 uniform-401 discipline).
var errInvalidTicket = errors.New("invalid or expired ticket")

// mintWSTicket signs a run ticket (audience wsAudRun) for runID/keyID valid for
// ttl from now. secret must be non-empty (New guarantees a random one when the
// operator sets none).
func mintWSTicket(secret string, runID uuid.UUID, keyID string, now time.Time, ttl time.Duration) (string, time.Time, error) {
	return mintTicket(secret, wsAudRun, runID.String(), keyID, now, ttl)
}

// mintFirehoseWSTicket signs a firehose ticket (audience wsAudFirehose, no run
// id) for keyID valid for ttl from now (ticket 16.4).
func mintFirehoseWSTicket(secret, keyID string, now time.Time, ttl time.Duration) (string, time.Time, error) {
	return mintTicket(secret, wsAudFirehose, "", keyID, now, ttl)
}

func mintTicket(secret, audience, runID, keyID string, now time.Time, ttl time.Duration) (string, time.Time, error) {
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return "", time.Time{}, err
	}
	exp := now.Add(ttl)
	claims := wsTicketClaims{
		Version:  wsTicketVersion,
		Audience: audience,
		RunID:    runID,
		KeyID:    keyID,
		Expires:  exp.Unix(),
		Nonce:    base64.RawURLEncoding.EncodeToString(nonce),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", time.Time{}, err
	}
	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	sig := ticketSignature(secret, payloadB64)
	// Truncate the reported expiry to whole seconds so it matches the claim
	// (the wire carries Unix seconds); a client compares against it.
	return payloadB64 + "." + sig, time.Unix(exp.Unix(), 0), nil
}

// verifyWSTicket checks a run ticket against secret and returns its claims. It
// enforces: the two-part shape, a constant-time signature match, the schema
// version, the run audience, expiry against now, and that the ticket names
// wantRun. Every failure is errInvalidTicket.
func verifyWSTicket(secret, ticket string, wantRun uuid.UUID, now time.Time) (wsTicketClaims, error) {
	claims, err := verifyTicketSigned(secret, ticket, now)
	if err != nil {
		return wsTicketClaims{}, err
	}
	if claims.Audience != wsAudRun || claims.RunID != wantRun.String() {
		return wsTicketClaims{}, errInvalidTicket
	}
	return claims, nil
}

// verifyFirehoseWSTicket checks a firehose ticket (audience wsAudFirehose,
// no run scope) against secret (ticket 16.4). Every failure is errInvalidTicket.
func verifyFirehoseWSTicket(secret, ticket string, now time.Time) (wsTicketClaims, error) {
	claims, err := verifyTicketSigned(secret, ticket, now)
	if err != nil {
		return wsTicketClaims{}, err
	}
	if claims.Audience != wsAudFirehose {
		return wsTicketClaims{}, errInvalidTicket
	}
	return claims, nil
}

// verifyTicketSigned validates the shape, signature, version, and expiry common
// to every ticket; the audience/run checks are the caller's.
func verifyTicketSigned(secret, ticket string, now time.Time) (wsTicketClaims, error) {
	payloadB64, sig, ok := cutLast(ticket, '.')
	if !ok {
		return wsTicketClaims{}, errInvalidTicket
	}
	want := ticketSignature(secret, payloadB64)
	if subtle.ConstantTimeCompare([]byte(sig), []byte(want)) != 1 {
		return wsTicketClaims{}, errInvalidTicket
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return wsTicketClaims{}, errInvalidTicket
	}
	var claims wsTicketClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return wsTicketClaims{}, errInvalidTicket
	}
	if claims.Version != wsTicketVersion {
		return wsTicketClaims{}, errInvalidTicket
	}
	if !now.Before(time.Unix(claims.Expires, 0)) {
		return wsTicketClaims{}, errInvalidTicket
	}
	return claims, nil
}

// ticketSignature is the hex-free base64url HMAC-SHA256 over the encoded
// payload.
func ticketSignature(secret, payloadB64 string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payloadB64))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// cutLast splits s at the last occurrence of sep. Used because base64url never
// contains '.', so the last dot is unambiguously the sig separator.
func cutLast(s string, sep byte) (before, after string, found bool) {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == sep {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}
