// Package api implements agentloom's HTTP ingest surface (ticket 4.6, dev
// mode — no auth until M6): run submission, run inspection, and the health
// probe. The API talks only to Postgres: submission writes the run and its
// entry-step outbox rows in CreateRun's single transaction, and the worker
// fleet's dispatchers drain the outbox to Redis (ADR-002 — no central
// scheduler, so no Redis client here).
//
// Routes:
//
//	POST   /v1/runs        submit a run (inline definition or stored ref)
//	GET    /v1/runs/{id}   run status + step tree + attempt history
//	POST   /v1/keys        mint an API key (admin; plaintext shown once)
//	GET    /v1/keys        list API keys (admin; prefixes only)
//	DELETE /v1/keys/{id}   revoke an API key (admin; soft, idempotent)
//	GET    /healthz        liveness (Postgres ping)
//
// Auth (ticket 6.1, ADR-007): the /v1/keys subtree requires an admin
// bearer credential — a stored admin-scoped key, or the env-provided
// root key that bootstraps the first one. The other /v1 routes stay
// anonymous until 6.2 generalizes enforcement.
//
// Every error response carries the JSON envelope {"error": {code, message,
// issues?}}; definition problems surface M1's path-qualified issues
// verbatim. Request and response types live in types.go — cmd/ctl imports
// them as the wire contract until the OpenAPI spec (M6.6) takes over.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/store"
)

// MaxBodyBytes caps request bodies. ADR-003's definition limits are far
// below this; the cap only exists so a hostile body cannot exhaust memory.
const MaxBodyBytes = 1 << 20

// Handler is the API's http.Handler: a chi router over the store.
type Handler struct {
	router chi.Router
	st     *store.Store
	now    func() time.Time
	logger *slog.Logger
	// rootHash is the hex SHA-256 of the env-provided root key (ADR-007
	// admin bootstrap); empty means no root credential exists.
	rootHash string
	// keyRand feeds key generation; production uses crypto/rand, tests
	// may inject a deterministic reader.
	keyRand io.Reader
}

// New builds the Handler. now is the injected clock (project invariant —
// cmd/api passes time.Now); logger is the base request logger (nil means
// slog.Default()); rootKey is the optional bootstrap admin credential
// (ADR-007) — empty disables the root path, and a set-but-malformed key
// is a configuration error caught here rather than a silent 401 later.
func New(st *store.Store, now func() time.Time, logger *slog.Logger, rootKey string) (*Handler, error) {
	if st == nil {
		return nil, errors.New("api: nil store")
	}
	if now == nil {
		return nil, errors.New("api: nil clock — pass the injected time source")
	}
	if logger == nil {
		logger = slog.Default()
	}
	h := &Handler{st: st, now: now, logger: logger, keyRand: cryptoRand}
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
		r.Post("/runs", h.handleSubmitRun)
		r.Get("/runs/{runID}", h.handleGetRun)
		r.Route("/keys", func(r chi.Router) {
			r.Use(h.requireScope(ScopeAdmin))
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
// request.
func (h *Handler) requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := h.now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r.WithContext(log.Into(r.Context(), h.logger)))
		h.logger.LogAttrs(r.Context(), slog.LevelInfo, "http request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Duration("duration", h.now().Sub(start)))
	})
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
