package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mathcslearner/agentloom/internal/plugin"
)

// httpRequestName is the built-in's registered name and step-config tool
// value.
const httpRequestName = "http_request"

// httpRequestVersion is the tool's plugin version (ADR-009): 1.0.0, the
// real semantics replacing the 8.7-era `tool` dev stub. side_effectful is
// the flag the stub already carried, so no cache/journal behavior changes
// across the swap — only the version bump busts any M9 cache entry keyed
// on the stub's echo output.
const httpRequestVersion = "1.0.0"

const (
	// defaultHTTPTimeout bounds a request when the args omit `timeout` and
	// the constructor set no default.
	defaultHTTPTimeout = 30 * time.Second
	// defaultMaxResponseBytes caps a response body when the constructor set
	// no limit (1 MiB).
	defaultMaxResponseBytes int64 = 1 << 20
	// idempotencyHeader is the header http_request stamps with the step's
	// stable idempotency key on non-GET requests (ticket 5.5).
	idempotencyHeader = "Idempotency-Key"
)

// httpArgs is http_request's argument struct; its generated JSON Schema is
// the tool's validation contract. Only `url` is required. Body and Headers
// are arbitrary JSON / string map; a json.RawMessage body reflects to the
// permissive `true` schema so any JSON body is accepted.
type httpArgs struct {
	// Method is the HTTP method; default GET.
	Method string `json:"method,omitempty"`
	// URL is the absolute request URL; required. Its host is checked
	// against the allowlist before any connection is made.
	URL string `json:"url"`
	// Headers are extra request headers.
	Headers map[string]string `json:"headers,omitempty"`
	// Body is the request body, sent as compact JSON with a default
	// Content-Type of application/json. Absent means no body.
	Body json.RawMessage `json:"body,omitempty"`
	// Timeout is a per-request Go duration string ("5s"); overrides the
	// tool's configured default when set.
	Timeout string `json:"timeout,omitempty"`
}

// HTTPOptions configures the http_request tool. Allowlist is the set of
// hosts the tool may reach — the SSRF guard; an empty allowlist denies
// every host (a deployment opts hosts in). Client is injectable for tests.
type HTTPOptions struct {
	// Allowlist is the set of permitted hosts. Each entry is a hostname,
	// optionally with a port ("example.com" or "example.com:8443"); a bare
	// hostname permits any port. Matching is case-insensitive. Empty =
	// deny all.
	Allowlist []string
	// DefaultTimeout bounds a request whose args omit `timeout`; zero uses
	// defaultHTTPTimeout.
	DefaultTimeout time.Duration
	// MaxResponseBytes caps the response body read; zero uses
	// defaultMaxResponseBytes. A larger body is a permanent failure (the
	// step cannot safely truncate an unknown payload).
	MaxResponseBytes int64
	// Client is the HTTP client used for requests; nil builds a default
	// client with redirect re-validation. Tests inject a client with a
	// tripwire or recording transport.
	Client *http.Client
}

// HTTPRequest is the built-in http_request tool: one outbound HTTP call
// per Invoke, guarded by a host allowlist (SSRF), bounded by a timeout and
// a response-size cap, and automatically stamped with an Idempotency-Key
// on non-GET requests so a retried call is deduplicated server-side.
type HTTPRequest struct {
	allow    hostSet
	timeout  time.Duration
	maxBytes int64
	client   *http.Client
}

// NewHTTPRequest builds the tool from its options. It wires the client's
// CheckRedirect to re-validate every redirect hop against the allowlist,
// closing the redirect-to-forbidden-host bypass.
func NewHTTPRequest(opts HTTPOptions) *HTTPRequest {
	t := &HTTPRequest{
		allow:    newHostSet(opts.Allowlist),
		timeout:  opts.DefaultTimeout,
		maxBytes: opts.MaxResponseBytes,
	}
	if t.timeout <= 0 {
		t.timeout = defaultHTTPTimeout
	}
	if t.maxBytes <= 0 {
		t.maxBytes = defaultMaxResponseBytes
	}
	t.client = opts.Client
	if t.client == nil {
		t.client = &http.Client{}
	}
	// Re-validate redirects against the same allowlist. A nil-return policy
	// means "follow"; returning an error stops the redirect and surfaces
	// below. We clone the client so injecting CheckRedirect never mutates a
	// caller-shared client.
	client := *t.client
	client.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		if host, ok := t.allow.permit(req.URL); !ok {
			return &HostNotAllowedError{Host: host}
		}
		return nil
	}
	t.client = &client
	return t
}

// Manifest implements Tool (ADR-009): side_effectful (a real HTTP call
// acts on the world).
func (t *HTTPRequest) Manifest() plugin.Manifest {
	schema, err := argsSchema(&httpArgs{})
	if err != nil {
		panic(err) // unreachable: a fixed, reflectable struct
	}
	return plugin.Manifest{
		Kind:         plugin.KindTool,
		Name:         httpRequestName,
		Version:      httpRequestVersion,
		Description:  "Make one outbound HTTP request to an allowlisted host.",
		Capabilities: plugin.Capabilities{SideEffectful: true},
		ConfigSchema: schema,
	}
}

// httpResult is http_request's output shape, read downstream through
// templating (`${{ steps.fetch.output.status }}`) and CEL. Body is the
// response body as parsed JSON when the response is JSON and parses, else
// the raw body as a JSON string.
type httpResult struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

