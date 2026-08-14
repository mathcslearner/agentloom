package exec

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/plugin"
	"github.com/mathcslearner/agentloom/internal/retrieval"
)

// spyRetriever is a retrieval.Retriever that records the query it saw and
// returns scripted results/error, for retrieve-executor coverage.
type spyRetriever struct {
	name     string
	lastQ    *string
	lastK    *int
	results  []retrieval.ScoredDoc
	queryErr error
}

func (s spyRetriever) Manifest() plugin.Manifest {
	return plugin.Manifest{Kind: plugin.KindRetriever, Name: s.name, Version: "1.0.0"}
}

func (s spyRetriever) Ingest(context.Context, []retrieval.Doc) error { return nil }

func (s spyRetriever) Query(_ context.Context, q string, k int) ([]retrieval.ScoredDoc, error) {
	if s.lastQ != nil {
		*s.lastQ = q
	}
	if s.lastK != nil {
		*s.lastK = k
	}
	return s.results, s.queryErr
}

// retrieveStepConfig builds a retrieve step config selecting retriever with
// query and an optional top_k (top_k omitted when 0-and-string empty).
func retrieveStepConfig(retriever, query string, topK json.RawMessage) json.RawMessage {
	cfg := `{"retriever":"` + retriever + `","query":"` + query + `"`
	if len(topK) > 0 {
		cfg += `,"top_k":` + string(topK)
	}
	return json.RawMessage(cfg + `}`)
}

func runRetrieveExec(t *testing.T, reg *retrieval.Registry, cfg json.RawMessage) (Output, error) {
	t.Helper()
	return NewRetrieveExecutor(reg).Execute(context.Background(), StepContext{
		StepType: dag.StepRetrieve,
		Config:   cfg,
	})
}

func TestRetrieveExecNilRegistryPermanent(t *testing.T) {
	t.Parallel()
	_, err := runRetrieveExec(t, nil, retrieveStepConfig("pg_fulltext", "cats", nil))
	assertClassPermanent(t, err)
}

func TestRetrieveExecUnknownRetrieverPermanent(t *testing.T) {
	t.Parallel()
	reg, _ := retrieval.NewRegistry(spyRetriever{name: "known"})
	_, err := runRetrieveExec(t, reg, retrieveStepConfig("missing", "cats", nil))
	assertClassPermanent(t, err)
	if !errors.Is(err, retrieval.ErrUnknownRetriever) {
		t.Errorf("err = %v, want ErrUnknownRetriever reachable", err)
	}
}

func TestRetrieveExecMissingRetrieverFieldPermanent(t *testing.T) {
	t.Parallel()
	reg, _ := retrieval.NewRegistry(spyRetriever{name: "r"})
	_, err := runRetrieveExec(t, reg, json.RawMessage(`{"query":"cats"}`))
	if !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("err = %v, want ErrInvalidConfig", err)
	}
}

func TestRetrieveExecEmptyRenderedQueryPermanent(t *testing.T) {
	t.Parallel()
	reg, _ := retrieval.NewRegistry(spyRetriever{name: "r"})
	// A template that renders to empty leaves query empty at runtime.
	_, err := runRetrieveExec(t, reg, json.RawMessage(`{"retriever":"r","query":""}`))
	assertClassPermanent(t, err)
}

func TestRetrieveExecNegativeTopKPermanent(t *testing.T) {
	t.Parallel()
	reg, _ := retrieval.NewRegistry(spyRetriever{name: "r"})
	_, err := runRetrieveExec(t, reg, retrieveStepConfig("r", "cats", json.RawMessage(`-1`)))
	assertClassPermanent(t, err)
}

