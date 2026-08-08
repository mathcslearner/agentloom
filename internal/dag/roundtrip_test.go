package dag_test

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// rawTopLevelField extracts the verbatim bytes of one top-level field from
// a definition document.
func rawTopLevelField(t *testing.T, doc []byte, key string) json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(doc, &m); err != nil {
		t.Fatalf("unmarshaling document: %v", err)
	}
	return m[key]
}

// assertLosslessRoundTrip asserts the codec contract on one source
// document: decode→encode→decode is lossless, encoding is deterministic,
// and the ui block survives byte-for-byte. Shared by the testdata corpus
// (ticket 1.2) and the examples/definitions corpus (ticket 1.6).
func assertLosslessRoundTrip(t *testing.T, data []byte) {
	t.Helper()

	d1, err := dag.Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	out1, err := dag.Encode(d1)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !json.Valid(out1) {
		t.Fatalf("Encode produced invalid JSON:\n%s", out1)
	}
	d2, err := dag.Decode(out1)
	if err != nil {
		t.Fatalf("Decode of encoded output: %v\noutput:\n%s", err, out1)
	}
	if !reflect.DeepEqual(d1, d2) {
		t.Errorf("round trip not lossless:\nfirst decode:  %#v\nsecond decode: %#v", d1, d2)
	}
	out2, err := dag.Encode(d2)
	if err != nil {
		t.Fatalf("second Encode: %v", err)
	}
	if !bytes.Equal(out1, out2) {
		t.Errorf("encoding is not deterministic:\nfirst:  %s\nsecond: %s", out1, out2)
	}

	wantUI := rawTopLevelField(t, data, "ui")
	if !bytes.Equal(d1.UI, wantUI) {
		t.Errorf("decoded ui differs from source:\nwant: %s\ngot:  %s", wantUI, d1.UI)
	}
	gotUI := rawTopLevelField(t, out1, "ui")
	if !bytes.Equal(gotUI, wantUI) {
		t.Errorf("encoded ui not byte-for-byte:\nwant: %s\ngot:  %s", wantUI, gotUI)
	}
}

// TestRoundTrip is the ticket 1.2 acceptance test over the valid testdata
// corpus.
func TestRoundTrip(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob(filepath.Join("testdata", "valid", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("globbing valid fixtures: %v (found %d)", err, len(files))
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			t.Parallel()
			assertLosslessRoundTrip(t, readFixture(t, "valid", filepath.Base(f)))
		})
	}
}

// TestEncodeDoesNotHTMLEscape verifies CEL expressions and payload strings
// survive encoding without \u003c-style mangling.
func TestEncodeDoesNotHTMLEscape(t *testing.T) {
	t.Parallel()

	def, err := dag.Decode(readFixture(t, "valid", "kitchen_sink.json"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	out, err := dag.Encode(def)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !bytes.Contains(out, []byte("x < 3 & y > 2")) {
		t.Errorf("encoded output lost the literal 'x < 3 & y > 2'")
	}
	for _, esc := range []string{`\u003c`, `\u003e`, `\u0026`} {
		if bytes.Contains(out, []byte(esc)) {
			t.Errorf("encoded output contains HTML escape %s:\n%s", esc, out)
		}
	}
}

// TestEncodeCanonicalSpellings verifies the canonical encoding omits
// default spellings (edge type "normal", required:false) that the fixture
// wrote out explicitly.
func TestEncodeCanonicalSpellings(t *testing.T) {
	t.Parallel()

	def, err := dag.Decode(readFixture(t, "valid", "explicit_defaults.json"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	out, err := dag.Encode(def)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	for _, absent := range []string{`"normal"`, `"required":false`} {
		if bytes.Contains(out, []byte(absent)) {
			t.Errorf("canonical encoding should omit %s:\n%s", absent, out)
		}
	}
	if !bytes.Contains(out, []byte(`"temperature":0`)) {
		t.Errorf("canonical encoding dropped explicit temperature 0:\n%s", out)
	}
}

func TestEncodeNilDefinition(t *testing.T) {
	t.Parallel()

	if _, err := dag.Encode(nil); err == nil {
		t.Error("Encode(nil): want error, got nil")
	}
}

func TestEncodeInvalidUI(t *testing.T) {
	t.Parallel()

	def := &dag.Definition{
		SchemaVersion: dag.CurrentSchemaVersion,
		Name:          "x",
		Steps:         []dag.Step{{ID: "a", Type: dag.StepNoop}},
		UI:            json.RawMessage(`{"broken":`),
	}
	if _, err := dag.Encode(def); err == nil {
		t.Error("Encode with invalid ui bytes: want error, got nil")
	}
}
