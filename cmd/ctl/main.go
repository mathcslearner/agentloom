// Command ctl is agentloom's operator CLI (ticket 4.6):
//
//	ctl validate <file>          check a definition locally (M1 validation)
//	ctl submit <file>            submit a definition to the API as a new run
//	ctl watch <run-id>           poll a run and render its status tree
//	ctl runs                     list runs (filters + keyset pages; ticket 6.5)
//	ctl cancel <run-id>          cancel a run (ticket 6.5)
//	ctl park <run-id>            pause a run's dispatch (ticket 6.5)
//	ctl unpark <run-id>          resume a parked run (ticket 6.5)
//	ctl budget <run-id> <usd>    raise a run's spend budget (ticket 10.3)
//	ctl requeue <run-id> <step>  requeue a dead-lettered step (ticket 6.5)
//	ctl keys …                   manage API keys (create/list/revoke; ticket 6.1)
//	ctl plugins list             list the plugin catalog (ticket 8.1)
//	ctl cache bust|stats         invalidate cache entries / show hit rates (ticket 9.6)
//	ctl blackboard <run-id>      inspect a run's blackboard (ticket 12.2)
//
// The API base URL comes from --api or AGENTLOOM_API_URL (default
// http://localhost:8080); the bearer credential from --key or
// AGENTLOOM_API_KEY (every /v1 route requires a scoped key since ticket
// 6.2 — submit needs the submit scope, watch needs read). ctl is a pure
// HTTP client — it never touches Postgres or Redis.
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
	defaultKey := ""
	if v, ok := lookup("AGENTLOOM_API_KEY"); ok {
		defaultKey = v
	}
	root.PersistentFlags().String("api", defaultAPI, "base URL of the agentloom API")
	root.PersistentFlags().String("key", defaultKey, "bearer API key (default AGENTLOOM_API_KEY)")
	root.AddCommand(newValidateCmd(), newSubmitCmd(), newWatchCmd(), newRunsCmd(),
		newCancelCmd(), newParkCmd(), newUnparkCmd(), newBudgetCmd(), newRequeueCmd(),
		newKeysCmd(), newPluginsCmd(), newCacheCmd(), newBlackboardCmd(),
		newApprovalsCmd(), newApproveCmd(), newRejectCmd(), newOpsCmd())
	return root
}
