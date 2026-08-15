package limits_test

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mathcslearner/agentloom/internal/limits"
)

const validConfig = `{
  "resources": [
    {"name": "anthropic:claude-sonnet-5",
     "requests": {"per_minute": 60},
     "tokens":   {"per_minute": 200000, "burst": 400000}},
    {"name": "openai:*", "requests": {"per_minute": 120}},
    {"name": "tool:http_request", "requests": {"per_minute": 30}}
  ]
}`

func TestParseValid(t *testing.T) {
	t.Parallel()

	set, err := limits.Parse([]byte(validConfig))
	if err != nil {
		t.Fatalf("Parse valid config: unexpected error: %v", err)
	}
	if set.Len() != 3 {
		t.Fatalf("Len = %d, want 3", set.Len())
	}
	// Names are returned sorted regardless of declaration order.
	wantNames := []string{"anthropic:claude-sonnet-5", "openai:*", "tool:http_request"}
	got := set.Names()
	if len(got) != len(wantNames) {
		t.Fatalf("Names = %v, want %v", got, wantNames)
	}
	for i, w := range wantNames {
		if got[i] != w {
			t.Errorf("Names[%d] = %q, want %q", i, got[i], w)
		}
	}

	r, ok := set.Lookup("anthropic:claude-sonnet-5")
	if !ok {
		t.Fatal("exact Lookup missed a configured resource")
	}
	if r.Requests == nil || r.Requests.PerMinute != 60 {
		t.Errorf("requests = %+v, want per_minute 60", r.Requests)
	}
	if r.Tokens == nil || r.Tokens.PerMinute != 200000 || r.Tokens.Burst != 400000 {
		t.Errorf("tokens = %+v, want per_minute 200000 burst 400000", r.Tokens)
	}
}

func TestLookupWildcardAndUnlimited(t *testing.T) {
	t.Parallel()

	set, err := limits.Parse([]byte(validConfig))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// A model with no exact entry resolves to its provider wildcard.
	r, ok := set.Lookup("openai:gpt-4o")
	if !ok {
		t.Fatal("wildcard Lookup missed openai:gpt-4o")
	}
	if r.Name != "openai:*" {
		t.Errorf("wildcard resolved to %q, want openai:*", r.Name)
	}

	// Exact wins over wildcard when both could match: anthropic has an exact
	// entry but no wildcard, so an unlisted anthropic model is unlimited.
	if _, ok := set.Lookup("anthropic:claude-opus-5"); ok {
		t.Error("anthropic:claude-opus-5 resolved, want unlimited (no exact, no wildcard)")
	}

	// A resource of an entirely unconfigured provider is unlimited.
	if _, ok := set.Lookup("cohere:command"); ok {
		t.Error("cohere:command resolved, want unlimited")
	}

	// A bare name with no provider segment never wildcard-matches.
	if _, ok := set.Lookup("bareword"); ok {
		t.Error("bareword resolved, want unlimited")
	}
}

func TestRateHelpers(t *testing.T) {
	t.Parallel()

	// Explicit burst is the capacity; refill is per-minute / 60.
	r := limits.Rate{PerMinute: 120, Burst: 300}
	if got := r.RefillPerSec(); got != 2 {
		t.Errorf("RefillPerSec = %v, want 2", got)
	}
	if got := r.Capacity(); got != 300 {
		t.Errorf("Capacity = %d, want 300 (explicit burst)", got)
	}

	// Absent burst defaults to one minute of refill, rounded up.
	r = limits.Rate{PerMinute: 90.5}
	if got := r.Capacity(); got != 91 {
		t.Errorf("Capacity = %d, want 91 (ceil of per_minute)", got)
	}
	if got := r.RefillPerSec(); math.Abs(got-90.5/60) > 1e-12 {
		t.Errorf("RefillPerSec = %v, want %v", got, 90.5/60)
	}
}

func TestParseErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		config  string
		wantAll []string // substrings the joined error must contain
	}{
		{
			name:    "unknown field",
			config:  `{"resources": [{"name": "a:b", "requests": {"per_minute": 1}, "extra": 1}]}`,
			wantAll: []string{"unknown field"},
		},
		{
			name:    "unknown top-level field",
			config:  `{"resources": [], "surprise": true}`,
			wantAll: []string{"unknown field"},
		},
		{
			name:    "missing name",
			config:  `{"resources": [{"requests": {"per_minute": 1}}]}`,
			wantAll: []string{"resources[0]", "name is required"},
		},
		{
			name:    "whitespace in name",
			config:  `{"resources": [{"name": "a b", "requests": {"per_minute": 1}}]}`,
			wantAll: []string{"whitespace"},
		},
		{
			name:    "star not trailing",
			config:  `{"resources": [{"name": "*:model", "requests": {"per_minute": 1}}]}`,
			wantAll: []string{"trailing", "wildcard"},
		},
		{
			name:    "two stars",
			config:  `{"resources": [{"name": "a:*:*", "requests": {"per_minute": 1}}]}`,
			wantAll: []string{"wildcard"},
		},
		{
			name:    "duplicate name",
			config:  `{"resources": [{"name": "a:b", "requests": {"per_minute": 1}}, {"name": "a:b", "tokens": {"per_minute": 2}}]}`,
			wantAll: []string{"duplicate resource name", `"a:b"`},
		},
		{
			name:    "no limits",
			config:  `{"resources": [{"name": "a:b"}]}`,
			wantAll: []string{"at least one of requests/tokens"},
		},
		{
			name:    "zero per_minute",
			config:  `{"resources": [{"name": "a:b", "requests": {"per_minute": 0}}]}`,
			wantAll: []string{"per_minute must be a positive finite number"},
		},
		{
			name:    "negative per_minute",
			config:  `{"resources": [{"name": "a:b", "tokens": {"per_minute": -5}}]}`,
			wantAll: []string{"per_minute must be a positive finite number"},
		},
		{
			name:    "negative burst",
			config:  `{"resources": [{"name": "a:b", "requests": {"per_minute": 10, "burst": -1}}]}`,
			wantAll: []string{"burst must not be negative"},
		},
		{
			name:    "malformed json",
			config:  `{"resources": [`,
			wantAll: []string{"decoding config"},
		},
		{
			name:    "trailing content",
			config:  `{"resources": []} garbage`,
			wantAll: []string{"trailing content"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := limits.Parse([]byte(tc.config))
			if err == nil {
				t.Fatalf("Parse(%s): want error, got nil", tc.name)
			}
			for _, want := range tc.wantAll {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestParseReportsAllErrorsAtOnce(t *testing.T) {
	t.Parallel()

	// Three independent problems across two resources — all must surface in
	// one joined error, matching the config package's fail-with-everything
	// discipline.
	config := `{"resources": [
      {"name": "a b", "requests": {"per_minute": 0}},
      {"name": "ok:c"}
    ]}`
	_, err := limits.Parse([]byte(config))
	if err == nil {
		t.Fatal("Parse: want error, got nil")
	}
	for _, want := range []string{"whitespace", "per_minute must be a positive finite number", "at least one of requests/tokens"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("joined error %q does not contain %q", err, want)
		}
	}
}

func TestParseOverflowRate(t *testing.T) {
	t.Parallel()

	// JSON has no NaN/Inf literals; encoding/json rejects an overflowing
	// magnitude at decode time before it can reach validateRate (the Inf/NaN
	// guard there stays as defense in depth). Either way the config is
	// rejected — a non-finite rate never yields a usable Set.
	_, err := limits.Parse([]byte(`{"resources": [{"name": "a:b", "requests": {"per_minute": 1e400}}]}`))
	if err == nil {
		t.Fatal("Parse with 1e400 per_minute: want error, got nil")
	}
}

func TestLoadInline(t *testing.T) {
	t.Parallel()

	set, err := limits.Load(validConfig, "")
	if err != nil {
		t.Fatalf("Load inline: %v", err)
	}
	if set.Len() != 3 {
		t.Errorf("Len = %d, want 3", set.Len())
	}
}

func TestLoadFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "resources.json")
	if err := os.WriteFile(path, []byte(validConfig), 0o600); err != nil {
		t.Fatalf("writing temp config: %v", err)
	}
	set, err := limits.Load("", path)
	if err != nil {
		t.Fatalf("Load file: %v", err)
	}
	if set.Len() != 3 {
		t.Errorf("Len = %d, want 3", set.Len())
	}
}

func TestLoadNeitherIsEmpty(t *testing.T) {
	t.Parallel()

	set, err := limits.Load("", "")
	if err != nil {
		t.Fatalf("Load empty: %v", err)
	}
	if set.Len() != 0 {
		t.Errorf("Len = %d, want 0 (no configured limits)", set.Len())
	}
	// Everything is unlimited on an empty set.
	if _, ok := set.Lookup("anthropic:claude-sonnet-5"); ok {
		t.Error("empty set resolved a resource, want unlimited")
	}
	// Whitespace-only sources also count as empty.
	set, err = limits.Load("   ", "  ")
	if err != nil {
		t.Fatalf("Load whitespace: %v", err)
	}
	if set.Len() != 0 {
		t.Errorf("Len = %d, want 0", set.Len())
	}
}

func TestLoadBothIsError(t *testing.T) {
	t.Parallel()

	_, err := limits.Load(validConfig, "/some/path.json")
	if err == nil {
		t.Fatal("Load with both inline and file: want error, got nil")
	}
	if !strings.Contains(err.Error(), "not both") {
		t.Errorf("error %q does not explain the mutual exclusion", err)
	}
}

func TestLoadFileMissing(t *testing.T) {
	t.Parallel()

	_, err := limits.Load("", filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err == nil {
		t.Fatal("Load with missing file: want error, got nil")
	}
	if !strings.Contains(err.Error(), "reading config file") {
		t.Errorf("error %q does not name the read failure", err)
	}
}

func TestLoadInvalidInline(t *testing.T) {
	t.Parallel()

	_, err := limits.Load(`{"resources": [{"name": "a:b"}]}`, "")
	if err == nil {
		t.Fatal("Load with invalid inline config: want error, got nil")
	}
	if !strings.Contains(err.Error(), "inline config") {
		t.Errorf("error %q does not name the inline source", err)
	}
}

func TestNilSetSafety(t *testing.T) {
	t.Parallel()

	var s *limits.Set
	if _, ok := s.Lookup("anything"); ok {
		t.Error("nil Set resolved a resource")
	}
	if s.Len() != 0 {
		t.Errorf("nil Set Len = %d, want 0", s.Len())
	}
	if s.Names() != nil {
		t.Errorf("nil Set Names = %v, want nil", s.Names())
	}
}
