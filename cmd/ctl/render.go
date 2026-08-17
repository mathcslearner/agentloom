package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/mathcslearner/agentloom/internal/api"
)

// fprint / fprintf / fprintln write to w swallowing write errors: ctl's
// writers are the process's stdio (or a test buffer), where a failed write
// has no recovery and must not mask the command's real exit status.
func fprint(w io.Writer, args ...any) {
	fmt.Fprint(w, args...) //nolint:errcheck // stdio write
}

func fprintf(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, format, args...) //nolint:errcheck // stdio write
}

func fprintln(w io.Writer, args ...any) {
	fmt.Fprintln(w, args...) //nolint:errcheck // stdio write
}

// statusGlyph marks each step status in the rendered tree.
var statusGlyph = map[string]string{
	"pending":       "·",
	"ready":         "◌",
	"running":       "▸",
	"succeeded":     "✓",
	"failed":        "✗",
	"skipped":       "-",
	"retrying":      "↻",
	"dead_lettered": "†",
	"cancelled":     "⊘",
	"collected":     "~",
}

// renderRun writes the run's status tree: a header with the rollup
// counters, then every step in topological order, indented by its depth in
// the (normal-edge) graph. Loop edges are excluded from ordering — the
// instance graph is acyclic by validation — and any malformed remainder is
// appended flat rather than dropped.
func renderRun(w io.Writer, resp api.RunResponse) {
	r := resp.Run
	fprintf(w, "run %s: %s — %d/%d succeeded, %d failed, %d skipped\n",
		r.ID, r.Status, r.StepsSucceeded, r.StepsTotal, r.StepsFailed, r.StepsSkipped)

	depth := map[string]int{}
	for _, id := range topoOrder(resp) {
		step, ok := stepByID(resp.Steps, id)
		if !ok {
			continue
		}
		d := 0
		for _, e := range resp.Edges {
			if e.Type == "normal" && e.To == id {
				if pd, ok := depth[e.From]; ok && pd+1 > d {
					d = pd + 1
				}
			}
		}
		depth[id] = d
		renderStep(w, step, d)
	}
}

// renderStep writes one step line (and its failure message, if any).
func renderStep(w io.Writer, s api.StepView, depth int) {
	glyph, ok := statusGlyph[s.Status]
	if !ok {
		glyph = "?"
	}
	indent := strings.Repeat("  ", depth)
	line := fmt.Sprintf("  %s%s %s [%s] %s", indent, glyph, s.ID, s.Type, s.Status)
	if s.AttemptCount > 1 {
		line += fmt.Sprintf(" (%d attempts)", s.AttemptCount)
	}
	fprintln(w, line)
	if (s.Status == "failed" || s.Status == "dead_lettered") && len(s.Error) > 0 {
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(s.Error, &e) == nil && e.Message != "" {
			fprintf(w, "  %s    error: %s\n", indent, e.Message)
		}
	}
}

// topoOrder returns the step ids in Kahn topological order over normal
// edges, breaking ties by the response's step order (the store lists steps
// deterministically). Steps left over (only possible with a cyclic or
// dangling payload) are appended in response order so nothing vanishes.
func topoOrder(resp api.RunResponse) []string {
	indeg := make(map[string]int, len(resp.Steps))
	order := make([]string, 0, len(resp.Steps))
	for _, s := range resp.Steps {
		indeg[s.ID] = 0
	}
	for _, e := range resp.Edges {
		if e.Type != "normal" {
			continue
		}
		if _, ok := indeg[e.To]; ok {
			indeg[e.To]++
		}
	}
	emitted := make(map[string]bool, len(resp.Steps))
	for {
		progressed := false
		for _, s := range resp.Steps {
			if emitted[s.ID] || indeg[s.ID] != 0 {
				continue
			}
			emitted[s.ID] = true
			order = append(order, s.ID)
			progressed = true
			for _, e := range resp.Edges {
				if e.Type == "normal" && e.From == s.ID {
					indeg[e.To]--
				}
			}
		}
		if !progressed {
			break
		}
	}
	for _, s := range resp.Steps {
		if !emitted[s.ID] {
			order = append(order, s.ID)
		}
	}
	return order
}

func stepByID(steps []api.StepView, id string) (api.StepView, bool) {
	for _, s := range steps {
		if s.ID == id {
			return s, true
		}
	}
	return api.StepView{}, false
}
