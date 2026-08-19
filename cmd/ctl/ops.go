package main

import (
	"net/url"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// newOpsCmd builds `ctl ops` (ticket 18.6): the operator views — the caller's
// own identity/scopes, the queue-health system stats, and the cross-run
// dead-letter list. All read-scoped. The requeue action itself is the existing
// `ctl requeue <run-id> <step-id>`.
func newOpsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ops",
		Short: "Operator views: identity, queue health, and the dead-letter list",
	}
	cmd.AddCommand(newWhoAmICmd(), newStatsCmd(), newDLQCmd())
	return cmd
}

// newWhoAmICmd builds `ctl ops whoami`: the caller's key id and scopes.
func newWhoAmICmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show the caller's key id and granted scopes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, err := clientFromCmd(cmd)
			if err != nil {
				return err
			}
			me, err := cl.whoami(cmd.Context())
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fprintf(w, "key_id\t%s\n", me.KeyID)
			fprintf(w, "scopes\t%v\n", me.Scopes)
			return w.Flush()
		},
	}
}

// newStatsCmd builds `ctl ops stats`: the queue-health snapshot.
func newStatsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stats",
		Short: "Show queue depth, PEL, delayed, DLQ backlog, active runs, and workers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, err := clientFromCmd(cmd)
			if err != nil {
				return err
			}
			s, err := cl.systemStats(cmd.Context())
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			if s.Queue != nil {
				q := s.Queue
				fprintf(w, "queue\t%s/%s\n", q.Stream, q.Group)
				fprintf(w, "ready\t%d\n", q.ReadyDepth)
				fprintf(w, "pending\t%d\n", q.Pending)
				fprintf(w, "delayed\t%d\n", q.Delayed)
				fprintf(w, "workers\t%d active / %d total\n", q.WorkersActive, len(q.Workers))
			} else {
				fprintf(w, "queue\tunavailable (%s)\n", s.QueueError)
			}
			fprintf(w, "outbox\t%d pending\n", s.Outbox.Backlog)
			fprintf(w, "dead_letters\t%d open\n", s.DeadLetters.Open)
			fprintf(w, "runs_active\t%d\n", s.Runs.Active)
			if err := w.Flush(); err != nil {
				return err
			}
			if s.Queue != nil && len(s.Queue.Workers) > 0 {
				fprintf(cmd.OutOrStdout(), "\nworkers:\n")
				ww := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
				fprintf(ww, "  ID\tIDLE(ms)\tPENDING\tACTIVE\n")
				for _, c := range s.Queue.Workers {
					fprintf(ww, "  %s\t%d\t%d\t%t\n", c.ID, c.IdleMs, c.Pending, c.Active)
				}
				return ww.Flush()
			}
			return nil
		},
	}
}

// newDLQCmd builds `ctl ops dlq`: the cross-run dead-letter list.
func newDLQCmd() *cobra.Command {
	var status, runID, source string
	cmd := &cobra.Command{
		Use:   "dlq",
		Short: "List dead-lettered steps across runs (default: open only)",
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
			if runID != "" {
				q.Set("run_id", runID)
			}
			if source != "" {
				q.Set("source", source)
			}
			resp, err := cl.listDeadLetters(cmd.Context(), q.Encode())
			if err != nil {
				return err
			}
			if len(resp.DeadLetters) == 0 {
				fprintf(cmd.OutOrStdout(), "no dead letters\n")
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fprintf(w, "RUN\tSTEP\tTYPE\tSOURCE\tCLASS\tATTEMPTS\tOPEN\n")
			for _, d := range resp.DeadLetters {
				fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%t\n",
					d.RunID, d.StepID, d.StepType, d.Source, d.Class, d.AttemptsAtDeath, d.Open)
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&status, "status", "", "open (default) or all")
	cmd.Flags().StringVar(&runID, "run", "", "filter to one run id")
	cmd.Flags().StringVar(&source, "source", "", "filter to a source: retries_exhausted, permanent, poison")
	return cmd
}
