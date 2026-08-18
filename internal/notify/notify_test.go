package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func sampleNotification() ApprovalNotification {
	return ApprovalNotification{
		SchemaVersion: SchemaVersion,
		Event:         EventApprovalRequested,
		Approval: ApprovalInfo{
			ID: "a1b2", RunID: "r1", StepID: "gate", Attempt: 1,
			Title: "Publish?", AllowedDecisions: []string{"approve", "reject"},
			Payload:   json.RawMessage(`{"text":"hello"}`),
			CreatedAt: time.Unix(1700000000, 0).UTC(),
		},
		Run:   RunInfo{ID: "r1", DefinitionName: "demo"},
		Links: Links{Approval: "/v1/approvals/a1b2", Decide: "/v1/approvals/a1b2:decide", Run: "/v1/runs/r1"},
	}
}

func fixedNow() time.Time { return time.Unix(1700000000, 0).UTC() }

func instantSleep(context.Context, time.Duration) error { return nil }

func TestSignVerify(t *testing.T) {
	secret := "shh"
	ts := "1700000000"
	body := []byte(`{"a":1}`)
	sig := Sign(secret, ts, body)
	if !Verify(secret, ts, body, sig) {
		t.Fatal("Verify rejected a valid signature")
	}
	// Tamper each input.
	if Verify("other", ts, body, sig) {
		t.Error("Verify accepted a wrong secret")
	}
	if Verify(secret, "1700000001", body, sig) {
		t.Error("Verify accepted a wrong timestamp")
	}
	if Verify(secret, ts, []byte(`{"a":2}`), sig) {
		t.Error("Verify accepted a tampered body")
	}
	if Verify(secret, ts, body, "v1=deadbeef") {
		t.Error("Verify accepted a bad signature")
	}
}

func TestVerifyWithin(t *testing.T) {
	secret, body := "shh", []byte(`{}`)
	now := time.Unix(1700000000, 0)
	ts := "1700000000"
	sig := Sign(secret, ts, body)
	if !VerifyWithin(secret, ts, body, sig, now.Add(2*time.Minute), 5*time.Minute) {
		t.Error("VerifyWithin rejected a within-tolerance delivery")
	}
	if VerifyWithin(secret, ts, body, sig, now.Add(10*time.Minute), 5*time.Minute) {
		t.Error("VerifyWithin accepted a replay past tolerance")
	}
	if VerifyWithin(secret, "not-a-number", body, sig, now, time.Minute) {
		t.Error("VerifyWithin accepted an unparseable timestamp")
	}
}

// recorder captures each request's headers/body for assertion.
type capture struct {
	mu    sync.Mutex
	calls []captured
}
type captured struct {
	sig, ts, deliveryID, event string
	body                       []byte
}

func (c *capture) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, captured{
		sig: r.Header.Get(HeaderSignature), ts: r.Header.Get(HeaderTimestamp),
		deliveryID: r.Header.Get(HeaderDeliveryID), event: r.Header.Get(HeaderEvent),
		body: body,
	})
}
func (c *capture) count() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.calls) }

