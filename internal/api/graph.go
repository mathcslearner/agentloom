package api

// Run-graph introspection endpoint (ticket 13.6, ADR-015): GET
// /v1/runs/{id}/graph serves the run's current versioned graph with per-row
// provenance (definition | planner | map | loop, and the version + time each
// node/edge was added) plus the ordered per-version expansion deltas. It is
// the contract the M18 dashboard uses to render and animate runtime graph
// expansion. Read-only, like the cost, log, and blackboard endpoints — the API
// never mutates the graph (planner/map/loop completions do, in the worker),
// so ADR-002 stays untouched.

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// originDefinition is the provenance kind reported for an authored node/edge —
// one whose origin columns are NULL (introduced at instantiation, not by an
// expansion). The expansion kinds ("planner"/"map"/"loop") are the stored
// origin_kind values.
const originDefinition = "definition"

// graphExpandedPayload is the shallow decode of a graph_expanded event's
// payload (store.GraphExpandedEvent). Its Delta is decoded to topology only —
// step ids/types and edge endpoints/types — deliberately NOT into dag.Step /
// dag.Edge: a dag.Step's Config is a StepConfig interface that only
// dag.DecodePlanOutput can populate, and the graph view surfaces no config, so
// a plain shallow decode is both correct and sufficient.
type graphExpandedPayload struct {
	OriginStep  string `json:"origin_step"`
	OriginKind  string `json:"origin_kind"`
	FromVersion int32  `json:"from_version"`
	ToVersion   int32  `json:"to_version"`
	Depth       int32  `json:"depth"`
	Delta       struct {
		Steps []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"steps"`
		Edges []struct {
			From string `json:"from"`
			To   string `json:"to"`
			Type string `json:"type"`
		} `json:"edges"`
	} `json:"delta"`
	Readied []string `json:"readied"`
	Widened []string `json:"widened"`
}

// handleRunGraph is GET /v1/runs/{id}/graph. It reads the run's steps, edges,
// and graph_expanded events and projects them into RunGraphResponse. A run
// that never expanded returns its authored graph (version 1) with an empty
// Expansions list.
func (h *Handler) handleRunGraph(w http.ResponseWriter, r *http.Request) {
	runID, ok := h.runIDParam(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	run, err := h.st.Runs().Get(ctx, runID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, ErrorDetail{
				Code: ErrCodeRunNotFound, Message: "no run with id " + runID.String(),
			})
			return
		}
		internalError(w, r, "reading run", err)
		return
	}
	steps, err := h.st.Steps().ListByRun(ctx, runID)
	if err != nil {
		internalError(w, r, "listing run steps", err)
		return
	}
	edges, err := h.st.Steps().ListEdgesByRun(ctx, runID)
	if err != nil {
		internalError(w, r, "listing run edges", err)
		return
	}
	events, err := h.st.Events().ListByType(ctx, runID, store.EventGraphExpanded)
	if err != nil {
		internalError(w, r, "listing graph_expanded events", err)
		return
	}
	resp, err := buildRunGraphResponse(run, steps, edges, events)
	if err != nil {
		internalError(w, r, "building run graph", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// buildRunGraphResponse is the pure projection from the run row, its step and
// edge rows, and its graph_expanded events into the wire graph view. It is
// factored out of the handler so the contract and consistency tests can drive
// it over fixture rows deterministically.
//
// The nodes and edges come from the rows (they carry live status/resolution
// plus the denormalized provenance columns); the Expansions come from the
// events (the authoritative version-transition metadata and the injection
// time). A node's AddedAt resolves through a version->time map: version 1 is
// the run's creation time, and each later version is its graph_expanded
// event's time. The two representations agree by construction (a row's
// graph_version is stamped by the same expansion that emitted the event) and a
// consistency test asserts it.
func buildRunGraphResponse(run gen.Run, steps []gen.RunStep, edges []gen.RunEdge, events []gen.Event) (RunGraphResponse, error) {
	// Decode the expansion deltas first so their times index the version map.
	expansions := make([]GraphExpansionView, 0, len(events))
	versionTime := map[int32]time.Time{1: run.CreatedAt}
	for _, ev := range events {
		var ge graphExpandedPayload
		if err := json.Unmarshal(ev.Payload, &ge); err != nil {
			return RunGraphResponse{}, err
		}
		versionTime[ge.ToVersion] = ev.CreatedAt

		addedSteps := make([]string, 0, len(ge.Delta.Steps))
		for _, s := range ge.Delta.Steps {
			addedSteps = append(addedSteps, s.ID)
		}
		addedEdges := make([]GraphEdgeRef, 0, len(ge.Delta.Edges))
		for _, e := range ge.Delta.Edges {
			addedEdges = append(addedEdges, GraphEdgeRef{
				From: e.From, To: e.To, Type: edgeTypeString(e.Type),
			})
		}
		expansions = append(expansions, GraphExpansionView{
			Version:     int(ge.ToVersion),
			FromVersion: int(ge.FromVersion),
			OriginStep:  ge.OriginStep,
			OriginKind:  ge.OriginKind,
			Depth:       int(ge.Depth),
			AddedAt:     ev.CreatedAt,
			AddedSteps:  addedSteps,
			AddedEdges:  addedEdges,
			Readied:     ge.Readied,
			Widened:     ge.Widened,
		})
	}
	// Events already arrive in seq order (ListEventsByType), which is version
	// order; sort defensively so the delta feed is monotonic regardless.
	sort.Slice(expansions, func(i, j int) bool { return expansions[i].Version < expansions[j].Version })

	nodes := make([]GraphNodeView, 0, len(steps))
	for _, s := range steps {
		nodes = append(nodes, GraphNodeView{
			ID:           s.StepID,
			Type:         s.StepType,
			Status:       s.Status,
			Depth:        int(s.Depth),
			GraphVersion: int(s.GraphVersion),
			Origin:       originView(s.OriginKind, s.OriginStep),
			AddedAt:      versionTime[s.GraphVersion],
		})
	}

	edgeViews := make([]GraphEdgeView, 0, len(edges))
	for _, e := range edges {
		edgeViews = append(edgeViews, GraphEdgeView{
			From:         e.FromStep,
			To:           e.ToStep,
			Type:         edgeTypeString(e.EdgeType),
			When:         textOrEmpty(e.WhenExpr),
			Resolution:   e.Resolution,
			GraphVersion: int(e.GraphVersion),
			Origin:       originView(e.OriginKind, e.OriginStep),
		})
	}

	return RunGraphResponse{
		RunID:        run.ID.String(),
		GraphVersion: int(run.GraphVersion),
		StepsTotal:   int(run.StepsTotal),
		Nodes:        nodes,
		Edges:        edgeViews,
		Expansions:   expansions,
	}, nil
}

// originView maps the stored origin columns into the wire provenance. Both
// columns are NULL for an authored row (reported as "definition"); when set,
// origin_kind is the expansion kind and origin_step names the injecting step.
func originView(kind, step *string) GraphOriginView {
	if kind == nil {
		return GraphOriginView{Kind: originDefinition}
	}
	return GraphOriginView{Kind: *kind, Step: textOrEmpty(step)}
}

// edgeTypeString normalizes an edge type for the wire: an empty stored/plan
// type (dag.Edge omits "normal" in its canonical JSON) is reported as
// "normal" explicitly, so the graph view always names the type.
func edgeTypeString(t string) string {
	if t == "" {
		return "normal"
	}
	return t
}

// textOrEmpty dereferences a nullable text column to "" when NULL.
func textOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
