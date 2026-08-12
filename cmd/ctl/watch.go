package main

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/mathcslearner/agentloom/internal/store"
)

// newWatchCmd builds `ctl watch <run-id>`: poll the run and reprint its
// status tree whenever it changes, until the run reaches a terminal
// status. Exit 0 on succeeded, 1 on failed or cancelled (or on timeout).
// A parked run is not terminal — the watch keeps polling through it.
func newWatchCmd() *cobra.Command {
	var (
		interval time.Duration
		timeout  time.Duration
	)
	cmd := &cobra.Command{
		Use:   "watch <run-id>",
		Short: "Watch a run until it completes",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := clientFromCmd(cmd)
			if err != nil {
				return err
			}
			if interval <= 0 {
				return fmt.Errorf("--interval must be positive, got %s", interval)
			}
			ctx := cmd.Context()
			if timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()
			}
			return watch(ctx, cl, args[0], interval, cmd)
		},
	}
	cmd.Flags().DurationVar(&interval, "interval", time.Second, "poll interval")
	cmd.Flags().DurationVar(&timeout, "timeout", 15*time.Minute, "give up after this long (0 = never)")
	return cmd
}

// watch is the poll loop, split from the cobra wiring for tests.
func watch(ctx context.Context, c *client, runID string, interval time.Duration, cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	var last string
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		resp, err := c.getRun(ctx, runID)
		if err != nil {
			return err
		}
		var buf bytes.Buffer
		renderRun(&buf, resp)
		if buf.String() != last {
			last = buf.String()
			fprint(out, last)
		}
		switch resp.Run.Status {
		case store.RunStatusSucceeded:
			return nil
		case store.RunStatusFailed:
			return fmt.Errorf("run %s failed", runID)
		case store.RunStatusCancelled:
			return fmt.Errorf("run %s cancelled", runID)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("watch: %w (run still %s)", ctx.Err(), resp.Run.Status)
		case <-t.C:
		}
	}
}
