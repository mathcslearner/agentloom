package api

// Ops views (ticket 18.6): the caller-identity endpoint the dashboard reads to
// render permission-gated controls, and the queue-health system-stats endpoint
// behind the operator panel. Both are read-scoped and side-effect-free.
//
// System stats composes the read-only queue-introspection seam (QueueIntrospector,
// a thin cmd/api adapter over the queue handle — the api package never imports
// internal/queue or go-redis, the CacheOps discipline) with Postgres counts.
// Postgres is the only hard dependency, so the endpoint always answers 200:
// when the queue seam is unwired or a queue read fails, Queue is nil and
// QueueError carries the reason, while the DLQ / outbox / runs figures still
// render. A read is not a dispatch, so ADR-002 (the API never dispatches) holds.

import "net/http"

// handleWhoAmI is GET /v1/auth/whoami (ticket 18.6): the authenticated caller's
// key id and granted scopes. The dashboard renders permission-gated controls
// from this — hiding a control the key cannot use — but the server still
// enforces every scope on the underlying route, so this is a UX affordance, not
// the authorization itself. requireScope(read) has already authenticated and
// stamped the identity.
func (h *Handler) handleWhoAmI(w http.ResponseWriter, r *http.Request) {
	id, ok := identityFrom(r.Context())
	if !ok {
		// Unreachable: requireScope stamps the identity before this handler
		// runs. Answer 401 rather than a nil-deref if the middleware chain
		// ever changes.
		writeError(w, http.StatusUnauthorized, ErrorDetail{
			Code: ErrCodeUnauthorized, Message: errUnauthorized.Error(),
		})
		return
	}
	scopes := id.scopes
	if scopes == nil {
		scopes = []string{}
	}
	writeJSON(w, http.StatusOK, WhoAmIResponse{KeyID: id.keyID, Scopes: scopes})
}

// handleSystemStats is GET /v1/system/stats (ticket 18.6): a queue-health
// snapshot for the operator panel. The queue block comes from the introspection
// seam (nil / error ⇒ Queue null + QueueError); the DLQ backlog, outbox backlog,
// and active-run count come from Postgres.
func (h *Handler) handleSystemStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	resp := SystemStatsResponse{ObservedAt: h.now()}

	if h.queueStats == nil {
		resp.QueueError = "queue introspection is not configured on this API instance"
	} else if qv, err := h.queueStats.QueueStats(ctx); err != nil {
		// A single failed queue read must not sink the whole endpoint — the
		// Postgres figures are still useful. Report the queue as unavailable.
		resp.QueueError = "queue introspection failed: " + err.Error()
	} else {
		resp.Queue = &qv
	}

	open, err := h.st.DeadLetters().CountOpen(ctx)
	if err != nil {
		internalError(w, r, "counting open dead letters", err)
		return
	}
	resp.DeadLetters = DeadLetterStatsView{Open: open}

	active, err := h.st.Runs().CountActive(ctx)
	if err != nil {
		internalError(w, r, "counting active runs", err)
		return
	}
	resp.Runs = RunStatsView{Active: active}

	ob, err := h.st.Outbox().Stats(ctx)
	if err != nil {
		internalError(w, r, "reading outbox stats", err)
		return
	}
	resp.Outbox = OutboxStatsView{Backlog: ob.Backlog}
	if ob.OldestCreatedAt != nil {
		ageMs := h.now().Sub(*ob.OldestCreatedAt).Milliseconds()
		if ageMs < 0 {
			ageMs = 0
		}
		resp.Outbox.OldestAgeMs = &ageMs
	}

	writeJSON(w, http.StatusOK, resp)
}
