package jsonrepair_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mathcslearner/agentloom/internal/jsonrepair"
)

func TestRepairTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		in     string
		status jsonrepair.Status
		value  string
		steps  []string
	}{
		{"valid_object", `{"a":1}`, jsonrepair.StatusRaw, `{"a":1}`, nil},
		{"valid_with_whitespace", "  { \"a\" : 1 }  ", jsonrepair.StatusRaw, `{"a":1}`, nil},
		{"fence_json", "```json\n{\"a\":1}\n```", jsonrepair.StatusRepaired, `{"a":1}`, []string{jsonrepair.StepStripCodeFence}},
		{"fence_bare", "```\n[1,2]\n```", jsonrepair.StatusRepaired, `[1,2]`, []string{jsonrepair.StepStripCodeFence}},
		{"prose_wrapped", "Here: {\"a\":1} done", jsonrepair.StatusRepaired, `{"a":1}`, []string{jsonrepair.StepExtractFirstJSON}},
		{"trailing_comma", `{"a":1,}`, jsonrepair.StatusRepaired, `{"a":1}`, []string{jsonrepair.StepTrailingCommas}},
		{"unquoted_keys", `{a: 1, b: 2}`, jsonrepair.StatusRepaired, `{"a":1,"b":2}`, []string{jsonrepair.StepUnquotedKeys}},
		{"combined", "```json\n{a: 1, b: [1, 2,],}\n```", jsonrepair.StatusRepaired, `{"a":1,"b":[1,2]}`, []string{jsonrepair.StepStripCodeFence, jsonrepair.StepTrailingCommas, jsonrepair.StepUnquotedKeys}},
		{"unrepairable", "no json here", jsonrepair.StatusUnrepairable, "", nil},
		{"unrepairable_truncated", `{"a":[1,2`, jsonrepair.StatusUnrepairable, "", nil},
		{"comma_in_string_safe", `{"s":"a,b,"}`, jsonrepair.StatusRaw, `{"s":"a,b,"}`, nil},
		{"brace_in_string_safe", `x {"s":"{y}"} z`, jsonrepair.StatusRepaired, `{"s":"{y}"}`, []string{jsonrepair.StepExtractFirstJSON}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := jsonrepair.Repair(tt.in)
			if got.Status != tt.status {
				t.Fatalf("status = %q, want %q (steps %v)", got.Status, tt.status, got.Steps)
			}
			if tt.status == jsonrepair.StatusUnrepairable {
				if got.Value != nil {
					t.Fatalf("unrepairable result carries value %q", got.Value)
				}
				return
			}
			if string(got.Value) != tt.value {
				t.Fatalf("value = %q, want %q", got.Value, tt.value)
			}
			if !json.Valid(got.Value) {
				t.Fatalf("repaired value is not valid JSON: %q", got.Value)
			}
			if !equalSteps(got.Steps, tt.steps) {
				t.Fatalf("steps = %v, want %v", got.Steps, tt.steps)
			}
		})
	}
}

// TestRepairIdempotent asserts a repaired value re-repairs to itself (raw,
// no steps) — the output is a stable fixed point.
func TestRepairIdempotent(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"```json\n{a:1,}\n```", `{"x":[1,2,3,]}`, "prose {\"k\":\"v\"} more"} {
		first := jsonrepair.Repair(in)
		if first.Status == jsonrepair.StatusUnrepairable {
			t.Fatalf("%q unexpectedly unrepairable", in)
		}
		second := jsonrepair.Repair(string(first.Value))
		if second.Status != jsonrepair.StatusRaw || len(second.Steps) != 0 {
			t.Fatalf("re-repair of %q = %q %v, want raw with no steps", first.Value, second.Status, second.Steps)
		}
		if string(second.Value) != string(first.Value) {
			t.Fatalf("re-repair changed the value: %q -> %q", first.Value, second.Value)
		}
	}
}

// corpusFile mirrors testdata/corpus.json.
type corpusFile struct {
	Cases []struct {
		Name   string   `json:"name"`
		Input  string   `json:"input"`
		Status string   `json:"status"`
		Value  string   `json:"value"`
		Steps  []string `json:"steps"`
	} `json:"cases"`
}

// TestCorpusRepairRate drives the messy-output corpus and asserts (a) each
// case's exact outcome and (b) the aggregate repair rate — the fixture-driven
// measurement the 11.3 acceptance criterion requires. The rate is
// (raw+repaired)/total; the corpus deliberately includes unrepairable cases,
// so the asserted rate is an exact fraction, not 100%.
func TestCorpusRepairRate(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "corpus.json"))
	if err != nil {
		t.Fatalf("reading corpus: %v", err)
	}
	var corpus corpusFile
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("decoding corpus: %v", err)
	}
	if len(corpus.Cases) == 0 {
		t.Fatal("corpus is empty")
	}

	recovered := 0
	for _, c := range corpus.Cases {
		got := jsonrepair.Repair(c.Input)
		if string(got.Status) != c.Status {
			t.Errorf("%s: status = %q, want %q (steps %v)", c.Name, got.Status, c.Status, got.Steps)
			continue
		}
		switch got.Status {
		case jsonrepair.StatusRaw, jsonrepair.StatusRepaired:
			recovered++
			if string(got.Value) != c.Value {
				t.Errorf("%s: value = %q, want %q", c.Name, got.Value, c.Value)
			}
			if !equalSteps(got.Steps, c.Steps) {
				t.Errorf("%s: steps = %v, want %v", c.Name, got.Steps, c.Steps)
			}
		case jsonrepair.StatusUnrepairable:
			if got.Value != nil {
				t.Errorf("%s: unrepairable carries value %q", c.Name, got.Value)
			}
		}
	}

	// The corpus is fixed, so the repair rate is an exact, asserted number:
	// 14 of 17 cases recover (three are deliberately unrepairable).
	const wantRecovered = 14
	if recovered != wantRecovered {
		t.Fatalf("repair rate: recovered %d/%d cases, want %d/%d (%.0f%%)",
			recovered, len(corpus.Cases), wantRecovered, len(corpus.Cases),
			100*float64(wantRecovered)/float64(len(corpus.Cases)))
	}
}

// FuzzRepair asserts the two invariants that must hold for any input: Repair
// never panics, and a non-unrepairable result is always valid JSON.
func FuzzRepair(f *testing.F) {
	for _, seed := range []string{
		"", "{", "}", "```", "```json\n{}\n```", `{a:1,}`, "null", `[1,2,3,]`,
		"prose {\"k\":\"v,\"} tail", `{"nested":{"a":[1,{b:2,},]}}`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		got := jsonrepair.Repair(in)
		if got.Status != jsonrepair.StatusUnrepairable && !json.Valid(got.Value) {
			t.Fatalf("non-unrepairable result is invalid JSON: status=%q value=%q", got.Status, got.Value)
		}
	})
}

func equalSteps(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
