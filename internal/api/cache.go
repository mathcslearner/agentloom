package api

// Response-cache ops surface (ticket 9.6, ADR-011): the admin endpoints
// that invalidate cache entries by namespace and report per-plugin hit
// rates. Both operate over the CacheOps seam (a *cache/redisstore.Store in
// cmd/api); a nil seam answers 503 cache_unavailable, since the cache is an
// opt-in extra and never a boot dependency (ADR-002 — the API's Redis
// independence for dispatch is untouched).
//
// A bust is a privileged, auditable action: it is admin-scoped, and every
// successful bust emits a structured audit line carrying the actor key id
// (ADR-007), the namespace it matched, and the number of entries removed.

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/mathcslearner/agentloom/internal/cache"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/plugin"
)

// cacheableKinds is the closed set of plugin kinds a cache bust may target —
// the kinds whose outputs are ever cached (ADR-011: concrete providers,
// tools, retrievers). Executors and validators are never cache-key plugins,
// so naming one is a 400, not a silent no-op that could mask a typo.
var cacheableKinds = map[string]plugin.Kind{
	string(plugin.KindModelProvider): plugin.KindModelProvider,
	string(plugin.KindTool):          plugin.KindTool,
	string(plugin.KindRetriever):     plugin.KindRetriever,
}

// handleCacheBust is POST /v1/cache/bust (admin): remove response-cache
// entries by namespace. The request selects all entries, one plugin kind, or
// one concrete plugin; the store SCAN-batches a non-blocking UNLINK and
// returns the count. The action is audit-logged with the actor key id.
func (h *Handler) handleCacheBust(w http.ResponseWriter, r *http.Request) {
	if h.cacheOps == nil {
		h.writeCacheUnavailable(w)
		return
	}

	var req CacheBustRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, ErrorDetail{
			Code: ErrCodeInvalidRequest, Message: "decoding request body: " + err.Error(),
		})
		return
	}
	if dec.More() {
		writeError(w, http.StatusBadRequest, ErrorDetail{
			Code: ErrCodeInvalidRequest, Message: "request body holds more than one JSON document",
		})
		return
	}

	match, ok := h.parseBustMatch(w, req)
	if !ok {
		return
	}

	deleted, err := h.cacheOps.Bust(r.Context(), match)
	if err != nil {
		internalError(w, r, "busting cache", err)
		return
	}

	// The audit record (ADR-007): actor key id, the namespace matched, and
	// the effect. Stamped at info so it lands in ops logs by default.
	logger := log.From(r.Context())
	attrs := []slog.Attr{
		slog.String("action", "cache_bust"),
		slog.Int64("deleted", deleted),
	}
	if match.All() {
		attrs = append(attrs, slog.String("scope", "all"))
	} else {
		attrs = append(attrs, slog.String("plugin_kind", string(match.Kind)))
		if match.Name != "" {
			attrs = append(attrs, slog.String("plugin_name", match.Name))
		}
	}
	if id, ok := identityFrom(r.Context()); ok {
		attrs = append(attrs, log.KeyID(id.keyID))
	}
	logger.LogAttrs(r.Context(), slog.LevelInfo, "cache bust", attrs...)

	writeJSON(w, http.StatusOK, CacheBustResponse{Deleted: deleted})
}

// parseBustMatch validates the request into a cache.BustMatch, answering the
// 400 itself. A plugin_name without a plugin_kind, or a plugin_kind outside
// the cacheable set, is rejected.
func (h *Handler) parseBustMatch(w http.ResponseWriter, req CacheBustRequest) (cache.BustMatch, bool) {
	if req.PluginKind == "" {
		if req.PluginName != "" {
			writeError(w, http.StatusBadRequest, ErrorDetail{
				Code: ErrCodeInvalidRequest, Message: "plugin_name requires plugin_kind",
			})
			return cache.BustMatch{}, false
		}
		return cache.BustMatch{}, true // bust everything
	}
	kind, ok := cacheableKinds[req.PluginKind]
	if !ok {
		writeError(w, http.StatusBadRequest, ErrorDetail{
			Code:    ErrCodeInvalidRequest,
			Message: "plugin_kind must be one of model_provider, tool, retriever",
		})
		return cache.BustMatch{}, false
	}
	return cache.BustMatch{Kind: kind, Name: req.PluginName}, true
}

// handleCacheStats is GET /v1/cache/stats (admin): per-plugin cumulative
// hit/miss/store counters with derived hit rates, read from the cache
// store's Redis counters (ticket 9.6). The numbers reconcile against the
// worker fleet's engine_cache_* Prometheus counters (ADR-011).
func (h *Handler) handleCacheStats(w http.ResponseWriter, r *http.Request) {
	if h.cacheOps == nil {
		h.writeCacheUnavailable(w)
		return
	}
	stats, err := h.cacheOps.Stats(r.Context())
	if err != nil {
		internalError(w, r, "reading cache stats", err)
		return
	}
	plugins := make([]CachePluginStat, 0, len(stats))
	for _, s := range stats {
		plugins = append(plugins, CachePluginStat{
			Kind:    string(s.Kind),
			Name:    s.Name,
			Hits:    s.Hits,
			Misses:  s.Misses,
			Stores:  s.Stores,
			HitRate: s.HitRate,
		})
	}
	writeJSON(w, http.StatusOK, CacheStatsResponse{Plugins: plugins})
}

// writeCacheUnavailable answers 503 when the cache ops surface is not wired.
func (h *Handler) writeCacheUnavailable(w http.ResponseWriter) {
	writeError(w, http.StatusServiceUnavailable, ErrorDetail{
		Code:    ErrCodeCacheUnavailable,
		Message: "response cache is not enabled on this API",
	})
}
