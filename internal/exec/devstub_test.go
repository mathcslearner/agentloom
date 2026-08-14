package exec

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
)

func TestStubTool(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		config json.RawMessage
		want   string
	}{
		{
			"input echoed",
			json.RawMessage(`{"tool": "http_request", "input": {"url": "https://example.com"}}`),
			`{"stub":true,"tool":"http_request","input":{"url":"https://example.com"}}`,
		},
		{
			"absent input becomes null",
			json.RawMessage(`{"tool": "http_request"}`),
			`{"stub":true,"tool":"http_request","input":null}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := (StubToolExecutor{}).Execute(context.Background(), StepContext{
				StepType: dag.StepTool, Config: tc.config,
			})
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if string(out.Data) != tc.want {
				t.Errorf("output = %s, want %s", out.Data, tc.want)
			}
		})
	}

	for name, cfg := range map[string]json.RawMessage{
		"absent config": nil,
		"missing tool":  json.RawMessage(`{"input": {}}`),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := (StubToolExecutor{}).Execute(context.Background(), StepContext{
				StepType: dag.StepTool, Config: cfg,
			})
			if !errors.Is(err, ErrInvalidConfig) {
				t.Errorf("error = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestStubRetrieve(t *testing.T) {
	t.Parallel()

	out, err := (StubRetrieveExecutor{}).Execute(context.Background(), StepContext{
		StepType: dag.StepRetrieve,
		Config:   json.RawMessage(`{"retriever": "pg_fulltext", "query": "durable execution", "top_k": 8}`),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	want := `{"stub":true,"retriever":"pg_fulltext","query":"durable execution","results":[]}`
	if string(out.Data) != want {
		t.Errorf("output = %s, want %s", out.Data, want)
	}

	for name, cfg := range map[string]json.RawMessage{
		"absent config":     nil,
		"missing retriever": json.RawMessage(`{"query": "q"}`),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := (StubRetrieveExecutor{}).Execute(context.Background(), StepContext{
				StepType: dag.StepRetrieve, Config: cfg,
			})
			if !errors.Is(err, ErrInvalidConfig) {
				t.Errorf("error = %v, want ErrInvalidConfig", err)
			}
		})
	}
}
