// Package api implements agentloom's HTTP ingest surface (tickets 4.6,
// 6.1, 6.2, 6.4): run submission, run inspection, key management, and the
// health probe. Durable state lives only in Postgres: submission writes
// the run and its entry-step outbox rows in CreateRun's single
// transaction, and the worker fleet's dispatchers drain the outbox to
// Redis (ADR-002 — no central scheduler, so the API never dispatches).
// Since ticket 6.4 the API does hold a Redis client, but solely for
// rate-limit token buckets (internal/ratelimit) — never for dispatch —
// and it fails open when Redis is unreachable, so Postgres remains the
// only hard dependency.
//
// Routes (scope per ADR-007's route→scope table, rate-limit route class
// per its route→class table):
//
//	POST   /v1/runs                                 submit  submit  submit a run (inline definition or stored ref; Idempotency-Key header honored)
//	GET    /v1/runs                                 read    read    list runs (keyset pagination + status/definition/time filters)
//	GET    /v1/runs/{id}                            read    read    run status + step tree + attempt history + DLQ records
//	POST   /v1/runs/{id}/cancel                     submit  submit  cancel a run (cooperative; converges via workers/reconciler)
//	POST   /v1/runs/{id}/park                       submit  submit  pause dispatch
//	POST   /v1/runs/{id}/unpark                     submit  submit  resume dispatch (re-outboxes stranded ready steps)
//	POST   /v1/runs/{id}/steps/{sid}/requeue        submit  submit  requeue a dead-lettered step (budget re-armed)
//	GET    /v1/runs/{id}/steps/{sid}/logs           read    read    one attempt's captured executor logs (keyset pages)
//	POST   /v1/definitions                          submit  submit  register a definition (version 1; name must be new)
//	GET    /v1/definitions                          read    read    list definitions (latest version per name, keyset by name)
//	GET    /v1/definitions/{id}                     read    read    one stored definition, spec included
//	GET    /v1/definitions/{name}/versions          read    read    every version of one name
//	POST   /v1/definitions/{name}/versions          submit  submit  append the next version of an existing name
//	GET    /v1/plugins                              read    read    plugin catalog: manifests + config schemas (ticket 8.1)
//	POST   /v1/keys                                 admin   admin   mint an API key (plaintext shown once)
//	GET    /v1/keys                                 admin   admin   list API keys (prefixes only)
//	DELETE /v1/keys/{id}                            admin   admin   revoke an API key (soft, idempotent)
//	GET    /healthz                                 exempt  exempt  liveness (Postgres ping)
//
// Auth (tickets 6.1/6.2, ADR-007): every /v1 route requires a bearer API
// key carrying the route's scope — a stored key, or the env-provided
// root key (implicit admin) that bootstraps the first one. Credential
// failures are a uniform 401; a valid key lacking the scope is a 403
// naming the missing scope. /healthz stays exempt so probes need no
// secret. Requests to paths outside the route table (404/405) answer
// anonymously — chi's fallback handlers run outside the /v1 middleware
// tree, and route existence is public knowledge (the spec), so this
// leaks nothing.
//
// Rate limiting (ticket 6.4, ADR-007): after auth, every /v1 route
// acquires from the caller's per-key_id token bucket for the route's
// class and from the global safety bucket. A denial is 429 with
// Retry-After and X-RateLimit-Limit/-Remaining/-Reset headers; see
// ratelimit.go for the decisions (fail-open, per-key-before-global,
// derived Reset).
//
// Every error response carries the JSON envelope {"error": {code, message,
// issues?}}; definition problems surface M1's path-qualified issues
// verbatim. The formal contract is api/openapi.yaml (ticket 6.6) —
// TestOpenAPIRouteCoverage keeps it and this router in lockstep, so a new
// route lands in the spec in the same change. Request and response types
// live in types.go (cmd/ctl imports them); their spec schemas are
// maintained by hand, so change both together.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/go-chi/chi/v5"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	obstrace "github.com/mathcslearner/agentloom/internal/obs/trace"
	"github.com/mathcslearner/agentloom/internal/store"
)

// MaxBodyBytes caps request bodies. ADR-003's definition limits are far
// below this; the cap only exists so a hostile body cannot exhaust memory.
const MaxBodyBytes = 1 << 20

