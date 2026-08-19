package loadtest_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/loadtest"
)

// scenarioDir is the committed scenario corpus (ticket 19.1). Keeping the
// tree loadable and every definition well-formed is what makes the
// scenarios "runnable as named configs" checkable in CI before the load
// generator (19.2) exists.
func scenarioDir(t *testing.T) string {
	t.Helper()
	return filepath.Join("..", "..", "test", "load", "scenarios")
}

// TestScenarioCorpusLoads: the committed scenario tree parses, every
// referenced definition decodes and validates, and every mix entry
// resolves — the DoD "scenario definitions runnable as named configs".
func TestScenarioCorpusLoads(t *testing.T) {
	t.Parallel()
	scenarios, err := loadtest.LoadDir(scenarioDir(t))
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	want := map[string]bool{"linear-10": false, "fanout-50": false, "planner-heavy": false, "agent-loop": false, "mixed": false}
	for _, s := range scenarios {
		if _, ok := want[s.Name]; !ok {
			t.Errorf("unexpected scenario %q", s.Name)
			continue
		}
		want[s.Name] = true
		if s.Duration.D() <= 0 {
			t.Errorf("%s: non-positive duration", s.Name)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("scenario %q missing from corpus", name)
		}
	}
	// Sorted by name.
	for i := 1; i < len(scenarios); i++ {
		if scenarios[i-1].Name > scenarios[i].Name {
			t.Errorf("scenarios not sorted: %q before %q", scenarios[i-1].Name, scenarios[i].Name)
		}
	}
}

// TestParseValidConstant / ramp / mix cover the three arrival shapes.
func TestParseArrivalShapes(t *testing.T) {
	t.Parallel()
	cases := map[string][]byte{
		"constant": []byte(`{"schema_version":1,"name":"c","definition":"d.json","arrival":{"mode":"constant","rate_per_sec":10},"duration":"5m"}`),
		"ramp":     []byte(`{"schema_version":1,"name":"r","definition":"d.json","arrival":{"mode":"ramp","ramp":{"from_per_sec":1,"to_per_sec":10,"step_per_sec":1,"step_duration":"30s"}},"duration":"5m"}`),
		"mix":      []byte(`{"schema_version":1,"name":"m","arrival":{"mode":"constant","rate_per_sec":10},"duration":"5m","mix":[{"scenario":"a","weight":0.5},{"scenario":"b","weight":0.5}]}`),
	}
	for name, data := range cases {
		s, err := loadtest.Parse(data)
		if err != nil {
			t.Errorf("%s: Parse: %v", name, err)
			continue
		}
		if s.Duration.D() != 5*time.Minute {
			t.Errorf("%s: duration = %s, want 5m", name, s.Duration.D())
		}
	}
}

// TestParseRejects covers the shape-validation failure modes.
func TestParseRejects(t *testing.T) {
	t.Parallel()
	bad := map[string][]byte{
		"unknown field":     []byte(`{"schema_version":1,"name":"x","definition":"d.json","arrival":{"mode":"constant","rate_per_sec":1},"duration":"1m","bogus":true}`),
		"wrong version":     []byte(`{"schema_version":2,"name":"x","definition":"d.json","arrival":{"mode":"constant","rate_per_sec":1},"duration":"1m"}`),
		"bad name":          []byte(`{"schema_version":1,"name":"Bad_Name","definition":"d.json","arrival":{"mode":"constant","rate_per_sec":1},"duration":"1m"}`),
		"def and mix":       []byte(`{"schema_version":1,"name":"x","definition":"d.json","arrival":{"mode":"constant","rate_per_sec":1},"duration":"1m","mix":[{"scenario":"a","weight":1}]}`),
		"neither def":       []byte(`{"schema_version":1,"name":"x","arrival":{"mode":"constant","rate_per_sec":1},"duration":"1m"}`),
		"constant no rate":  []byte(`{"schema_version":1,"name":"x","definition":"d.json","arrival":{"mode":"constant"},"duration":"1m"}`),
		"ramp no block":     []byte(`{"schema_version":1,"name":"x","definition":"d.json","arrival":{"mode":"ramp"},"duration":"1m"}`),
		"bad mode":          []byte(`{"schema_version":1,"name":"x","definition":"d.json","arrival":{"mode":"burst"},"duration":"1m"}`),
		"zero duration":     []byte(`{"schema_version":1,"name":"x","definition":"d.json","arrival":{"mode":"constant","rate_per_sec":1},"duration":"0s"}`),
		"mix weights wrong": []byte(`{"schema_version":1,"name":"x","arrival":{"mode":"constant","rate_per_sec":1},"duration":"1m","mix":[{"scenario":"a","weight":0.3},{"scenario":"b","weight":0.3}]}`),
		"mix dup":           []byte(`{"schema_version":1,"name":"x","arrival":{"mode":"constant","rate_per_sec":1},"duration":"1m","mix":[{"scenario":"a","weight":0.5},{"scenario":"a","weight":0.5}]}`),
	}
	for name, data := range bad {
		if _, err := loadtest.Parse(data); err == nil {
			t.Errorf("%s: Parse accepted an invalid scenario", name)
		}
	}
}

// TestLoadDirRejectsMissingDefinition: a scenario referencing a
// non-existent definition file fails LoadDir.
func TestLoadDirRejectsMissingDefinition(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "s.json"),
		`{"schema_version":1,"name":"s","definition":"nope.json","arrival":{"mode":"constant","rate_per_sec":1},"duration":"1m"}`)
	if _, err := loadtest.LoadDir(dir); err == nil {
		t.Error("LoadDir accepted a scenario with a missing definition file")
	}
}

// TestLoadDirRejectsUnknownMixRef: a mix referencing a scenario not in the
// directory fails LoadDir.
func TestLoadDirRejectsUnknownMixRef(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "m.json"),
		`{"schema_version":1,"name":"m","arrival":{"mode":"constant","rate_per_sec":1},"duration":"1m","mix":[{"scenario":"ghost","weight":1}]}`)
	if _, err := loadtest.LoadDir(dir); err == nil {
		t.Error("LoadDir accepted a mix referencing an unknown scenario")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
