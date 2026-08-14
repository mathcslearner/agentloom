package dag_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// renderConfig is the test harness for one parse+render round trip.
func renderConfig(t *testing.T, config string, data dag.RenderData) (json.RawMessage, error) {
	t.Helper()
	ct, err := dag.ParseConfigTemplates(json.RawMessage(config))
	if err != nil {
		t.Fatalf("ParseConfigTemplates(%s): %v", config, err)
	}
	return ct.Render(data)
}

// mustRender fails the test on any render error and returns the result.
func mustRender(t *testing.T, config string, data dag.RenderData) string {
	t.Helper()
	out, err := renderConfig(t, config, data)
	if err != nil {
		t.Fatalf("Render(%s): %v", config, err)
	}
	return string(out)
}

// jsonEq compares two JSON documents structurally.
func jsonEq(t *testing.T, want, got string) {
	t.Helper()
	var w, g any
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want is not valid JSON: %v\n%s", err, want)
	}
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("got is not valid JSON: %v\n%s", err, got)
	}
	wb, _ := json.Marshal(w)
	gb, _ := json.Marshal(g)
	if string(wb) != string(gb) {
		t.Errorf("JSON mismatch:\nwant: %s\ngot:  %s", wb, gb)
	}
}

func out(pairs ...string) map[string]json.RawMessage {
	m := make(map[string]json.RawMessage, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		m[pairs[i]] = json.RawMessage(pairs[i+1])
	}
	return m
}

func TestRenderPassThrough(t *testing.T) {
	t.Parallel()

	// No templates: Render must return the original bytes unchanged, key
	// order and formatting included.
	src := `{"b": 1, "a": {"x": [true, null]}, "note": "{{ not a template }}"}`
	got := mustRender(t, src, dag.RenderData{})
	if got != src {
		t.Errorf("pass-through changed bytes:\nwant: %s\ngot:  %s", src, got)
	}
}

func TestRenderParamsAndOutputs(t *testing.T) {
	t.Parallel()

	got := mustRender(t, `{
		"greeting": "${{ run.params.greeting }}",
		"line": "${{ steps.compose.output.text }} to ${{ steps.compose.output.audience.name }}!"
	}`, dag.RenderData{
		Params:  json.RawMessage(`{"greeting": "hello"}`),
		Outputs: out("compose", `{"text": "welcome", "audience": {"name": "world"}}`),
	})
	jsonEq(t, `{"greeting": "hello", "line": "welcome to world!"}`, got)
}

func TestRenderNestedPathsAndArrayIndices(t *testing.T) {
	t.Parallel()

	got := mustRender(t, `{"pick": "${{ steps.a.output.items.1.name }}"}`, dag.RenderData{
		Outputs: out("a", `{"items": [{"name": "first"}, {"name": "second"}]}`),
	})
	jsonEq(t, `{"pick": "second"}`, got)
}

func TestRenderWholeExpressionPreservesTypes(t *testing.T) {
	t.Parallel()

	got := mustRender(t, `{
		"obj": "${{ steps.a.output }}",
		"arr": "${{ steps.a.output.items }}",
		"num": "${{ steps.a.output.count }}",
		"big": "${{ run.params.n }}",
		"bool": "${{ steps.a.output.ok }}",
		"null": "${{ steps.a.output.nothing }}",
		"spaced": "  ${{ steps.a.output.count }}  "
	}`, dag.RenderData{
		Params:  json.RawMessage(`{"n": 9007199254740993}`),
		Outputs: out("a", `{"items": [1, 2], "count": 42, "ok": true, "nothing": null}`),
	})
	jsonEq(t, `{
		"obj": {"items": [1, 2], "count": 42, "ok": true, "nothing": null},
		"arr": [1, 2],
		"num": 42,
		"big": 9007199254740993,
		"bool": true,
		"null": null,
		"spaced": 42
	}`, got)
}

func TestRenderMixedInterpolationFormatsValues(t *testing.T) {
	t.Parallel()

	got := mustRender(t, `{"s": "n=${{ steps.a.output.count }} ok=${{ steps.a.output.ok }} obj=${{ steps.a.output.obj }} txt=${{ steps.a.output.txt }}"}`,
		dag.RenderData{Outputs: out("a", `{"count": 3, "ok": false, "obj": {"k": "v"}, "txt": "plain"}`)})
	jsonEq(t, `{"s": "n=3 ok=false obj={\"k\":\"v\"} txt=plain"}`, got)
}

