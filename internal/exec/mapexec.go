package exec

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/plugin"
)

// mapVersion / gatherVersion are the production versions of the map fan-out
// executors (ADR-015, ticket 13.4).
const (
	mapVersion    = "1.0.0"
	gatherVersion = "1.0.0"
)

// MapExecutor runs map steps (ADR-015, ticket 13.4): it resolves the runtime
// list (the 8.2 templating middleware already rendered MapConfig.Items into a
// JSON array) and emits it as its output — {items, indices, count} — from which
// the engine's completion transaction generates the fan-out delta (one
// sub-template instance per item plus a gather join) and applies it through
// store.ExpandRun. The executor itself makes no external call and mutates no
// graph; it is a deterministic list pass-through whose one job is to surface the
// resolved list (and enforce the per-item cap) so the completion can expand.
type MapExecutor struct{}

// Type implements Executor.
func (MapExecutor) Type() string { return string(dag.StepMap) }

// PluginManifest implements SelfDescribing (ADR-009). No flags: a map's output
// is deterministic, but caching it would cache a graph mutation, and it makes
// no external call — so it is neither cacheable nor cost-bearing nor
// side-effectful (the join/branch stance).
func (MapExecutor) PluginManifest() plugin.Manifest {
	return builtinManifest(dag.StepMap, mapVersion,
		"Resolve a runtime list and emit it for the engine to fan out into per-item sub-template instances (ADR-015).",
		plugin.Capabilities{})
}

// mapOutput is a map step's output: the resolved item list, its 0-based
// indices, and the count. The engine reads Items to generate the expansion, and
// each generated instance references its item through this step's output
// (`${{ steps.<map>.output.items.<k> }}` / `.indices.<k>`).
type mapOutput struct {
	Items   []json.RawMessage `json:"items"`
	Indices []int             `json:"indices"`
	Count   int               `json:"count"`
}

// Execute implements Executor.
func (MapExecutor) Execute(_ context.Context, sc StepContext) (Output, error) {
	c, err := configAs[*dag.MapConfig](sc)
	if err != nil {
		return Output{}, err
	}
	if c == nil || len(c.Items) == 0 {
		// The rendered list is absent — a corrupt materialized config (validation
		// requires items); deterministic, so permanent.
		return Output{}, Permanentf("map: config has no items")
	}
	var items []json.RawMessage
	if err := json.Unmarshal(c.Items, &items); err != nil {
		// The resolved `items` is not a JSON array (the upstream reference did
		// not produce a list): a deterministic data error, permanent.
		return Output{}, Permanentf("map: items did not resolve to a JSON array: %v", err)
	}
	if c.MaxItems > 0 && len(items) > c.MaxItems {
		return Output{}, Permanentf("map: list has %d items, exceeding max_items %d", len(items), c.MaxItems)
	}
	indices := make([]int, len(items))
	for i := range items {
		indices[i] = i
	}
	data, err := json.Marshal(mapOutput{Items: items, Indices: indices, Count: len(items)})
	if err != nil {
		return Output{}, fmt.Errorf("map: marshaling output: %w", err)
	}
	return Output{Data: data}, nil
}

// GatherExecutor runs gather steps (ADR-015, ticket 13.4): a fan-in barrier
// whose config carries an ordered list of per-instance output references (the
// map expansion generated them; the 8.2 renderer has resolved each into its
// instance's result). It emits that resolved list verbatim as its output — the
// map's ordered result array. An authored gather (a literal array) works the
// same way.
type GatherExecutor struct{}

// Type implements Executor.
func (GatherExecutor) Type() string { return string(dag.StepGather) }

// PluginManifest implements SelfDescribing (ADR-009). No flags: the barrier's
// meaning is *when* it becomes ready (all instances succeeded), and executing it
// is a pure pass-through of its resolved items — the join stance.
func (GatherExecutor) PluginManifest() plugin.Manifest {
	return builtinManifest(dag.StepGather, gatherVersion,
		"Fan-in barrier that emits its resolved ordered items array — the map's gathered results (ADR-015).",
		plugin.Capabilities{})
}

// Execute implements Executor.
func (GatherExecutor) Execute(_ context.Context, sc StepContext) (Output, error) {
	c, err := configAs[*dag.GatherConfig](sc)
	if err != nil {
		return Output{}, err
	}
	if c == nil || len(c.Items) == 0 {
		// An empty map (a zero-length list) generates a gather with no items:
		// the ordered result is the empty array.
		return Output{Data: json.RawMessage("[]")}, nil
	}
	return Output{Data: c.Items}, nil
}
