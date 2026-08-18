package notify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Webhook defaults (ticket 15.5). Retries are synchronous and capped: the
// delivery runs in the engine's park path, before the ACK, so the whole
// retry budget must stay well inside the queue lease TTL. Three attempts with
// a 1s→2s backoff is worst-case ~3s of waiting on a dead endpoint — bounded,
// and only when the webhook is broken.
const (
	DefaultTimeout     = 5 * time.Second
	DefaultMaxAttempts = 3
	DefaultBackoff     = 1 * time.Second
	DefaultMaxBackoff  = 30 * time.Second
	// maxResponseBytes caps how much of a webhook response body we drain
	// before closing — enough to reuse the connection, nothing retained.
	maxResponseBytes = 4 << 10
)

// WebhookConfig configures a Webhook notifier.
type WebhookConfig struct {
	// URL is the endpoint each notification is POSTed to. Required; must be
	// http or https.
	URL string
	// Secret keys the HMAC signature. Required — an unsigned webhook is a
	// footgun (a receiver could not authenticate the sender).
	Secret string
	// Timeout bounds a single delivery attempt. Zero uses DefaultTimeout.
	Timeout time.Duration
	// MaxAttempts is the total number of attempts (>=1). Zero uses
	// DefaultMaxAttempts.
	MaxAttempts int
	// Backoff is the base delay between attempts; attempt k waits
	// Backoff*2^(k-1), capped at MaxBackoff. Zero uses DefaultBackoff.
	Backoff time.Duration
	// MaxBackoff caps the exponential backoff. Zero uses DefaultMaxBackoff.
	MaxBackoff time.Duration
	// Client is the HTTP client (injectable for tests). Nil uses a client with
	// the per-attempt timeout applied via context, not Client.Timeout.
	Client *http.Client
	// Now is the injected clock stamped into the signature timestamp. Nil uses
	// time.Now.
	Now func() time.Time
	// Sleep waits d or returns ctx.Err() if the context is cancelled first
	// (injectable so tests advance instantly). Nil uses a real timer.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Webhook is the built-in Notifier: it POSTs a signed ApprovalNotification to
// a configured URL, retrying transient failures with capped exponential
// backoff.
type Webhook struct {
	url         string
	host        string
	secret      string
	timeout     time.Duration
	maxAttempts int
	backoff     time.Duration
	maxBackoff  time.Duration
	client      *http.Client
	now         func() time.Time
	sleep       func(ctx context.Context, d time.Duration) error
}

var _ Notifier = (*Webhook)(nil)

// NewWebhook validates cfg and builds a Webhook. It fails when the URL is
// missing/unparseable/not http(s) or the secret is empty — so a
// misconfigured webhook is a boot error, never a silent no-op.
func NewWebhook(cfg WebhookConfig) (*Webhook, error) {
	if cfg.URL == "" {
		return nil, errors.New("notify: webhook URL is required")
	}
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("notify: parsing webhook URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("notify: webhook URL scheme %q must be http or https", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("notify: webhook URL has no host")
	}
	if cfg.Secret == "" {
		return nil, errors.New("notify: webhook secret is required (an unsigned webhook cannot be authenticated)")
	}
	w := &Webhook{
		url:         cfg.URL,
		host:        u.Host,
		secret:      cfg.Secret,
		timeout:     cfg.Timeout,
		maxAttempts: cfg.MaxAttempts,
		backoff:     cfg.Backoff,
		maxBackoff:  cfg.MaxBackoff,
		client:      cfg.Client,
		now:         cfg.Now,
		sleep:       cfg.Sleep,
	}
	if w.timeout <= 0 {
		w.timeout = DefaultTimeout
	}
	if w.maxAttempts <= 0 {
		w.maxAttempts = DefaultMaxAttempts
	}
	if w.backoff <= 0 {
		w.backoff = DefaultBackoff
	}
	if w.maxBackoff <= 0 {
		w.maxBackoff = DefaultMaxBackoff
	}
	if w.client == nil {
		w.client = &http.Client{}
	}
	if w.now == nil {
		w.now = time.Now
	}
	if w.sleep == nil {
		w.sleep = realSleep
	}
	return w, nil
}

// Host is the webhook's host — the only part of the URL safe to log or record
// on an event (the path or query could carry a token).
func (w *Webhook) Host() string { return w.host }

// Notify POSTs the notification, retrying transient failures. The body and
// signature timestamp are fixed on the first attempt and reused across
// retries, so every attempt carries the same delivery id and a valid
// signature — a receiver deduping on the id sees at most one distinct
// delivery. Returns the caller's context error unwrapped when the context is
// cancelled (the engine's convention for its executors).
func (w *Webhook) Notify(ctx context.Context, n ApprovalNotification) (Result, error) {
	body, err := marshalCanonical(n)
	if err != nil {
		// A payload the engine built cannot be marshalled is a bug, not a
		// transient condition — permanent.
		return Result{}, &Error{Permanent: true, Op: "encode", Attempts: 0}
	}
	ts := formatUnix(w.now())
	sig := Sign(w.secret, ts, body)

	var last Error
	for attempt := 1; attempt <= w.maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return Result{}, err // caller cancelled — unwrapped
		}
		status, perr := w.post(ctx, body, ts, sig, n)
		if perr == nil {
			return Result{Attempts: attempt, StatusCode: status}, nil
		}
		// Distinguish a caller cancellation from a self-imposed per-attempt
		// timeout: the former is fatal and unwrapped, the latter is transient.
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		last = classify(status, attempt)
		if last.Permanent || attempt == w.maxAttempts {
			return Result{}, &last
		}
		if serr := w.sleep(ctx, w.attemptBackoff(attempt)); serr != nil {
			return Result{}, serr // context cancelled during backoff
		}
	}
	return Result{}, &last
}

// post makes one delivery attempt under a per-attempt timeout and returns the
// HTTP status (0 if the call never completed) and a non-nil error on failure.
func (w *Webhook) post(ctx context.Context, body []byte, ts, sig string, n ApprovalNotification) (int, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderTimestamp, ts)
	req.Header.Set(HeaderSignature, sig)
	req.Header.Set(HeaderDeliveryID, n.DeliveryID())
	req.Header.Set(HeaderEvent, n.Event)
	resp, err := w.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain a bounded prefix so the connection can be reused; discard it.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, fmt.Errorf("webhook returned status %d", resp.StatusCode)
}

// attemptBackoff is Backoff*2^(attempt-1), capped at MaxBackoff.
func (w *Webhook) attemptBackoff(attempt int) time.Duration {
	d := w.backoff
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= w.maxBackoff {
			return w.maxBackoff
		}
	}
	if d > w.maxBackoff {
		return w.maxBackoff
	}
	return d
}

// classify decides whether a failed attempt is permanent. A 4xx other than
// 408 (Request Timeout) and 429 (Too Many Requests) is permanent — retrying
// a bad request or an auth failure is futile. Everything else (5xx, transport
// errors, the two retryable 4xx) is transient.
func classify(status, attempt int) Error {
	e := Error{Attempts: attempt, StatusCode: status, Op: "deliver"}
	if status >= 400 && status < 500 && status != http.StatusRequestTimeout && status != http.StatusTooManyRequests {
		e.Permanent = true
	}
	return e
}

func realSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
