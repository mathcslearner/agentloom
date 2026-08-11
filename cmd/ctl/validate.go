package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/mathcslearner/agentloom/internal/api"
	"github.com/mathcslearner/agentloom/internal/dag"
)

// newValidateCmd builds `ctl validate <file>`: run M1's codec + structural
// validation locally and print every path-qualified finding. Exit 0 means
// submittable (warnings allowed); exit 1 means the API would reject it.
func newValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate <file>",
		Short: "Validate a workflow definition locally",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()

			def, err := dag.Decode(raw)
			if err != nil {
				printIssues(out, api.DefinitionIssues(err))
				return fmt.Errorf("%s failed to decode", args[0])
			}
			found, err := dag.Validate(def)
			printIssues(out, api.ValidationIssues(found))
			if err != nil {
				return fmt.Errorf("%s is invalid", args[0])
			}
			fprintf(out, "%s: valid (%d warning(s))\n", args[0], len(found))
			return nil
		},
	}
}

// printIssues renders path-qualified issues one per line, mirroring the
// dag package's error formatting.
func printIssues(w io.Writer, issues []api.Issue) {
	for _, i := range issues {
		loc := i.Path
		if loc == "" {
			loc = "(document)"
		}
		if i.Code != "" {
			fprintf(w, "  [%s] %s: %s (%s)\n", i.Severity, loc, i.Msg, i.Code)
			continue
		}
		fprintf(w, "  [%s] %s: %s\n", i.Severity, loc, i.Msg)
	}
}