// Handler is the API's http.Handler: a chi router over the store.
type Handler struct {
	router chi.Router
	st     *store.Store
	// ctl is the run-lifecycle control surface (ticket 6.5): cancel, park,
	// unpark, requeue. Built by New over the same store and clock, with no
	// dispatcher nudge — the ops' outbox rows are drained on the worker
	// fleet's dispatch cadence (ADR-002: the API never talks to Redis for
	// dispatch).
	ctl    *engine.Control
	now    func() time.Time
	logger *slog.Logger
	// rootHash is the hex SHA-256 of the env-provided root key (ADR-007
	// admin bootstrap); empty means no root credential exists.
	rootHash string
	// keyRand feeds key generation; production uses crypto/rand, tests
	// may inject a deterministic reader.
	keyRand io.Reader
	// rl configures per-client rate limiting (ticket 6.4); a nil
	// rl.Acquirer disables it. rl.Metrics is never nil after New.
	rl RateLimitOptions
	// metrics receives per-request observations (ticket 7.2). Never nil
	// after New — the default is the no-op recorder.
	metrics RequestMetrics
	// plugins is the precomputed GET /v1/plugins listing (ticket 8.1),
	// set by WithPlugins; nil serves as an empty catalog.
	plugins []PluginInfo
}

// RequestMetrics is the API's per-request observability seam (ticket 7.2,
// ADR-008): cmd/api wires internal/obs/metrics.APIMetrics here, which
// satisfies this interface structurally. Implementations must be safe for
// concurrent use and must not block — they sit on the request hot path.
type RequestMetrics interface {
	// Request records one served request. route is the chi route pattern
	// (or "unmatched"), never the raw path; method is clamped to the
	// routed vocabulary.
	Request(route, method string, status int, d time.Duration)
	// RequestStarted and RequestFinished bracket every request (ticket
	// 7.5): the gauge behind them is the API dashboard's in-flight panel.
	// They carry no labels — route is only known after routing, and an
	// in-flight gauge keyed on anything request-derived would leak.
	RequestStarted()
	RequestFinished()
}

// nopRequestMetrics is the default RequestMetrics.
type nopRequestMetrics struct{}

func (nopRequestMetrics) Request(string, string, int, time.Duration) {}
func (nopRequestMetrics) RequestStarted()                            {}
func (nopRequestMetrics) RequestFinished()                           {}

// Option customizes a Handler beyond New's required arguments.
type Option func(*Handler)

// WithRequestMetrics sets the per-request metrics recorder (ticket 7.2).
// Nil restores the no-op default.
func WithRequestMetrics(m RequestMetrics) Option {
	return func(h *Handler) {
		if m == nil {
			m = nopRequestMetrics{}
		}
		h.metrics = m
	}
}

