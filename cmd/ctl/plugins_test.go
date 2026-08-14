package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mathcslearner/agentloom/internal/api"
)

// fakePluginsServer stubs GET /v1/plugins, recording the bearer each
// request presented.
func fakePluginsServer(t *testing.T, bearers *[]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/plugins", func(w http.ResponseWriter, r *http.Request) {
		*bearers = append(*bearers, r.Header.Get("Authorization"))
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method %s on /v1/plugins", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.ListPluginsResponse{Plugins: []api.PluginInfo{
			{
				Kind: "executor", Name: "echo", Version: "1.0.0",
				Description:  "Return the configured input as output.",
				Capabilities: api.PluginCapabilities{Cacheable: true},
				ConfigSchema: json.RawMessage(`{"type":"object"}`),
			},
			{
				Kind: "executor", Name: "llm", Version: "0.1.0-stub",
				Capabilities: api.PluginCapabilities{Cacheable: true, CostBearing: true},
			},
			{
				Kind: "executor", Name: "sleep", Version: "1.0.0",
				Capabilities: api.PluginCapabilities{},
			},
		}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestPluginsListRendersTable(t *testing.T) {
	t.Parallel()
	var bearers []string
	srv := fakePluginsServer(t, &bearers)
	readKey := "sk_" + strings.Repeat("r", 43)

	out, _, err := runCtl(t, nil, "plugins", "list", "--api", srv.URL, "--key", readKey)
	if err != nil {
		t.Fatalf("plugins list: %v", err)
	}
	for _, needle := range []string{
		"KIND", "NAME", "VERSION", "CAPABILITIES",
		"executor", "echo", "0.1.0-stub", "cacheable,cost_bearing",
	} {
		if !strings.Contains(out, needle) {
			t.Errorf("listing %q missing %q", out, needle)
		}
	}
	// A flagless plugin renders "-", and schemas are never dumped.
	sleepLine := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "sleep") {
			sleepLine = line
		}
	}
	if !strings.Contains(sleepLine, "-") {
		t.Errorf("sleep line %q does not render flagless capabilities as %q", sleepLine, "-")
	}
	if strings.Contains(out, `"type"`) {
		t.Errorf("listing %q dumps config schemas", out)
	}
	if len(bearers) != 1 || bearers[0] != "Bearer "+readKey {
		t.Errorf("bearer headers = %v, want the --key credential", bearers)
	}
}
