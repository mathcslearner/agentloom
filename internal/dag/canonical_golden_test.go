package dag_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// canonicalGoldenPath pins the canonical `dag.Encode` output for every
// decodable fixture in the definition corpus (ticket 17.6). It is the ground
// truth the frontend's `canonicalize` byte-for-byte export parity test consumes
// (web/lib/graphdef/test/canonical.test.ts): a change to the canonical encoding
// fails this on the Go side and the TS parity test on the other. Regenerate
// with UPDATE_GOLDEN=1.
const canonicalGoldenPath = "testdata/canonical.golden.json"

// canonicalCorpus computes the canonical encoding for every fixture that decodes
// cleanly, keyed by the repo-root-relative path the frontend loader also uses.
// Decode failures (testdata/invalid) are skipped — there is nothing to encode.
func canonicalCorpus(t *testing.T) map[string]string {
	t.Helper()
	dirs := map[string]string{
		filepath.Join("..", "..", "examples", "definitions"): "examples/definitions",
		filepath.Join("testdata", "valid"):                   "internal/dag/testdata/valid",
		filepath.Join("testdata", "invalid"):                 "internal/dag/testdata/invalid",
		filepath.Join("testdata", "invalid_structural"):      "internal/dag/testdata/invalid_structural",
	}
	out := map[string]string{}
	for dir, rel := range dirs {
		files, err := filepath.Glob(filepath.Join(dir, "*.json"))
		if err != nil {
			t.Fatalf("glob %s: %v", dir, err)
		}
		for _, f := range files {
			raw, err := os.ReadFile(f) // #nosec G304 -- committed fixture path, test-only
			if err != nil {
				t.Fatalf("read %s: %v", f, err)
			}
			def, err := dag.Decode(raw)
			if err != nil {
				continue // decode failures have no canonical form
			}
			// Canonical export (ADR-019, ticket 17.6) emits the `ui` block with
			// recursively-sorted, compact keys — the builder owns `ui`, so there
			// is no authored formatting to preserve. `dag.Encode` splices `ui`
			// verbatim (ADR-003), so we normalize `ui` to the canonical export
			// form here before encoding; the frontend's `canonicalize` produces
			// the same sorted-compact `ui`. This stays a fixed point through the
			// backend: `Encode(Decode(golden)) == golden`, since Encode splices
			// the already-canonical `ui` unchanged (asserted below).
			if len(def.UI) > 0 {
				def.UI = canonicalizeUIForGolden(t, def.UI)
			}
			enc, err := dag.Encode(def)
			if err != nil {
				t.Fatalf("encode %s: %v", f, err)
			}
			// Re-decode + re-encode must be a fixed point (canonical form is
			// stable), the invariant the round-trip guarantee rests on.
			def2, err := dag.Decode(enc)
			if err != nil {
				t.Fatalf("re-decode %s: %v", f, err)
			}
			enc2, err := dag.Encode(def2)
			if err != nil {
				t.Fatalf("re-encode %s: %v", f, err)
			}
			if string(enc) != string(enc2) {
				t.Fatalf("canonical form of %s is not a fixed point:\n  first:  %s\n  second: %s", f, enc, enc2)
			}
			out[rel+"/"+filepath.Base(f)] = string(enc)
		}
	}
	return out
}

// canonicalizeUIForGolden renders a `ui` block in the canonical export form:
// sorted, compact, no HTML escaping — matching the frontend's `canonicalize`
// (web/lib/graphdef). Go's default encoder sorts map[string]any keys, and
// SetEscapeHTML(false) leaves `<`/`>`/`&` literal; number formatting matches the
// frontend's `String(n)` for the value ranges a `ui` block carries.
func canonicalizeUIForGolden(t *testing.T, raw json.RawMessage) json.RawMessage {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("ui unmarshal: %v", err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatalf("ui marshal: %v", err)
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n"))
}

// TestCanonicalGolden pins the canonical serialization for the whole corpus. The
// emitted testdata/canonical.golden.json is what the frontend's `canonicalize`
// export must reproduce byte-for-byte. Regenerate with UPDATE_GOLDEN=1.
func TestCanonicalGolden(t *testing.T) {
	got := canonicalCorpus(t)

	blob, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	blob = append(blob, '\n')

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(canonicalGoldenPath, blob, 0o644); err != nil { // #nosec G306 -- committed golden, test-only
			t.Fatalf("write golden: %v", err)
		}
		return
	}

	want, err := os.ReadFile(canonicalGoldenPath)
	if err != nil {
		t.Fatalf("read golden (run UPDATE_GOLDEN=1 to create): %v", err)
	}
	if string(want) != string(blob) {
		var wantMap map[string]string
		_ = json.Unmarshal(want, &wantMap)
		keys := make([]string, 0, len(got))
		for k := range got {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if wantMap[k] != got[k] {
				t.Errorf("canonical drift at %q:\n golden: %s\n got:    %s", k, wantMap[k], got[k])
			}
		}
		t.Fatalf("%s is stale; run UPDATE_GOLDEN=1 go test ./internal/dag -run TestCanonicalGolden", canonicalGoldenPath)
	}
}
