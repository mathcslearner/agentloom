package notify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"
)

// Notification HTTP headers (ticket 15.5). The signature scheme mirrors the
// common webhook convention (Stripe/GitHub-style): a versioned HMAC over
// "<timestamp>.<body>" so a receiver both authenticates the sender and
// rejects a stale replay by checking the timestamp against a tolerance
// window.
const (
	// HeaderTimestamp carries the unix-seconds timestamp the signature covers.
	HeaderTimestamp = "X-Agentloom-Timestamp"
	// HeaderSignature carries "v1=<hex HMAC-SHA256>".
	HeaderSignature = "X-Agentloom-Signature"
	// HeaderDeliveryID carries the notification's dedupe key (the approval id).
	HeaderDeliveryID = "X-Agentloom-Delivery-Id"
	// HeaderEvent carries the event name (EventApprovalRequested).
	HeaderEvent = "X-Agentloom-Event"
)

// signatureVersion prefixes the signature value so the scheme can evolve
// without a receiver mis-parsing a future algorithm.
const signatureVersion = "v1"

// Sign returns the HeaderSignature value for a payload: "v1=<hex>" where hex
// is HMAC-SHA256(secret, "<ts>.<body>"). ts is the unix-seconds string that
// must also be sent as HeaderTimestamp.
func Sign(secret string, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	return signatureVersion + "=" + hex.EncodeToString(mac.Sum(nil))
}

// Verify checks a received signature against the secret, timestamp, and body
// in constant time. It is the receiver-side counterpart to Sign, exported so
// webhook consumers (and the 15.5 tests) validate deliveries the same way the
// sender produced them. It does not enforce a timestamp tolerance — that is
// the receiver's replay policy; VerifyWithin adds it.
func Verify(secret, ts string, body []byte, signature string) bool {
	want := Sign(secret, ts, body)
	return hmac.Equal([]byte(want), []byte(signature))
}

// VerifyWithin is Verify plus a replay guard: it additionally rejects a
// timestamp that is not parseable or is further than tolerance from now. A
// receiver uses it to drop a captured-and-replayed delivery.
func VerifyWithin(secret, ts string, body []byte, signature string, now time.Time, tolerance time.Duration) bool {
	secs, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	delta := now.Sub(time.Unix(secs, 0))
	if delta < 0 {
		delta = -delta
	}
	if delta > tolerance {
		return false
	}
	return Verify(secret, ts, body, signature)
}
