package cost_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/cost"
)

const validCatalog = `{
  "schema_version": 1,
  "models": [
    {"name": "anthropic:claude-sonnet-5", "effective_from": "2025-01-01", "input_per_mtok": 3.0, "output_per_mtok": 15.0},
    {"name": "openai:*", "effective_from": "2025-01-01", "input_per_mtok": 2.5, "output_per_mtok": 10.0}
  ],
  "tools": [
    {"name": "tool:paid_search", "effective_from": "2025-01-01", "per_call_usd": 0.01}
  ],
  "fallback": {"input_per_mtok": 30.0, "output_per_mtok": 60.0}
}`

func date(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("bad test date %q: %v", s, err)
	}
	return tm
}

func TestParseValid(t *testing.T) {
	t.Parallel()

	cat, err := cost.Parse([]byte(validCatalog))
	if err != nil {
		t.Fatalf("Parse valid catalog: %v", err)
	}
	if cat.ModelCount() != 2 {
		t.Errorf("ModelCount = %d, want 2", cat.ModelCount())
	}
	if cat.ToolCount() != 1 {
		t.Errorf("ToolCount = %d, want 1", cat.ToolCount())
	}
	fb, ok := cat.Fallback()
	if !ok || fb.InputPerMTok != 30.0 || fb.OutputPerMTok != 60.0 {
		t.Errorf("Fallback = %+v ok=%v, want {30 60} true", fb, ok)
	}

	at := date(t, "2025-06-01")
	r, src, ok := cat.Lookup("anthropic:claude-sonnet-5", at)
	if !ok || src != cost.SourceExact || r.InputPerMTok != 3.0 || r.OutputPerMTok != 15.0 {
		t.Errorf("exact lookup = %+v src=%v ok=%v, want {3 15} exact true", r, src, ok)
	}
	n, ok := cat.ToolPrice("tool:paid_search", at)
	if !ok || n != int64(0.01*float64(cost.NanoPerUSD)) {
		t.Errorf("ToolPrice = %d ok=%v, want %d true", n, ok, int64(0.01*float64(cost.NanoPerUSD)))
	}
}

func TestParseErrors(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		doc  string
		want string
	}{
		"wrong schema version": {
			`{"schema_version": 2, "fallback": {"input_per_mtok": 1, "output_per_mtok": 1}}`,
			"schema_version",
		},
		"unknown field": {
			`{"schema_version": 1, "bogus": true, "fallback": {"input_per_mtok": 1, "output_per_mtok": 1}}`,
			"unknown field",
		},
		"trailing content": {
			`{"schema_version": 1, "fallback": {"input_per_mtok": 1, "output_per_mtok": 1}} extra`,
			"trailing content",
		},
		"negative rate": {
			`{"schema_version": 1, "models": [{"name": "a:b", "effective_from": "2025-01-01", "input_per_mtok": -1, "output_per_mtok": 1}], "fallback": {"input_per_mtok": 1, "output_per_mtok": 1}}`,
			"must be a non-negative finite number",
		},
		"missing effective_from": {
			`{"schema_version": 1, "models": [{"name": "a:b", "input_per_mtok": 1, "output_per_mtok": 1}], "fallback": {"input_per_mtok": 1, "output_per_mtok": 1}}`,
			"effective_from is required",
		},
		"bad date": {
			`{"schema_version": 1, "models": [{"name": "a:b", "effective_from": "01-01-2025", "input_per_mtok": 1, "output_per_mtok": 1}], "fallback": {"input_per_mtok": 1, "output_per_mtok": 1}}`,
			"invalid date",
		},
		"whitespace in name": {
			`{"schema_version": 1, "models": [{"name": "a b", "effective_from": "2025-01-01", "input_per_mtok": 1, "output_per_mtok": 1}], "fallback": {"input_per_mtok": 1, "output_per_mtok": 1}}`,
			"must not contain whitespace",
		},
		"bad wildcard": {
			`{"schema_version": 1, "models": [{"name": "a:*:b", "effective_from": "2025-01-01", "input_per_mtok": 1, "output_per_mtok": 1}], "fallback": {"input_per_mtok": 1, "output_per_mtok": 1}}`,
			"trailing",
		},
		"model with tool prefix": {
			`{"schema_version": 1, "models": [{"name": "tool:x", "effective_from": "2025-01-01", "input_per_mtok": 1, "output_per_mtok": 1}], "fallback": {"input_per_mtok": 1, "output_per_mtok": 1}}`,
			"reserved \"tool:\" prefix",
		},
		"tool without prefix": {
			`{"schema_version": 1, "tools": [{"name": "search", "effective_from": "2025-01-01", "per_call_usd": 1}], "fallback": {"input_per_mtok": 1, "output_per_mtok": 1}}`,
			"tool:<name>",
		},
		"negative tool price": {
			`{"schema_version": 1, "tools": [{"name": "tool:x", "effective_from": "2025-01-01", "per_call_usd": -1}], "fallback": {"input_per_mtok": 1, "output_per_mtok": 1}}`,
			"per_call_usd must not be negative",
		},
		"duplicate model+date": {
			`{"schema_version": 1, "models": [
				{"name": "a:b", "effective_from": "2025-01-01", "input_per_mtok": 1, "output_per_mtok": 1},
				{"name": "a:b", "effective_from": "2025-01-01", "input_per_mtok": 2, "output_per_mtok": 2}
			], "fallback": {"input_per_mtok": 1, "output_per_mtok": 1}}`,
			"duplicate price",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := cost.Parse([]byte(tc.doc))
			if err == nil {
				t.Fatalf("Parse(%s): expected error", name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Parse(%s) error = %q, want substring %q", name, err, tc.want)
			}
		})
	}
}

