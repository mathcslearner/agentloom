package cache

import (
	"testing"

	"github.com/mathcslearner/agentloom/internal/plugin"
)

func TestBustPattern(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		prefix  string
		match   BustMatch
		want    string
		wantErr bool
	}{
		{
			name:   "all",
			prefix: "cache",
			match:  BustMatch{},
			want:   "cache:v*:*",
		},
		{
			name:   "kind only",
			prefix: "cache",
			match:  BustMatch{Kind: plugin.KindModelProvider},
			want:   "cache:v*:model_provider:*",
		},
		{
			name:   "kind and name",
			prefix: "cache",
			match:  BustMatch{Kind: plugin.KindTool, Name: "json_transform"},
			want:   "cache:v*:tool:json_transform:*",
		},
		{
			name:    "name without kind is rejected",
			prefix:  "cache",
			match:   BustMatch{Name: "mock"},
			wantErr: true,
		},
		{
			name:    "unknown kind is rejected",
			prefix:  "cache",
			match:   BustMatch{Kind: "not_a_kind"},
			wantErr: true,
		},
		{
			name:   "glob metacharacters in the prefix are escaped",
			prefix: "ca*che",
			match:  BustMatch{},
			want:   `ca\*che:v*:*`,
		},
		{
			name:   "glob metacharacters in the name are escaped",
			prefix: "cache",
			match:  BustMatch{Kind: plugin.KindTool, Name: "we*rd"},
			want:   `cache:v*:tool:we\*rd:*`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := BustPattern(tt.prefix, tt.match)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("BustPattern(%q, %+v) = %q, want error", tt.prefix, tt.match, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("BustPattern(%q, %+v) error: %v", tt.prefix, tt.match, err)
			}
			if got != tt.want {
				t.Errorf("BustPattern(%q, %+v) = %q, want %q", tt.prefix, tt.match, got, tt.want)
			}
		})
	}
}

// TestBustPatternExcludesStats: no BustPattern shape can match a stats key,
// so a bust never erases the cumulative counters.
func TestBustPatternExcludesStats(t *testing.T) {
	t.Parallel()
	prefix := "cache"
	p := PluginRef{Kind: plugin.KindModelProvider, Name: "mock", Version: "1.0.0"}
	statsKey := StatsRedisKey(prefix, p)

	for _, m := range []BustMatch{
		{},
		{Kind: plugin.KindModelProvider},
		{Kind: plugin.KindModelProvider, Name: "mock"},
	} {
		pat, err := BustPattern(prefix, m)
		if err != nil {
			t.Fatalf("BustPattern(%+v): %v", m, err)
		}
		if globMatch(pat, statsKey) {
			t.Errorf("BustPattern(%+v) = %q matched stats key %q; stats must survive a bust", m, pat, statsKey)
		}
	}
}

func TestStatsRedisKeyRoundTrip(t *testing.T) {
	t.Parallel()
	prefix := "agentloom:cache"
	want := PluginRef{Kind: plugin.KindRetriever, Name: "pg_fulltext"}
	key := StatsRedisKey(prefix, PluginRef{Kind: want.Kind, Name: want.Name, Version: "1.0.0"})

	got, ok := ParseStatsKey(prefix, key)
	if !ok {
		t.Fatalf("ParseStatsKey(%q, %q) = not ok", prefix, key)
	}
	if got.Kind != want.Kind || got.Name != want.Name {
		t.Errorf("ParseStatsKey = %+v, want kind %q name %q", got, want.Kind, want.Name)
	}

	// A key from a different prefix, and a malformed tail, are skipped.
	if _, ok := ParseStatsKey("other", key); ok {
		t.Error("ParseStatsKey accepted a key from a different prefix")
	}
	if _, ok := ParseStatsKey(prefix, prefix+":"+statsSegment+":only-kind"); ok {
		t.Error("ParseStatsKey accepted a malformed tail (no name)")
	}
}

func TestNewPluginStats(t *testing.T) {
	t.Parallel()
	p := PluginRef{Kind: plugin.KindModelProvider, Name: "mock"}

	s := NewPluginStats(p, 3, 1, 4)
	if s.Hits != 3 || s.Misses != 1 || s.Stores != 4 {
		t.Errorf("counters = %+v", s)
	}
	if s.HitRate != 0.75 {
		t.Errorf("HitRate = %v, want 0.75", s.HitRate)
	}

	// No lookups → zero hit rate, not a divide-by-zero.
	if z := NewPluginStats(p, 0, 0, 0); z.HitRate != 0 {
		t.Errorf("HitRate with no lookups = %v, want 0", z.HitRate)
	}
}

func TestParseCounter(t *testing.T) {
	t.Parallel()
	if n, err := ParseCounter(""); err != nil || n != 0 {
		t.Errorf("ParseCounter(empty) = %d, %v; want 0, nil", n, err)
	}
	if n, err := ParseCounter("42"); err != nil || n != 42 {
		t.Errorf("ParseCounter(42) = %d, %v; want 42, nil", n, err)
	}
	if _, err := ParseCounter("notanumber"); err == nil {
		t.Error("ParseCounter(notanumber) = nil error, want error")
	}
}

// globMatch is a minimal Redis-glob matcher covering exactly the
// metacharacters BustPattern emits (`*` and escaped literals), enough to
// assert the stats-exclusion property without a Redis round-trip.
func globMatch(pattern, s string) bool {
	// Recursive descent over pattern vs s.
	for len(pattern) > 0 {
		switch pattern[0] {
		case '*':
			// Collapse consecutive stars, then try every split.
			for len(pattern) > 1 && pattern[1] == '*' {
				pattern = pattern[1:]
			}
			if len(pattern) == 1 {
				return true // trailing star matches the rest
			}
			for i := 0; i <= len(s); i++ {
				if globMatch(pattern[1:], s[i:]) {
					return true
				}
			}
			return false
		case '\\':
			if len(pattern) < 2 || len(s) == 0 || pattern[1] != s[0] {
				return false
			}
			pattern, s = pattern[2:], s[1:]
		default:
			if len(s) == 0 || pattern[0] != s[0] {
				return false
			}
			pattern, s = pattern[1:], s[1:]
		}
	}
	return len(s) == 0
}
