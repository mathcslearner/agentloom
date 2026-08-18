package exec

// Ticket 8.1's plugin-SPI guards: production builtins must self-describe
// (no synthesized manifests in the shipped sets), manifests must match
// ADR-009's flag table, and the registry must enforce manifest identity.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/cache"
	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/plugin"
)

// describedStub is a self-describing test executor with a settable
// manifest, for identity-rule coverage.
type describedStub struct {
	typ      string
	manifest plugin.Manifest
}

func (s describedStub) Type() string { return s.typ }

func (s describedStub) Execute(context.Context, StepContext) (Output, error) {
	return Output{}, nil
}

func (s describedStub) PluginManifest() plugin.Manifest { return s.manifest }

// builtinCapabilities is ADR-009's flag table, pinned executable. A flag
// change is a deliberate ADR amendment, never drift.
var builtinCapabilities = map[string]plugin.Capabilities{
	"noop":             {Cacheable: true},
	"echo":             {Cacheable: true},
	"sleep":            {},
	"fail_n_times":     {},
	"join":             {},
	"branch":           {},
	"map":              {},
	"gather":           {},
	"counter":          {SideEffectful: true},
	"effectful_echo":   {SideEffectful: true},
	"blackboard_write": {SideEffectful: true},
	"llm":              {Cacheable: true, CostBearing: true},
	"planner":          {Cacheable: true, CostBearing: true},
	"agent":            {Cacheable: true, CostBearing: true},
	"tool":             {SideEffectful: true},
	"retrieve":         {Cacheable: true},
	"human_approval":   {SideEffectful: true},
}

// stubTypes are the dev-stub executors, whose manifests carry the
// pre-release stub version (ADR-009). The llm stub was replaced by the
// production executor in ticket 8.6, the tool stub in 8.7, and the last
// one — retrieve — in 8.8, so the set is now empty (every builtin ships at
// a real 1.0.0). It stays here as the enforced record: a future dev stub
// must be listed, and nothing may carry the -stub suffix that is not.
var stubTypes = map[string]bool{}

// TestBuiltinManifestConformance pins that every shipped builtin
// self-describes (ADR-009's synthesized-manifest escape hatch is for
// test doubles only): real version strings, the ADR flag table, and a
// config schema generated from the dag catalog.
func TestBuiltinManifestConformance(t *testing.T) {
	t.Parallel()

	manifests := Builtins(nil, nil, nil).Manifests()
	if got, want := len(manifests), len(builtinCapabilities); got != want {
		t.Fatalf("Builtins has %d manifests, flag table has %d — keep them in lockstep", got, want)
	}
	for _, m := range manifests {
		if m.Kind != plugin.KindExecutor {
			t.Errorf("%s: manifest kind %q — want executor", m.Name, m.Kind)
		}
		if m.Version == "0.0.0" {
			t.Errorf("%s: synthesized manifest in the shipped set — implement PluginManifest", m.Name)
		}
		if stub := stubTypes[m.Name]; stub != strings.HasSuffix(m.Version, "-stub") {
			t.Errorf("%s: version %q — stub executors (and only they) carry the -stub pre-release", m.Name, m.Version)
		}
		if want, ok := builtinCapabilities[m.Name]; !ok {
			t.Errorf("%s: not in the ADR-009 flag table", m.Name)
		} else if m.Capabilities != want {
			t.Errorf("%s: capabilities %+v — ADR-009 table says %+v", m.Name, m.Capabilities, want)
		}
		if m.Description == "" {
			t.Errorf("%s: manifest has no description", m.Name)
		}
		if m.ConfigSchema == nil {
			t.Errorf("%s: manifest has no config schema — catalog types get one from dag.StepConfigSchema", m.Name)
			continue
		}
		want, err := dag.StepConfigSchema(dag.StepType(m.Name))
		if err != nil {
			t.Errorf("%s: StepConfigSchema: %v", m.Name, err)
		} else if string(m.ConfigSchema) != string(want) {
			t.Errorf("%s: manifest schema differs from dag.StepConfigSchema", m.Name)
		}
	}
}

