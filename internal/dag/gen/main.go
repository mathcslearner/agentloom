// Command gen regenerates the JSON Schemas published from the Go structs — the
// workflow definition schema and the planner PlanOutput schema (ADR-015) from
// internal/dag, and the event-feed envelope schema (ADR-018) from
// internal/event. It is invoked by `make generate`; CI runs it and fails on any
// diff against the committed files under docs/schema/.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/event"
)

func main() {
	out := flag.String("out", "", "path to write the workflow definition JSON Schema to")
	planOut := flag.String("plan-out", "", "path to write the PlanOutput JSON Schema to (ADR-015)")
	eventsOut := flag.String("events-out", "", "path to write the event-feed JSON Schema to (ADR-018)")
	flag.Parse()
	if *out == "" && *planOut == "" && *eventsOut == "" {
		fatalf("usage: gen -out <path> [-plan-out <path>] [-events-out <path>]")
	}
	if *out != "" {
		write(*out, dag.GenerateJSONSchema)
	}
	if *planOut != "" {
		write(*planOut, dag.GeneratePlanOutputSchema)
	}
	if *eventsOut != "" {
		write(*eventsOut, event.GenerateSchema)
	}
}

// write renders a schema and writes it to path, exiting nonzero on failure.
func write(path string, build func() ([]byte, error)) {
	data, err := build()
	if err != nil {
		fatalf("building schema for %s: %v", path, err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		fatalf("writing %s: %v", path, err)
	}
}

// fatalf reports a fatal generator error and exits nonzero.
func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "gen: "+format+"\n", args...)
	os.Exit(1)
}