// New builds the Handler. now is the injected clock (project invariant —
// cmd/api passes time.Now); logger is the base request logger (nil means
// slog.Default()); rootKey is the optional bootstrap admin credential
// (ADR-007) — empty disables the root path, and a set-but-malformed key
// is a configuration error caught here rather than a silent 401 later;
// rl configures per-client rate limiting (ticket 6.4) — the zero value
// disables it; opts carry the optional extras (request metrics, ticket
// 7.2).
func New(st *store.Store, now func() time.Time, logger *slog.Logger, rootKey string, rl RateLimitOptions, opts ...Option) (*Handler, error) {
	if st == nil {
		return nil, errors.New("api: nil store")
	}
	if now == nil {
		return nil, errors.New("api: nil clock — pass the injected time source")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if err := rl.validate(); err != nil {
		return nil, err
	}
	if rl.Metrics == nil {
		rl.Metrics = nopRateLimitMetrics{}
	}
	ctl, err := engine.NewControl(st, engine.WithControlClock(now))
	if err != nil {
		return nil, err
	}
	h := &Handler{st: st, ctl: ctl, now: now, logger: logger, keyRand: cryptoRand, rl: rl, metrics: nopRequestMetrics{}}
	for _, opt := range opts {
		opt(h)
	}
	if rootKey != "" {
		if !keyShapeOK(rootKey) {
			// Deliberately no detail: the message must never echo the value.
			return nil, errors.New("api: root key does not have the sk_ key shape")
		}
		h.rootHash = hashKey(rootKey)
	}

	r := chi.NewRouter()
	r.Use(h.requestLog)
	r.Use(h.recoverPanic)
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, ErrorDetail{Code: ErrCodeNotFound, Message: "no such route"})
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusMethodNotAllowed, ErrorDetail{Code: ErrCodeMethodNotAllowed, Message: "method not allowed"})
	})
	r.Get("/healthz", h.handleHealthz)
	r.Route("/v1", func(r chi.Router) {
		// Ticket 6.2: every route below carries its ADR-007 scope — a new
		// /v1 route without a requireScope wrapper fails the route-coverage
		// test in auth_routes_test.go. Ticket 6.4: each also carries its
		// rate-limit route class, mounted after auth (the bucket key is the
		// authenticated key_id; 401/403 requests consume no tokens), with
		// the same coverage discipline in ratelimit_routes_test.go.
		r.With(h.requireScope(ScopeSubmit), h.rateLimit(classSubmit)).Post("/runs", h.handleSubmitRun)
		r.With(h.requireScope(ScopeRead), h.rateLimit(classRead)).Get("/runs", h.handleListRuns)
		r.With(h.requireScope(ScopeRead), h.rateLimit(classRead)).Get("/runs/{runID}", h.handleGetRun)
		// Run lifecycle (ticket 6.5): steering work is the submit scope
		// (ADR-007), so these mount per-route — the /runs subtree mixes
		// read and submit.
		r.With(h.requireScope(ScopeSubmit), h.rateLimit(classSubmit)).Post("/runs/{runID}/cancel", h.handleCancelRun)
		r.With(h.requireScope(ScopeSubmit), h.rateLimit(classSubmit)).Post("/runs/{runID}/park", h.handleParkRun)
		r.With(h.requireScope(ScopeSubmit), h.rateLimit(classSubmit)).Post("/runs/{runID}/unpark", h.handleUnparkRun)
		r.With(h.requireScope(ScopeSubmit), h.rateLimit(classSubmit)).Post("/runs/{runID}/steps/{stepID}/requeue", h.handleRequeueStep)
		// Per-step logs (ticket 7.4).
		r.With(h.requireScope(ScopeRead), h.rateLimit(classRead)).Get("/runs/{runID}/steps/{stepID}/logs", h.handleStepLogs)
		// Definition registry (ticket 6.5).
		r.With(h.requireScope(ScopeSubmit), h.rateLimit(classSubmit)).Post("/definitions", h.handleCreateDefinition)
		r.With(h.requireScope(ScopeRead), h.rateLimit(classRead)).Get("/definitions", h.handleListDefinitions)
		r.With(h.requireScope(ScopeRead), h.rateLimit(classRead)).Get("/definitions/{defID}", h.handleGetDefinition)
		r.With(h.requireScope(ScopeRead), h.rateLimit(classRead)).Get("/definitions/{name}/versions", h.handleListDefinitionVersions)
		r.With(h.requireScope(ScopeSubmit), h.rateLimit(classSubmit)).Post("/definitions/{name}/versions", h.handleCreateDefinitionVersion)
		// Plugin catalog (ticket 8.1, ADR-009).
		r.With(h.requireScope(ScopeRead), h.rateLimit(classRead)).Get("/plugins", h.handleListPlugins)
		r.Route("/keys", func(r chi.Router) {
			r.Use(h.requireScope(ScopeAdmin))
			r.Use(h.rateLimit(classAdmin))
			r.Post("/", h.handleCreateKey)
			r.Get("/", h.handleListKeys)
			r.Delete("/{keyID}", h.handleRevokeKey)
		})
	})
	h.router = r
	return h, nil
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.router.ServeHTTP(w, r)
}

