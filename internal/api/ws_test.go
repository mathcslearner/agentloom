package api

// Unit coverage for the run-WebSocket ticket (ticket 16.3, ADR-018) and the WS
// frame encodings. The connection driver and the full protocol are covered by
// ws_integration_test.go against a real store + engine.

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

const wsTestSecret = "test-ws-secret-not-a-real-value"

func TestWSTicketRoundTrip(t *testing.T) {
	t.Parallel()
	run := uuid.New()
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

	ticket, exp, err := mintWSTicket(wsTestSecret, run, "key-123", now, time.Minute)
	if err != nil {
		t.Fatalf("mintWSTicket: %v", err)
	}
	if !exp.Equal(now.Add(time.Minute).Truncate(time.Second)) {
		t.Errorf("expiry = %v, want %v", exp, now.Add(time.Minute).Truncate(time.Second))
	}
	claims, err := verifyWSTicket(wsTestSecret, ticket, run, now.Add(30*time.Second))
	if err != nil {
		t.Fatalf("verifyWSTicket: %v", err)
	}
	if claims.RunID != run.String() {
		t.Errorf("run id = %q, want %q", claims.RunID, run)
	}
	if claims.KeyID != "key-123" {
		t.Errorf("key id = %q, want key-123", claims.KeyID)
	}
}

func TestWSTicketRejections(t *testing.T) {
	t.Parallel()
	run := uuid.New()
	other := uuid.New()
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	good, _, err := mintWSTicket(wsTestSecret, run, "k", now, time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	cases := []struct {
		name   string
		secret string
		ticket string
		run    uuid.UUID
		at     time.Time
	}{
		{"expired", wsTestSecret, good, run, now.Add(2 * time.Minute)},
		{"exactly at expiry", wsTestSecret, good, run, now.Add(time.Minute)},
		{"wrong run", wsTestSecret, good, other, now.Add(time.Second)},
		{"wrong secret", "other-secret", good, run, now.Add(time.Second)},
		{"tampered payload", wsTestSecret, "x" + good, run, now.Add(time.Second)},
		{"no dot", wsTestSecret, "notaticket", run, now.Add(time.Second)},
		{"empty", wsTestSecret, "", run, now.Add(time.Second)},
		{"only sig", wsTestSecret, ".abc", run, now.Add(time.Second)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := verifyWSTicket(tc.secret, tc.ticket, tc.run, tc.at); err == nil {
				t.Errorf("verifyWSTicket accepted %s ticket, want rejection", tc.name)
			}
		})
	}
}

func TestWSTicketTamperedSignatureRejected(t *testing.T) {
	t.Parallel()
	run := uuid.New()
	now := time.Now()
	good, _, err := mintWSTicket(wsTestSecret, run, "k", now, time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// Flip the last character of the signature.
	tampered := good[:len(good)-1]
	if good[len(good)-1] == 'A' {
		tampered += "B"
	} else {
		tampered += "A"
	}
	if _, err := verifyWSTicket(wsTestSecret, tampered, run, now); err == nil {
		t.Error("verifyWSTicket accepted a tampered signature")
	}
}

func TestWSTicketVersionMismatchRejected(t *testing.T) {
	t.Parallel()
	// A hand-forged payload with a bad version but a valid signature (a client
	// that reuses the secret must still be rejected on a version bump).
	run := uuid.New()
	claims := wsTicketClaims{Version: 999, RunID: run.String(), KeyID: "k", Expires: time.Now().Add(time.Hour).Unix()}
	payload, _ := json.Marshal(claims)
	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	ticket := payloadB64 + "." + ticketSignature(wsTestSecret, payloadB64)
	if _, err := verifyWSTicket(wsTestSecret, ticket, run, time.Now()); err == nil {
		t.Error("verifyWSTicket accepted a wrong-version ticket")
	}
}

func TestWSFrameEncodings(t *testing.T) {
	t.Parallel()
	// Each frame carries its discriminator so the client can switch on "type".
	for _, tc := range []struct {
		v    any
		want string
	}{
		{WSCaughtUpFrame{Type: WSFrameCaughtUp, LastSeq: 7}, `"type":"caught_up"`},
		{WSErrorFrame{Type: WSFrameError, Code: "internal", Message: "x"}, `"type":"error"`},
	} {
		b, err := json.Marshal(tc.v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !strings.Contains(string(b), tc.want) {
			t.Errorf("frame %T = %s, want it to contain %s", tc.v, b, tc.want)
		}
	}
}

func TestParseLastSeq(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"", 0, false},
		{"0", 0, false},
		{"42", 42, false},
		{"-1", 0, true},
		{"abc", 0, true},
	} {
		got, err := parseLastSeq(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseLastSeq(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
		}
		if err == nil && got != tc.want {
			t.Errorf("parseLastSeq(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
