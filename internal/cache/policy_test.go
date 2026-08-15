package cache_test

import (
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/cache"
	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/plugin"
)

const defaultTTL = time.Hour

// cacheable is an eligible plugin's flags (e.g. llm, retrieve, json_transform).
var cacheable = plugin.Capabilities{Cacheable: true}

// sideEffectful is an ineligible plugin's flags (e.g. http_request, counter).
var sideEffectful = plugin.Capabilities{SideEffectful: true, Cacheable: false}

func mode(m dag.CacheMode) *dag.CachePolicy { return &dag.CachePolicy{Mode: m} }

// TestDecideMatrix is the ADR-011 policy matrix, encoded and pinned: step
// determinism × capability flags × step-level mode → read/write decision.
func TestDecideMatrix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		caps          plugin.Capabilities
		deterministic bool
		policy        *dag.CachePolicy
		wantRead      bool
		wantWrite     bool
	}{
		// Eligible + deterministic: default caches read-write.
		{"deterministic default", cacheable, true, nil, true, true},
		// Eligible + non-deterministic: default bypasses.
		{"non-deterministic default", cacheable, false, nil, false, false},
		// Opt-in overrides the non-deterministic default.
		{"non-deterministic read_write", cacheable, false, mode(dag.CacheReadWrite), true, true},
		{"non-deterministic read_only", cacheable, false, mode(dag.CacheReadOnly), true, false},
		// Opt-out overrides the deterministic default.
		{"deterministic off", cacheable, true, mode(dag.CacheOff), false, false},
		{"deterministic read_only", cacheable, true, mode(dag.CacheReadOnly), true, false},
		// Ineligible: side-effectful is a hard gate no mode can open.
		{"side-effectful default", sideEffectful, true, nil, false, false},
		{"side-effectful read_write opt-in is a bypass", sideEffectful, true, mode(dag.CacheReadWrite), false, false},
		{"side-effectful read_only opt-in is a bypass", sideEffectful, false, mode(dag.CacheReadOnly), false, false},
		// Ineligible: non-cacheable (e.g. sleep) is also a hard gate.
		{"non-cacheable default", plugin.Capabilities{}, true, nil, false, false},
		{"non-cacheable read_write is a bypass", plugin.Capabilities{}, true, mode(dag.CacheReadWrite), false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := cache.Decide(tc.caps, tc.deterministic, tc.policy, defaultTTL)
			if d.Read != tc.wantRead || d.Write != tc.wantWrite {
				t.Errorf("Decide read/write = %v/%v, want %v/%v", d.Read, d.Write, tc.wantRead, tc.wantWrite)
			}
		})
	}
}

// TestDecideTTLAndScope pins the TTL and scope resolution: step override
// wins over the fleet default, with defensive fallbacks.
func TestDecideTTLAndScope(t *testing.T) {
	t.Parallel()

	t.Run("default ttl and global scope", func(t *testing.T) {
		t.Parallel()
		d := cache.Decide(cacheable, true, nil, defaultTTL)
		if d.TTL != defaultTTL {
			t.Errorf("TTL = %v, want default %v", d.TTL, defaultTTL)
		}
		if d.Scope != dag.CacheGlobal {
			t.Errorf("Scope = %q, want global", d.Scope)
		}
	})

	t.Run("step ttl and scope override", func(t *testing.T) {
		t.Parallel()
		d := cache.Decide(cacheable, false, &dag.CachePolicy{Mode: dag.CacheReadWrite, TTL: "30m", Scope: dag.CacheRun}, defaultTTL)
		if d.TTL != 30*time.Minute {
			t.Errorf("TTL = %v, want 30m", d.TTL)
		}
		if d.Scope != dag.CacheRun {
			t.Errorf("Scope = %q, want run", d.Scope)
		}
	})

	t.Run("unparseable ttl falls back to default", func(t *testing.T) {
		t.Parallel()
		d := cache.Decide(cacheable, true, &dag.CachePolicy{Mode: dag.CacheReadWrite, TTL: "nonsense"}, defaultTTL)
		if d.TTL != defaultTTL {
			t.Errorf("TTL = %v, want default fallback %v", d.TTL, defaultTTL)
		}
	})

	t.Run("bypass carries the zero decision", func(t *testing.T) {
		t.Parallel()
		d := cache.Decide(sideEffectful, true, mode(dag.CacheReadWrite), defaultTTL)
		if d != (cache.Decision{}) {
			t.Errorf("bypass decision = %+v, want zero value", d)
		}
	})
}