func TestRenderFuncs(t *testing.T) {
	t.Parallel()

	got := mustRender(t, `{
		"fallback": "${{ get 'steps.a.output.missing' | default 'none' }}",
		"present": "${{ get 'steps.a.output.txt' | default 'none' }}",
		"escaped": "${{ get 'steps.a.output.missing' | default 'it\\'s fine' }}",
		"json": "${{ steps.a.output.obj | toJson }}",
		"cut": "${{ steps.a.output.txt | truncate 3 }}"
	}`, dag.RenderData{Outputs: out("a", `{"txt": "abcdef", "obj": {"k": "<v>"}}`)})
	// toJson must not HTML-escape (the "<v>" survives).
	jsonEq(t, `{"fallback": "none", "present": "abcdef", "escaped": "it's fine", "json": "{\"k\":\"<v>\"}", "cut": "abc"}`, got)
}

func TestRenderStrictMissingRef(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		config string
		data   dag.RenderData
	}{
		"unknown step output": {
			config: `{"x": "${{ steps.ghost.output }}"}`,
			data:   dag.RenderData{Outputs: out("a", `{}`)},
		},
		"missing path": {
			config: `{"x": "${{ steps.a.output.nope }}"}`,
			data:   dag.RenderData{Outputs: out("a", `{"yep": 1}`)},
		},
		"index out of range": {
			config: `{"x": "${{ steps.a.output.items.5 }}"}`,
			data:   dag.RenderData{Outputs: out("a", `{"items": [1]}`)},
		},
		"descend into scalar": {
			config: `{"x": "${{ steps.a.output.n.deeper }}"}`,
			data:   dag.RenderData{Outputs: out("a", `{"n": 7}`)},
		},
		"missing param": {
			config: `{"x": "${{ run.params.absent }}"}`,
			data:   dag.RenderData{Params: json.RawMessage(`{"present": 1}`)},
		},
		"no params at all": {
			config: `{"x": "${{ run.params.greeting }}"}`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := renderConfig(t, tc.config, tc.data)
			var mre *dag.MissingRefError
			if !errors.As(err, &mre) {
				t.Fatalf("want *MissingRefError, got %v", err)
			}
		})
	}
}

func TestRenderStepOutputNullIsResolvable(t *testing.T) {
	t.Parallel()

	// A recorded null output is a value, not a missing reference; only
	// descending into it fails.
	got := mustRender(t, `{"x": "${{ steps.a.output }}"}`,
		dag.RenderData{Outputs: out("a", `null`)})
	jsonEq(t, `{"x": null}`, got)

	_, err := renderConfig(t, `{"x": "${{ steps.a.output.field }}"}`,
		dag.RenderData{Outputs: out("a", `null`)})
	var mre *dag.MissingRefError
	if !errors.As(err, &mre) {
		t.Fatalf("want *MissingRefError descending into null, got %v", err)
	}
}

func TestRenderInjectionIsInert(t *testing.T) {
	t.Parallel()

	// Values arriving through outputs and params are data: a template
	// expression inside them must come out verbatim, never re-rendered.
	payload := `{"attack": "${{ run.params.secret }} {{ printf }} ` + "`" + `raw` + "`" + `"}`
	got := mustRender(t, `{"x": "${{ steps.evil.output.attack }}", "y": "literal: ${{ steps.evil.output.attack }}"}`,
		dag.RenderData{Outputs: out("evil", payload)})
	want := `${{ run.params.secret }} {{ printf }} ` + "`raw`"
	var decoded struct{ X, Y string }
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("decoding rendered config: %v", err)
	}
	if decoded.X != want {
		t.Errorf("whole-expression injection not inert:\nwant %q\ngot  %q", want, decoded.X)
	}
	if decoded.Y != "literal: "+want {
		t.Errorf("interpolated injection not inert:\nwant %q\ngot  %q", "literal: "+want, decoded.Y)
	}
}

func TestRenderRawDelimitersOutsideTemplatesAreInert(t *testing.T) {
	t.Parallel()

	// Standard Go template delimiters and function names have no meaning:
	// only ${{ ... }} is an expression.
	src := `{"a": "{{ if true }}not a template{{ end }}", "b": "close }} alone"}`
	got := mustRender(t, src, dag.RenderData{})
	if got != src {
		t.Errorf("non-${{ }} text was interpreted:\nwant: %s\ngot:  %s", src, got)
	}
}