func TestWebhookDeliversWithSignedHeaders(t *testing.T) {
	recv := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recv.record(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	secret := "topsecret"
	wh, err := NewWebhook(WebhookConfig{URL: srv.URL, Secret: secret, Now: fixedNow, Sleep: instantSleep})
	if err != nil {
		t.Fatalf("NewWebhook: %v", err)
	}
	n := sampleNotification()
	res, err := wh.Notify(context.Background(), n)
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if res.Attempts != 1 || res.StatusCode != 200 {
		t.Errorf("Result = %+v, want {1, 200}", res)
	}
	if recv.count() != 1 {
		t.Fatalf("receiver hit %d times, want 1", recv.count())
	}
	got := recv.calls[0]
	if got.deliveryID != n.DeliveryID() {
		t.Errorf("delivery id = %q, want %q", got.deliveryID, n.DeliveryID())
	}
	if got.event != EventApprovalRequested {
		t.Errorf("event header = %q", got.event)
	}
	if !Verify(secret, got.ts, got.body, got.sig) {
		t.Error("received signature does not verify against the received body")
	}
}

func TestWebhookRetriesTransientThenSucceeds(t *testing.T) {
	recv := &capture{}
	statuses := []int{500, 500, 200}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recv.record(r)
		i := recv.count() - 1
		w.WriteHeader(statuses[i])
	}))
	defer srv.Close()

	secret := "s"
	wh, _ := NewWebhook(WebhookConfig{URL: srv.URL, Secret: secret, MaxAttempts: 3, Now: fixedNow, Sleep: instantSleep})
	res, err := wh.Notify(context.Background(), sampleNotification())
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if res.Attempts != 3 || res.StatusCode != 200 {
		t.Errorf("Result = %+v, want {3, 200}", res)
	}
	// Every attempt carried the same body, timestamp, and signature — a
	// receiver deduping on the delivery id sees one logical delivery.
	first := recv.calls[0]
	for i, c := range recv.calls {
		if c.ts != first.ts || c.sig != first.sig || string(c.body) != string(first.body) || c.deliveryID != first.deliveryID {
			t.Errorf("attempt %d differs from the first (body/ts/sig/id must be stable across retries)", i)
		}
		if !Verify(secret, c.ts, c.body, c.sig) {
			t.Errorf("attempt %d signature invalid", i)
		}
	}
}

func TestWebhookPermanent4xx(t *testing.T) {
	recv := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recv.record(r)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	wh, _ := NewWebhook(WebhookConfig{URL: srv.URL, Secret: "s", MaxAttempts: 3, Now: fixedNow, Sleep: instantSleep})
	_, err := wh.Notify(context.Background(), sampleNotification())
	var ne *Error
	if !errors.As(err, &ne) {
		t.Fatalf("error = %v, want *notify.Error", err)
	}
	if !ne.Permanent || ne.Attempts != 1 || ne.StatusCode != 400 {
		t.Errorf("Error = %+v, want permanent after 1 attempt, status 400", ne)
	}
	if recv.count() != 1 {
		t.Errorf("receiver hit %d times, want 1 (no retry on 4xx)", recv.count())
	}
}

func TestWebhookRetriesExhausted(t *testing.T) {
	recv := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recv.record(r)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	wh, _ := NewWebhook(WebhookConfig{URL: srv.URL, Secret: "s", MaxAttempts: 2, Now: fixedNow, Sleep: instantSleep})
	_, err := wh.Notify(context.Background(), sampleNotification())
	var ne *Error
	if !errors.As(err, &ne) {
		t.Fatalf("error = %v, want *notify.Error", err)
	}
	if ne.Permanent || ne.Attempts != 2 {
		t.Errorf("Error = %+v, want transient after 2 attempts", ne)
	}
	if recv.count() != 2 {
		t.Errorf("receiver hit %d times, want 2", recv.count())
	}
}

func TestWebhookContextCancelUnwrapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	wh, _ := NewWebhook(WebhookConfig{URL: srv.URL, Secret: "s", Now: fixedNow, Sleep: instantSleep})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := wh.Notify(ctx, sampleNotification())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled unwrapped", err)
	}
	var ne *Error
	if errors.As(err, &ne) {
		t.Error("cancellation must not be wrapped in *notify.Error")
	}
}

func TestNewWebhookValidation(t *testing.T) {
	cases := []struct {
		name string
		cfg  WebhookConfig
	}{
		{"empty url", WebhookConfig{Secret: "s"}},
		{"bad scheme", WebhookConfig{URL: "ftp://x/y", Secret: "s"}},
		{"no host", WebhookConfig{URL: "http://", Secret: "s"}},
		{"empty secret", WebhookConfig{URL: "http://example.com/hook"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewWebhook(tc.cfg); err == nil {
				t.Errorf("NewWebhook(%+v) = nil error, want an error", tc.cfg)
			}
		})
	}
	wh, err := NewWebhook(WebhookConfig{URL: "https://example.com:8443/hook?t=x", Secret: "s"})
	if err != nil {
		t.Fatalf("NewWebhook valid: %v", err)
	}
	if wh.Host() != "example.com:8443" {
		t.Errorf("Host() = %q, want the host only (no path/query — secret hygiene)", wh.Host())
	}
}