func TestRetrieveExecTopKDefaultAndCap(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		topK    json.RawMessage
		wantK   int
		wantOut int
	}{
		{"absent → default", nil, defaultTopK, defaultTopK},
		{"explicit", json.RawMessage(`3`), 3, 3},
		{"over cap → clamped", json.RawMessage(`9999`), maxTopK, maxTopK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var gotK int
			reg, _ := retrieval.NewRegistry(spyRetriever{name: "r", lastK: &gotK})
			out, err := runRetrieveExec(t, reg, retrieveStepConfig("r", "cats", tc.topK))
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if gotK != tc.wantK {
				t.Errorf("Query k = %d, want %d", gotK, tc.wantK)
			}
			var o retrieveOutput
			if err := json.Unmarshal(out.Data, &o); err != nil {
				t.Fatalf("unmarshaling output: %v", err)
			}
			if o.TopK != tc.wantOut {
				t.Errorf("output top_k = %d, want %d", o.TopK, tc.wantOut)
			}
		})
	}
}

func TestRetrieveExecOutputShape(t *testing.T) {
	t.Parallel()
	results := []retrieval.ScoredDoc{
		{Doc: retrieval.Doc{ID: "d1", Content: "cats are great", Metadata: json.RawMessage(`{"src":"a"}`)}, Score: 0.9},
		{Doc: retrieval.Doc{ID: "d2", Content: "more cats", Metadata: json.RawMessage(`{}`)}, Score: 0.4},
	}
	var gotQ string
	reg, _ := retrieval.NewRegistry(spyRetriever{name: "pg_fulltext", lastQ: &gotQ, results: results})
	out, err := runRetrieveExec(t, reg, retrieveStepConfig("pg_fulltext", "cats", json.RawMessage(`2`)))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if gotQ != "cats" {
		t.Errorf("query passed to retriever = %q, want cats", gotQ)
	}
	var o retrieveOutput
	if err := json.Unmarshal(out.Data, &o); err != nil {
		t.Fatalf("unmarshaling output: %v", err)
	}
	if o.Retriever != "pg_fulltext" || o.Query != "cats" {
		t.Errorf("output header = %+v, want retriever/query echoed", o)
	}
	if len(o.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(o.Results))
	}
	if o.Results[0].ID != "d1" || o.Results[0].Score != 0.9 {
		t.Errorf("first result = %+v, want d1 @ 0.9 (ranked order preserved)", o.Results[0])
	}
	if string(o.Results[0].Metadata) != `{"src":"a"}` {
		t.Errorf("metadata = %s, want carried through verbatim", o.Results[0].Metadata)
	}
}

func TestRetrieveExecEmptyResultsIsArrayNotNull(t *testing.T) {
	t.Parallel()
	reg, _ := retrieval.NewRegistry(spyRetriever{name: "r", results: nil})
	out, err := runRetrieveExec(t, reg, retrieveStepConfig("r", "nomatch", nil))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// `results` must serialize as [] so a downstream template never misses.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(out.Data, &raw); err != nil {
		t.Fatalf("unmarshaling output: %v", err)
	}
	if string(raw["results"]) != "[]" {
		t.Errorf("results = %s, want [] (never null)", raw["results"])
	}
}

func TestRetrieveExecClassPassthrough(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want dag.ErrorClass
	}{
		{"transient", &retrieval.Error{Retriever: "r", Class: dag.ClassTransient, Message: "db down"}, dag.ClassTransient},
		{"permanent", &retrieval.Error{Retriever: "r", Class: dag.ClassPermanent, Message: "bad"}, dag.ClassPermanent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg, _ := retrieval.NewRegistry(spyRetriever{name: "r", queryErr: tc.err})
			_, err := runRetrieveExec(t, reg, retrieveStepConfig("r", "cats", nil))
			var ce *ClassifiedError
			if !errors.As(err, &ce) {
				t.Fatalf("err = %v, want *ClassifiedError", err)
			}
			if ce.Class != tc.want {
				t.Errorf("class = %v, want %v", ce.Class, tc.want)
			}
		})
	}
}

func TestRetrieveExecContextErrorPassthrough(t *testing.T) {
	t.Parallel()
	reg, _ := retrieval.NewRegistry(spyRetriever{name: "r", queryErr: context.Canceled})
	_, err := runRetrieveExec(t, reg, retrieveStepConfig("r", "cats", nil))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled to pass through", err)
	}
	var ce *ClassifiedError
	if errors.As(err, &ce) {
		t.Errorf("context error wrapped in *ClassifiedError: %v", err)
	}
}
