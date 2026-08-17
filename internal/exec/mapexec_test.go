package exec

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// mapSC builds a map StepContext from a rendered MapConfig JSON.
func mapSC(cfg string) StepContext {
	return StepContext{StepType: dag.StepMap, Config: json.RawMessage(cfg), Attempt: 1}
}

// TestMapExecutorEmitsList: the resolved list is surfaced as {items, indices,
// count} for the engine to fan out over.
func TestMapExecutorEmitsList(t *testing.T) {
	t.Parallel()
	out, err := MapExecutor{}.Execute(t.Context(), mapSC(`{"items":["a","b","c"],"body":"x"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got struct {
		Items   []json.RawMessage `json:"items"`
		Indices []int             `json:"indices"`
		Count   int               `json:"count"`
	}
	if err := json.Unmarshal(out.Data, &got); err != nil {
		t.Fatalf("decoding output: %v", err)
	}
	if got.Count != 3 || len(got.Items) != 3 {
		t.Errorf("count/items = %d/%d, want 3/3", got.Count, len(got.Items))
	}
	if len(got.Indices) != 3 || got.Indices[0] != 0 || got.Indices[2] != 2 {
		t.Errorf("indices = %v, want [0 1 2]", got.Indices)
	}
	if string(got.Items[0]) != `"a"` {
		t.Errorf("items[0] = %s, want \"a\"", got.Items[0])
	}
}

// TestMapExecutorMaxItemsPermanent: a list longer than max_items fails
// permanently, before any expansion.
func TestMapExecutorMaxItemsPermanent(t *testing.T) {
	t.Parallel()
	_, err := MapExecutor{}.Execute(t.Context(), mapSC(`{"items":["a","b","c"],"body":"x","max_items":2}`))
	if err == nil {
		t.Fatal("Execute: want an error for a list exceeding max_items")
	}
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Class != dag.ClassPermanent {
		t.Errorf("error = %v, want a permanent ClassifiedError", err)
	}
}

// TestMapExecutorNonArrayPermanent: an items value that is not a JSON array is
// a deterministic data error → permanent.
func TestMapExecutorNonArrayPermanent(t *testing.T) {
	t.Parallel()
	_, err := MapExecutor{}.Execute(t.Context(), mapSC(`{"items":"not-a-list","body":"x"}`))
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Class != dag.ClassPermanent {
		t.Errorf("error = %v, want a permanent ClassifiedError", err)
	}
}

// TestMapExecutorEmptyList: an empty list is valid — count 0.
func TestMapExecutorEmptyList(t *testing.T) {
	t.Parallel()
	out, err := MapExecutor{}.Execute(t.Context(), mapSC(`{"items":[],"body":"x"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal(out.Data, &got)
	if got.Count != 0 {
		t.Errorf("count = %d, want 0", got.Count)
	}
}

// TestGatherExecutorEmitsItems: the gather emits its resolved ordered array
// verbatim.
func TestGatherExecutorEmitsItems(t *testing.T) {
	t.Parallel()
	out, err := GatherExecutor{}.Execute(t.Context(),
		StepContext{StepType: dag.StepGather, Config: json.RawMessage(`{"items":[{"n":1},{"n":2}]}`), Attempt: 1})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var arr []map[string]int
	if err := json.Unmarshal(out.Data, &arr); err != nil {
		t.Fatalf("gather output is not an array: %v (%s)", err, out.Data)
	}
	if len(arr) != 2 || arr[0]["n"] != 1 || arr[1]["n"] != 2 {
		t.Errorf("gather output = %v, want the ordered [{1},{2}]", arr)
	}
}

// TestGatherExecutorEmptyIsEmptyArray: a gather with no items (an empty map)
// emits the empty array.
func TestGatherExecutorEmptyIsEmptyArray(t *testing.T) {
	t.Parallel()
	out, err := GatherExecutor{}.Execute(t.Context(),
		StepContext{StepType: dag.StepGather, Config: json.RawMessage(`{}`), Attempt: 1})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if string(out.Data) != "[]" {
		t.Errorf("gather output = %s, want []", out.Data)
	}
}
