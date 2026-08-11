// Command ctl is agentloom's operator CLI (ticket 4.6):
//
//	ctl validate <file>    check a definition locally (M1 validation)
//	ctl submit <file>      submit a definition to the API as a new run
//	ctl watch <run-id>     poll a run and render its status tree
//
// The API base URL comes from --api or AGENTLOOM_API_URL (default
// http://localhost:8080). ctl is a pure HTTP client — it never touches
// Postgres or Redis.
package main

import (
	"os"

	"github.com/spf13/cobra"
)

func main() {
	if err := newRootCmd(os.LookupEnv).Execute(); err != nil {
		// Cobra already printed the error; the exit code is ours.
		os.Exit(1)
	}
}

// newRootCmd builds the ctl command tree. The environment is injected so
// tests control AGENTLOOM_API_URL without mutating the process env.
func newRootCmd(lookup func(string) (string, bool)) *cobra.Command {
	root := &cobra.Command{
		Use:           "ctl",
		Short:         "agentloom operator CLI",
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	defaultAPI := "http://localhost:8080"
	if v, ok := lookup("AGENTLOOM_API_URL"); ok && v != "" {
		defaultAPI = v
	}
	root.PersistentFlags().String("api", defaultAPI, "base URL of the agentloom API")
	root.AddCommand(newValidateCmd(), newSubmitCmd(), newWatchCmd())
	return root
}