// handleHealthz reports liveness: 200 while Postgres answers a ping.
func (h *Handler) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := h.st.Ping(r.Context()); err != nil {
		log.From(r.Context()).WarnContext(r.Context(), "healthz: postgres ping failed", slog.Any("error", err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "degraded"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// requestLog stamps the base logger into the request context (so handlers
// and the store log with request scope) and emits one structured line per
// request — carrying key_id whenever the request authenticated (ticket
// 6.2), reported back up from requireScope via the authStamp slot. It is
// also the request-metrics recording point (ticket 7.2): after routing,
// chi's RouteContext holds the matched pattern, which is the bounded
// `route` label ADR-008 requires — never the raw path. And it is the
// span-refinement point (ticket 7.3): the otelhttp server span wrapping
// the router gets renamed from its method-only placeholder to
// "<METHOD> <route pattern>" once routing has resolved the pattern, and
// trace_id/span_id join the request log line and every handler line.
func (h *Handler) requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := h.now()
		h.metrics.RequestStarted()
		defer h.metrics.RequestFinished()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		stamp := &authStamp{}
		ctx := authStampInto(log.Into(r.Context(), h.logger), stamp)
		// The otelhttp server span is already in the request context when
		// tracing is on; a no-op when it is off.
		ctx = obstrace.WithLogContext(ctx)
		next.ServeHTTP(rec, r.WithContext(ctx))
		duration := h.now().Sub(start)
		route := metricRoute(ctx)
		h.metrics.Request(route, metricMethod(r.Method), rec.status, duration)
		if route != "unmatched" {
			if span := oteltrace.SpanFromContext(ctx); span.SpanContext().IsValid() {
				span.SetName(r.Method + " " + route)
			}
		}
		attrs := []slog.Attr{
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Duration("duration", duration),
		}
		if sc := oteltrace.SpanContextFromContext(ctx); sc.IsValid() {
			attrs = append(attrs, log.TraceID(sc.TraceID().String()), log.SpanID(sc.SpanID().String()))
		}
		if stamp.keyID != "" {
			attrs = append(attrs, log.KeyID(stamp.keyID))
		}
		h.logger.LogAttrs(r.Context(), slog.LevelInfo, "http request", attrs...)
	})
}

// metricRoute resolves the bounded route label after routing: the chi
// route pattern for matched requests, "unmatched" for 404/405 fallbacks.
// The RouteContext is allocated before the middleware chain runs and
// mutated during routing, so reading it after next.ServeHTTP sees the
// final pattern.
func metricRoute(ctx context.Context) string {
	if rctx := chi.RouteContext(ctx); rctx != nil {
		if pattern := rctx.RoutePattern(); pattern != "" {
			return pattern
		}
	}
	return "unmatched"
}

// metricMethod clamps the method label to the vocabulary chi routes.
// Matched requests can only carry routed methods, but 404/405 fallbacks
// echo whatever verb the client sent — an unbounded label value without
// this clamp (ADR-008).
func metricMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodHead, http.MethodOptions:
		return method
	default:
		return "other"
	}
}

// authStamp is a mutable per-request slot: requestLog installs it before
// routing, requireScope (which runs later, inside the routing tree — a
// context added there cannot flow back up) fills in the authenticated
// key_id. Written and read on the request goroutine, so no locking.
type authStamp struct{ keyID string }

// ctxKeyAuthStamp keys the authStamp slot in the request context.
type ctxKeyAuthStamp struct{}

func authStampInto(ctx context.Context, s *authStamp) context.Context {
	return context.WithValue(ctx, ctxKeyAuthStamp{}, s)
}

func authStampFrom(ctx context.Context) (*authStamp, bool) {
	s, ok := ctx.Value(ctxKeyAuthStamp{}).(*authStamp)
	return s, ok
}

// recoverPanic converts a handler panic into a logged 500 carrying the
// JSON error envelope instead of a dropped connection (post-M4 audit).
// Registered inside requestLog so the request line records the 500.
// http.ErrAbortHandler passes through — it is net/http's sanctioned way
// to abort a response and must keep its meaning.
func (h *Handler) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(rec)
			}
			log.From(r.Context()).ErrorContext(r.Context(), "api: handler panic",
				slog.Any("panic", rec),
				slog.String("stack", string(debug.Stack())))
			// If the handler already wrote headers this is a no-op write
			// logged by net/http; the connection still ends cleanly.
			writeError(w, http.StatusInternalServerError, ErrorDetail{
				Code: ErrCodeInternal, Message: "internal error",
			})
		}()
		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures the response status for the request log.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// writeJSON writes v as the response body with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// Headers are gone; nothing to do but note it for the request log.
		slog.Default().Warn("api: encoding response", slog.Any("error", err))
	}
}

// writeError writes the error envelope with the given status.
func writeError(w http.ResponseWriter, status int, detail ErrorDetail) {
	writeJSON(w, status, ErrorBody{Error: detail})
}

// internalError logs the cause and answers 500 without echoing it — an
// internal message could leak state a client has no business seeing.
func internalError(w http.ResponseWriter, r *http.Request, what string, err error) {
	log.From(r.Context()).ErrorContext(r.Context(), "api: "+what, slog.Any("error", err))
	writeError(w, http.StatusInternalServerError, ErrorDetail{
		Code:    ErrCodeInternal,
		Message: fmt.Sprintf("%s failed", what),
	})
}
