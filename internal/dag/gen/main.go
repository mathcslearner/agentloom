// Command gen regenerates the workflow definition JSON Schema from the Go
// structs in internal/dag. It is invoked by `make generate`; CI runs it
// and fails on any diff against the committed file under docs/schema/.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/mathcslearner/agentloom/internal/dag"
)

func main() {
	out := flag.String("out", "", "path to write the generated JSON Schema to")
	flag.Parse()
	if *out == "" {
		fatalf("usage: gen -out <path>")
	}
	data, err := dag.GenerateJSONSchema()
	if err != nil {
		fatalf("building schema: %v", err)
	}
	if err := os.WriteFile(*out, data, 0o600); err != nil {
		fatalf("writing %s: %v", *out, err)
	}
}

// fatalf reports a fatal generator error and exits nonzero.
func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "gen: "+format+"\n", args...)
	os.Exit(1)
}
