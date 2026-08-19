package dag_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// verdictsGoldenPath is the shared verdict corpus (ticket 17.4): the exact
// Decode+Validate verdict for every fixture in the definition corpus, emitted
// here and consumed by the frontend's client-side validator parity test
// (web/lib/graphdef) so a client verdict and a server verdict name the same
// problem in the same place. Regenerate with UPDATE_GOLDEN=1.
const verdictsGoldenPath = "testdata/verdicts.golden.json"

// wireIssue mirrors the API's Issue wire shape (internal/api/types.go) without
// importing internal/api (which imports dag — a cycle). Kept field-compatible
// so the golden is exactly what a 400 body's issues[] carries.
type wireIssue struct {
	Code     string `json:"code,omitempty"`
	Severity string `json:"severity"`
	Path     string `json:"path,omitempty"`
	Msg      string `json:"msg"`
}

// fixtureVerdict is one fixture's outcome: either a decode failure (codeless
// issues) or, when decode succeeds, the full Validate findings (warnings
// included), matching internal/api's decodeAndValidate flow exactly.
type fixtureVerdict struct {
	// Decode is non-nil iff the fixture failed the codec layer; its issues are
	// codeless {path,msg} (a *dag.DecodeError carries no ValidationCode).
	Decode []wireIssue `json:"decode,omitempty"`
	// Issues is the full Validate findings list (warnings included), present iff
	// Decode succeeded. Empty slice = a fully valid definition.
	Issues []wireIssue `json:"issues"`
	// DecodeFailed disambiguates a decode failure from a clean decode with an
	// empty issues list.
	DecodeFailed bool `json:"decode_failed"`
}

// flattenDecodeErr walks the errors.Join tree of a dag.Decode error and
// collects every *dag.DecodeError leaf as a codeless wire issue — the same
// walk internal/api.DefinitionIssues does, replicated here to avoid the cycle.
func flattenDecodeErr(err error) []wireIssue {
	var out []wireIssue
	var walk func(error)
	walk = func(err error) {
		if err == nil {
			return
		}
		switch e := err.(type) { //nolint:errorlint // walking the tree, not matching one target
		case interface{ Unwrap() []error }:
			for _, sub := range e.Unwrap() {
				walk(sub)
			}
		case *dag.DecodeError:
			out = append(out, wireIssue{Severity: string(dag.SeverityError), Path: e.Path, Msg: e.Msg})
		default:
			if sub := errors.Unwrap(err); sub != nil {
				walk(sub)
				return
			}
			out = append(out, wireIssue{Severity: string(dag.SeverityError), Msg: err.Error()})
		}
	}
	walk(err)
	return out
}

// verdictFor computes one fixture's verdict from raw JSON, mirroring the API
// flow: Decode; on failure return the codeless issues; else Validate and
// return the full findings.
func verdictFor(raw []byte) fixtureVerdict {
	def, err := dag.Decode(raw)
	if err != nil {
		return fixtureVerdict{Decode: flattenDecodeErr(err), Issues: []wireIssue{}, DecodeFailed: true}
	}
	found, _ := dag.Validate(def)
	issues := make([]wireIssue, 0, len(found))
	for _, i := range found {
		issues = append(issues, wireIssue{
			Code: string(i.Code), Severity: string(i.Severity), Path: i.Path, Msg: i.Msg,
		})
	}
	return fixtureVerdict{Issues: issues}
}

// corpusVerdicts computes the verdict for every fixture in the shared corpus,
// keyed by the repo-root-relative path the frontend loader also uses.
func corpusVerdicts(t *testing.T) map[string]fixtureVerdict {
	t.Helper()
	dirs := []string{
		filepath.Join("..", "..", "examples", "definitions"),
		filepath.Join("testdata", "valid"),
		filepath.Join("testdata", "invalid"),
		filepath.Join("testdata", "invalid_structural"),
	}
	rel := map[string]string{
		filepath.Join("..", "..", "examples", "definitions"): "examples/definitions",
		filepath.Join("testdata", "valid"):                   "internal/dag/testdata/valid",
		filepath.Join("testdata", "invalid"):                 "internal/dag/testdata/invalid",
		filepath.Join("testdata", "invalid_structural"):      "internal/dag/testdata/invalid_structural",
	}
	out := map[string]fixtureVerdict{}
	for _, dir := range dirs {
		files, err := filepath.Glob(filepath.Join(dir, "*.json"))
		if err != nil {
			t.Fatalf("glob %s: %v", dir, err)
		}
		for _, f := range files {
			raw, err := os.ReadFile(f) // #nosec G304 -- committed fixture path, test-only
			if err != nil {
				t.Fatalf("read %s: %v", f, err)
			}
			key := rel[dir] + "/" + filepath.Base(f)
			out[key] = verdictFor(raw)
		}
	}
	return out
}

// TestVerdictsGolden pins the Decode+Validate verdict for the whole corpus.
// The emitted testdata/verdicts.golden.json is the ground truth the frontend's
// graphdef config-validator parity test compares against (17.4/17.5): a Go
// change to a message, code, or path fails this on one side and the TS parity
// test on the other. Regenerate with UPDATE_GOLDEN=1.
func TestVerdictsGolden(t *testing.T) {
	got := corpusVerdicts(t)

	// Marshal deterministically: sorted fixture keys (encoding/json sorts map
	// keys), issue arrays preserve Validate's deterministic order.
	blob, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	blob = append(blob, '\n')

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(verdictsGoldenPath, blob, 0o644); err != nil { // #nosec G306 -- committed golden, test-only
			t.Fatalf("write golden: %v", err)
		}
		return
	}

	want, err := os.ReadFile(verdictsGoldenPath)
	if err != nil {
		t.Fatalf("read golden (run UPDATE_GOLDEN=1 to create): %v", err)
	}
	if string(want) != string(blob) {
		// Point at the first differing fixture for a legible failure.
		var wantMap map[string]json.RawMessage
		_ = json.Unmarshal(want, &wantMap)
		keys := make([]string, 0, len(got))
		for k := range got {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			gv, _ := json.Marshal(got[k])
			if string(wantMap[k]) != string(gv) {
				t.Errorf("verdict drift at %q:\n golden: %s\n got:    %s", k, wantMap[k], gv)
			}
		}
		t.Fatalf("%s is stale; run UPDATE_GOLDEN=1 go test ./internal/dag -run TestVerdictsGolden", verdictsGoldenPath)
	}
}