func TestParseErrors(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		config   string
		wantRef  bool // expect a *TemplateRefError (vs a *TemplateError)
		contains string
	}{
		"unterminated expression": {
			config:   `{"x": "${{ steps.a.output"}`,
			contains: "unterminated template expression",
		},
		"control structure": {
			config:   `{"x": "${{ if steps.a.output }}yes${{ end }}"}`,
			contains: "control structures",
		},
		"variable declaration": {
			config:   `{"x": "${{ $v := 1 }}"}`,
			contains: "variables are not supported",
		},
		"unknown function": {
			config:   `{"x": "${{ printf 'boom' }}"}`,
			contains: "unknown function",
		},
		"builtin escape hatch blocked": {
			config:   `{"x": "${{ call get 'steps.a.output' }}"}`,
			contains: "unknown function",
		},
		"unknown root": {
			config:   `{"x": "${{ secrets.api_key }}"}`,
			wantRef:  true,
			contains: "unknown reference root",
		},
		"step ref without output": {
			config:   `{"x": "${{ steps.a.result }}"}`,
			wantRef:  true,
			contains: "steps.<id>.output",
		},
		"bare steps root": {
			config:   `{"x": "${{ steps }}"}`,
			wantRef:  true,
			contains: "steps.<id>.output",
		},
		"run without params": {
			config:   `{"x": "${{ run.settings.x }}"}`,
			wantRef:  true,
			contains: "run.params",
		},
		"empty segment": {
			config:   `{"x": "${{ steps.a.output. }}"}`,
			wantRef:  true,
			contains: "empty path segment",
		},
		"bad step id": {
			config:   `{"x": "${{ steps.Not-Valid.output }}"}`,
			wantRef:  true,
			contains: "does not match",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := dag.ParseConfigTemplates(json.RawMessage(tc.config))
			if err == nil {
				t.Fatalf("want parse error, got none")
			}
			var te *dag.TemplateError
			var re *dag.TemplateRefError
			switch {
			case tc.wantRef && !errors.As(err, &re):
				t.Errorf("want *TemplateRefError, got %v", err)
			case !tc.wantRef && !errors.As(err, &te):
				t.Errorf("want *TemplateError, got %v", err)
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.contains)
			}
		})
	}
}

func TestParseReportsEveryProblemWithPaths(t *testing.T) {
	t.Parallel()

	_, err := dag.ParseConfigTemplates(json.RawMessage(`{
		"one": "${{ secrets.x }}",
		"nested": {"two": "${{ steps.a.result }}"},
		"arr": ["${{ steps.b.output", "fine"]
	}`))
	if err == nil {
		t.Fatal("want parse errors, got none")
	}
	msg := err.Error()
	for _, want := range []string{"one", "nested.two", "arr[0]"} {
		if !strings.Contains(msg, want) {
			t.Errorf("joined error %q does not mention path %q", msg, want)
		}
	}
}

func TestRefsAndStepIDs(t *testing.T) {
	t.Parallel()

	ct, err := dag.ParseConfigTemplates(json.RawMessage(`{
		"a": "${{ steps.zeta.output.x }} and ${{ steps.alpha.output }}",
		"b": "${{ run.params.key }}",
		"c": "${{ get 'steps.lenient.output.maybe' | default 1 }}",
		"d": "${{ steps.zeta.output.y }}"
	}`))
	if err != nil {
		t.Fatalf("ParseConfigTemplates: %v", err)
	}
	ids := ct.StepIDs()
	want := []string{"alpha", "lenient", "zeta"}
	if len(ids) != len(want) {
		t.Fatalf("StepIDs: want %v, got %v", want, ids)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("StepIDs: want %v, got %v", want, ids)
		}
	}

	var lenient, strict, param int
	for _, r := range ct.Refs() {
		switch {
		case r.Lenient:
			lenient++
			if r.StepID != "lenient" {
				t.Errorf("lenient ref has StepID %q", r.StepID)
			}
		case r.ParamKey != "":
			param++
			if r.ParamKey != "key" {
				t.Errorf("param ref has key %q", r.ParamKey)
			}
		case r.StepID != "":
			strict++
		}
	}
	if lenient != 1 || strict != 3 || param != 1 {
		t.Errorf("ref counts: lenient=%d strict=%d param=%d, want 1/3/1", lenient, strict, param)
	}
}

func TestRenderDeterministic(t *testing.T) {
	t.Parallel()

	config := `{"x": "${{ steps.a.output.v }}", "y": "${{ run.params.p }} tail"}`
	data := dag.RenderData{
		Params:  json.RawMessage(`{"p": 1.5}`),
		Outputs: out("a", `{"v": {"nested": [1, 2, 3]}}`),
	}
	first := mustRender(t, config, data)
	for i := 0; i < 5; i++ {
		if got := mustRender(t, config, data); got != first {
			t.Fatalf("render not deterministic:\nfirst: %s\ngot:   %s", first, got)
		}
	}
}

func TestHasTemplate(t *testing.T) {
	t.Parallel()

	if dag.HasTemplate("plain {{ x }} text") {
		t.Error("HasTemplate false positive on {{ }}")
	}
	if !dag.HasTemplate(`prefix ${{ run.params.x }}`) {
		t.Error("HasTemplate false negative")
	}
}