// TestCachePolicyTracksCapabilityFlags cross-checks the ADR-011 policy
// (ticket 9.4) against the real builtin capability flags: a capability-flag
// drift must break the encoded cache policy, not just the flag table. The
// eligibility gate is purely the executor's flags here — for a tool step the
// *runtime* decision (9.5) consults the invoked tool's manifest, not the tool
// executor's side_effectful one, so a cacheable tool like json_transform is
// still cached (covered directly in internal/cache's policy tests).
func TestCachePolicyTracksCapabilityFlags(t *testing.T) {
	t.Parallel()

	const ttl = time.Hour
	for _, m := range Builtins(nil, nil, nil).Manifests() {
		caps := m.Capabilities
		eligible := caps.Cacheable && !caps.SideEffectful

		// Deterministic default: caches read-write iff eligible.
		if d := cache.Decide(caps, true, nil, ttl); d.Read != eligible || d.Write != eligible {
			t.Errorf("%s: deterministic default read/write = %v/%v, want %v/%v", m.Name, d.Read, d.Write, eligible, eligible)
		}
		// Non-deterministic default always bypasses.
		if d := cache.Decide(caps, false, nil, ttl); d.Read || d.Write {
			t.Errorf("%s: non-deterministic default should bypass, got read/write = %v/%v", m.Name, d.Read, d.Write)
		}
		// read_write opt-in caches iff eligible (never opens the hard gate).
		if d := cache.Decide(caps, false, &dag.CachePolicy{Mode: dag.CacheReadWrite}, ttl); d.Read != eligible || d.Write != eligible {
			t.Errorf("%s: read_write opt-in read/write = %v/%v, want %v/%v", m.Name, d.Read, d.Write, eligible, eligible)
		}
		// off opt-out always bypasses.
		if d := cache.Decide(caps, true, &dag.CachePolicy{Mode: dag.CacheOff}, ttl); d.Read || d.Write {
			t.Errorf("%s: off opt-out should bypass, got read/write = %v/%v", m.Name, d.Read, d.Write)
		}
	}
}

// TestRegisterSynthesizedManifest covers the test-double escape hatch:
// a bare Executor gets ADR-009's conservative manifest and, for a
// non-catalog type, no config schema.
func TestRegisterSynthesizedManifest(t *testing.T) {
	t.Parallel()

	r, err := NewRegistry(stubExecutor{typ: "bare_stub"})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	ms := r.Manifests()
	if len(ms) != 1 {
		t.Fatalf("Manifests() = %d entries, want 1", len(ms))
	}
	m := ms[0]
	if m.Name != "bare_stub" || m.Kind != plugin.KindExecutor {
		t.Errorf("synthesized identity = %s %q", m.Kind, m.Name)
	}
	if m.Version != "0.0.0" {
		t.Errorf("synthesized version = %q, want 0.0.0", m.Version)
	}
	want := plugin.Capabilities{SideEffectful: true}
	if m.Capabilities != want {
		t.Errorf("synthesized capabilities = %+v — want the conservative %+v", m.Capabilities, want)
	}
	if m.ConfigSchema != nil {
		t.Errorf("non-catalog type got a config schema: %s", m.ConfigSchema)
	}
}

// TestRegisterCatalogTypeGetsSchema covers the schema fill-in: a bare
// executor whose type IS a catalog step type still gets the generated
// schema attached.
func TestRegisterCatalogTypeGetsSchema(t *testing.T) {
	t.Parallel()

	r, err := NewRegistry(stubExecutor{typ: string(dag.StepNoop)})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	m := r.Manifests()[0]
	if m.ConfigSchema == nil {
		t.Fatal("catalog-typed executor got no config schema")
	}
	if !json.Valid(m.ConfigSchema) {
		t.Fatalf("attached schema is not valid JSON: %s", m.ConfigSchema)
	}
}

// TestRegisterManifestIdentityRules covers ADR-009's identity
// enforcement: a self-described manifest may not claim another name or
// kind, and an invalid manifest fails registration.
func TestRegisterManifestIdentityRules(t *testing.T) {
	t.Parallel()

	good := plugin.Manifest{Kind: plugin.KindExecutor, Name: "described", Version: "1.2.3"}
	if _, err := NewRegistry(describedStub{typ: "described", manifest: good}); err != nil {
		t.Fatalf("valid self-description rejected: %v", err)
	}

	cases := []struct {
		name     string
		manifest plugin.Manifest
		wantSub  string
	}{
		{"name mismatch", plugin.Manifest{Kind: plugin.KindExecutor, Name: "other", Version: "1.0.0"}, "may not claim another identity"},
		{"kind mismatch", plugin.Manifest{Kind: plugin.KindTool, Name: "described", Version: "1.0.0"}, "must register as"},
		{"bad version", plugin.Manifest{Kind: plugin.KindExecutor, Name: "described", Version: "one"}, "semver"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewRegistry(describedStub{typ: "described", manifest: tc.manifest})
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("NewRegistry error = %v, want substring %q", err, tc.wantSub)
			}
		})
	}
}