func TestParseAllErrorsJoined(t *testing.T) {
	t.Parallel()

	// Two independent problems: both must be reported, not just the first.
	doc := `{"schema_version": 1, "models": [
		{"name": "a b", "effective_from": "2025-01-01", "input_per_mtok": -1, "output_per_mtok": 1}
	], "fallback": {"input_per_mtok": 1, "output_per_mtok": 1}}`
	_, err := cost.Parse([]byte(doc))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "whitespace") || !strings.Contains(err.Error(), "non-negative") {
		t.Errorf("error %q should mention both the name and rate problems", err)
	}
}

func TestParseFallbackOptional(t *testing.T) {
	t.Parallel()

	// A standalone override may omit fallback — Parse allows it, Fallback
	// reports absent.
	cat, err := cost.Parse([]byte(`{"schema_version": 1, "models": [{"name": "a:b", "effective_from": "2025-01-01", "input_per_mtok": 1, "output_per_mtok": 1}]}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, ok := cat.Fallback(); ok {
		t.Error("Fallback should be absent when the document omits it")
	}
}

func TestLoadMutualExclusion(t *testing.T) {
	t.Parallel()

	if _, err := cost.Load(`{"schema_version":1}`, "/some/file"); err == nil {
		t.Fatal("Load with both inline and file: expected error")
	}
}

func TestLoadDefaultsWhenEmpty(t *testing.T) {
	t.Parallel()

	cat, err := cost.Load("", "")
	if err != nil {
		t.Fatalf("Load empty: %v", err)
	}
	def, _ := cost.Default()
	if cat.ModelCount() != def.ModelCount() {
		t.Errorf("Load(\"\",\"\") ModelCount = %d, want defaults %d", cat.ModelCount(), def.ModelCount())
	}
}

func TestLoadFromFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "pricing.json")
	if err := os.WriteFile(path, []byte(validCatalog), 0o600); err != nil {
		t.Fatalf("write temp catalog: %v", err)
	}
	cat, err := cost.Load("", path)
	if err != nil {
		t.Fatalf("Load from file: %v", err)
	}
	// The override's sonnet entry is present, merged onto the defaults.
	if _, _, ok := cat.Lookup("anthropic:claude-sonnet-5", date(t, "2025-06-01")); !ok {
		t.Error("override model missing after Load from file")
	}
}
