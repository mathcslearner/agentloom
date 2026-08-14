package api

// GET /v1/plugins (ticket 8.1, ADR-009): the plugin catalog compiled
// into the API binary — kind, name, version, capability flags, and each
// plugin's generated config schema, the machine-usable form M17.4's UI
// forms consume. The catalog is static for the process lifetime (the
// in-process compilation model), so the response is precomputed at
// construction and the handler is a pure write.

import (
	"net/http"

	"github.com/mathcslearner/agentloom/internal/plugin"
)

// WithPlugins sets the plugin catalog served on GET /v1/plugins (ticket
// 8.1). cmd/api passes the builtin registry's manifests; the default is
// an empty catalog, which the endpoint serves as an empty list — tests
// and callers that never wire plugins keep a working route. The slice is
// re-sorted defensively into the canonical (kind, name) order.
func WithPlugins(manifests []plugin.Manifest) Option {
	return func(h *Handler) {
		ms := append([]plugin.Manifest(nil), manifests...)
		plugin.SortManifests(ms)
		infos := make([]PluginInfo, 0, len(ms))
		for _, m := range ms {
			infos = append(infos, PluginInfo{
				Kind:        string(m.Kind),
				Name:        m.Name,
				Version:     m.Version,
				Description: m.Description,
				Capabilities: PluginCapabilities{
					SideEffectful: m.Capabilities.SideEffectful,
					Cacheable:     m.Capabilities.Cacheable,
					CostBearing:   m.Capabilities.CostBearing,
				},
				ConfigSchema: m.ConfigSchema,
			})
		}
		h.plugins = infos
	}
}

// handleListPlugins serves GET /v1/plugins (read scope, read rate-limit
// class): the whole catalog in one page — a few tens of KB, served
// rarely, so no pagination or filters in v1 (ADR-009).
func (h *Handler) handleListPlugins(w http.ResponseWriter, _ *http.Request) {
	plugins := h.plugins
	if plugins == nil {
		plugins = []PluginInfo{}
	}
	writeJSON(w, http.StatusOK, ListPluginsResponse{Plugins: plugins})
}