// Invoke performs the request. Outcome mapping (ADR-006): a blocked host
// is a permanent *HostNotAllowedError and no request is sent; transport
// failures and 429/5xx are transient; other non-2xx are permanent; a 2xx
// is success. A per-request timeout that trips while the parent ctx is
// still alive maps to a transient failure; ctx cancellation/deadline pass
// through unwrapped for the engine to judge.
func (t *HTTPRequest) Invoke(ctx context.Context, inv Invocation) (json.RawMessage, error) {
	var args httpArgs
	if err := strictUnmarshal(inv.Args, &args); err != nil {
		return nil, permanentf(httpRequestName, "decoding args: %v", err)
	}
	if args.URL == "" {
		return nil, permanentf(httpRequestName, "%q is required", "url")
	}
	method := strings.ToUpper(strings.TrimSpace(args.Method))
	if method == "" {
		method = http.MethodGet
	}

	u, err := url.Parse(args.URL)
	if err != nil {
		return nil, permanentf(httpRequestName, "invalid url")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, permanentf(httpRequestName, "unsupported url scheme %q", u.Scheme)
	}
	// SSRF guard: check the host BEFORE any connection. A block is
	// permanent and the request is provably never sent.
	if host, ok := t.allow.permit(u); !ok {
		return nil, &HostNotAllowedError{Host: host}
	}

	timeout := t.timeout
	if args.Timeout != "" {
		d, perr := time.ParseDuration(args.Timeout)
		if perr != nil || d <= 0 {
			return nil, permanentf(httpRequestName, "invalid timeout %q", args.Timeout)
		}
		timeout = d
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var body io.Reader
	if len(bytes.TrimSpace(args.Body)) != 0 {
		compact := &bytes.Buffer{}
		if err := json.Compact(compact, args.Body); err != nil {
			return nil, permanentf(httpRequestName, "invalid body json")
		}
		body = compact
	}

	req, err := http.NewRequestWithContext(reqCtx, method, u.String(), body)
	if err != nil {
		return nil, permanentf(httpRequestName, "building request: %v", err)
	}
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range args.Headers {
		req.Header.Set(k, v)
	}
	// Automatic idempotency key on non-GET requests (ticket 5.5). It wins
	// over any user-supplied header: the stable key across attempts is the
	// exactly-once guarantee, and letting rendered args override it would
	// defeat that. GET is safe/idempotent by definition, so it carries none.
	if method != http.MethodGet && inv.IdempotencyKey != "" {
		req.Header.Set(idempotencyHeader, inv.IdempotencyKey)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, t.doError(ctx, reqCtx, method, u.Host, err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-side close; nothing left to read

	raw, truncated, rerr := readAllLimited(resp.Body, t.maxBytes)
	if rerr != nil {
		return nil, transientf(httpRequestName, "reading response body from %s: %v", u.Host, rerr)
	}
	if truncated {
		return nil, permanentf(httpRequestName, "response body from %s exceeds %d bytes", u.Host, t.maxBytes)
	}

	inv.logger().InfoContext(ctx, "http_request completed",
		slog.String("method", method),
		slog.String("host", u.Host),
		slog.Int("status", resp.StatusCode))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 429/5xx heal on their own (transient); other non-2xx are the
		// request's own fault (permanent) — the ADR-006 HTTP taxonomy.
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			return nil, transientf(httpRequestName, "%s %s returned HTTP %d", method, u.Host, resp.StatusCode)
		}
		return nil, permanentf(httpRequestName, "%s %s returned HTTP %d", method, u.Host, resp.StatusCode)
	}

	return marshalHTTPResult(resp, raw)
}

// doError classifies a client.Do failure. A ctx error from the parent
// passes through unwrapped (engine judges timeout/cancelled); a per-request
// timeout (parent still alive) is transient; a blocked redirect surfaces as
// the permanent *HostNotAllowedError; anything else is a transient transport
// failure.
func (t *HTTPRequest) doError(parent, reqCtx context.Context, method, host string, err error) error {
	if parent.Err() != nil {
		// Cancellation/deadline from the engine's own context — keep it
		// identity-preserving so errors.Is holds upstream.
		return parent.Err()
	}
	var hostErr *HostNotAllowedError
	if errors.As(err, &hostErr) {
		return hostErr
	}
	if reqCtx.Err() != nil {
		// The per-request timeout tripped while the engine's ctx is alive —
		// a transient failure the retry may clear (ADR-006 timeout-as-transient
		// for a self-imposed tool deadline).
		return transientf(httpRequestName, "%s %s timed out", method, host)
	}
	return transientf(httpRequestName, "%s %s: %v", method, host, err)
}

// marshalHTTPResult renders a 2xx response into the output shape: a JSON
// body is embedded as JSON, a non-JSON body as a JSON string.
func marshalHTTPResult(resp *http.Response, raw []byte) (json.RawMessage, error) {
	var bodyJSON json.RawMessage
	if json.Valid(raw) && len(bytes.TrimSpace(raw)) > 0 {
		bodyJSON = json.RawMessage(raw)
	} else {
		encoded, err := json.Marshal(string(raw))
		if err != nil {
			return nil, permanentf(httpRequestName, "encoding response body: %v", err)
		}
		bodyJSON = encoded
	}
	data, err := json.Marshal(httpResult{
		Status: resp.StatusCode,
		Body:   bodyJSON,
	})
	if err != nil {
		return nil, permanentf(httpRequestName, "marshaling result: %v", err)
	}
	return data, nil
}
