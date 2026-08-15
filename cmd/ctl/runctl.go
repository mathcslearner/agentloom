package main

// Run-lifecycle and run-list commands (ticket 6.5): thin shells over the
// API's cancel/park/unpark/requeue endpoints and the keyset-paginated run
// list. Machine-readable output goes to stdout, human context to stderr —
// the submit/watch convention.

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// newCancelCmd builds `ctl cancel <run-id>`: request the cooperative
// cancel. The run may keep converging after the command returns — watch it
// with `ctl watch`.
func newCancelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <run-id>",
		Short: "Cancel a run (cooperative; in-flight steps settle as workers notice)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := clientFromCmd(cmd)
			if err != nil {
				return err
			}
			resp, err := cl.cancelRun(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			state := "cancelling (in-flight steps settling)"
			if resp.Finalized {
				state = "cancelled"
			}
			fprintf(cmd.ErrOrStderr(), "run %s: %s, %d step(s) swept\n",
				args[0], state, len(resp.CancelledSteps))
			return nil
		},
	}
}

// newParkCmd builds `ctl park <run-id>`: pause dispatch.
func newParkCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "park <run-id>",
		Short: "Park a run (pause dispatch; in-flight steps settle normally)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := clientFromCmd(cmd)
			if err != nil {
				return err
			}
			if _, err := cl.parkRun(cmd.Context(), args[0]); err != nil {
				return err
			}
			fprintf(cmd.ErrOrStderr(), "run %s parked\n", args[0])
			return nil
		},
	}
}

// newUnparkCmd builds `ctl unpark <run-id>`: resume dispatch.
func newUnparkCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unpark <run-id>",
		Short: "Unpark a run (resume dispatch, re-dispatching stranded ready steps)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := clientFromCmd(cmd)
			if err != nil {
				return err
			}
			resp, err := cl.unparkRun(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fprintf(cmd.ErrOrStderr(), "run %s unparked, %d step(s) re-dispatched\n",
				args[0], len(resp.Dispatched))
			return nil
		},
	}
}

// newBudgetCmd builds `ctl budget <run-id> <usd>`: raise a run's spend
// budget (ticket 10.3). Combined with unpark, this resumes a run parked with
// reason budget_exceeded.
func newBudgetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "budget <run-id> <usd>",
		Short: "Raise a run's spend budget (resume path with unpark)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			usd, err := strconv.ParseFloat(args[1], 64)
			if err != nil {
				return fmt.Errorf("budget must be a number in US dollars: %w", err)
			}
			cl, err := clientFromCmd(cmd)
			if err != nil {
				return err
			}
			resp, err := cl.setBudget(cmd.Context(), args[0], usd)
			if err != nil {
				return err
			}
			budget := "none"
			if resp.Run.Cost.BudgetUSD != nil {
				budget = "$" + *resp.Run.Cost.BudgetUSD
			}
			fprintf(cmd.ErrOrStderr(), "run %s budget set to %s\n", args[0], budget)
			return nil
		},
	}
}

// newRequeueCmd builds `ctl requeue <run-id> <step-id>`: reset a
// dead-lettered step to ready with its retry budget re-armed.
func newRequeueCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "requeue <run-id> <step-id>",
		Short: "Requeue a dead-lettered step (retry budget re-armed)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := clientFromCmd(cmd)
			if err != nil {
				return err
			}
			resp, err := cl.requeueStep(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			fprintf(cmd.ErrOrStderr(), "step %s requeued", args[1])
			if resp.RunResumed {
				fprint(cmd.ErrOrStderr(), ", run resumed")
			}
			if len(resp.Revived) > 0 {
				fprintf(cmd.ErrOrStderr(), ", revived: %s", strings.Join(resp.Revived, ", "))
			}
			fprintln(cmd.ErrOrStderr())
			return nil
		},
	}
}

// newRunsCmd builds `ctl runs`: one keyset page of runs, newest first.
// The next-page cursor (if any) prints to stderr so the table on stdout
// stays parseable.
func newRunsCmd() *cobra.Command {
	var (
		status     string
		definition string
		limit      int
		cursor     string
	)
	cmd := &cobra.Command{
		Use:   "runs",
		Short: "List runs (newest first, one page)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, err := clientFromCmd(cmd)
			if err != nil {
				return err
			}
			q := url.Values{}
			if status != "" {
				q.Set("status", status)
			}
			if definition != "" {
				q.Set("definition_id", definition)
			}
			if limit > 0 {
				q.Set("limit", strconv.Itoa(limit))
			}
			if cursor != "" {
				q.Set("cursor", cursor)
			}
			resp, err := cl.listRuns(cmd.Context(), q)
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
			fprintln(tw, "ID\tSTATUS\tOK\tFAIL\tSKIP\tCANCEL\tTOTAL\tCREATED")
			for _, r := range resp.Runs {
				fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%d\t%d\t%s\n",
					r.ID, r.Status, r.StepsSucceeded, r.StepsFailed,
					r.StepsSkipped, r.StepsCancelled, r.StepsTotal,
					r.CreatedAt.Format(time.RFC3339))
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			if resp.NextCursor != "" {
				fprintf(cmd.ErrOrStderr(), "next page: ctl runs --cursor %s\n", resp.NextCursor)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&status, "status", "", "filter by run status")
	cmd.Flags().StringVar(&definition, "definition", "", "filter by stored definition id")
	cmd.Flags().IntVar(&limit, "limit", 0, "page size (server default 50, max 200)")
	cmd.Flags().StringVar(&cursor, "cursor", "", "opaque page cursor from the previous page")
	return cmd
}
