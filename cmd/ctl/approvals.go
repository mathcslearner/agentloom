package main

// Human-approval commands (ticket 15.3, ADR-017): the pending-approval inbox
// (`ctl approvals`) and the two decide shells (`ctl approve` / `ctl reject`)
// over POST /v1/approvals/{id}:decide. Machine-readable output to stdout,
// human context to stderr — the submit/watch convention.

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/mathcslearner/agentloom/internal/api"
)

// errInvalidEditJSON is returned when --edit is not valid JSON.
var errInvalidEditJSON = errors.New("--edit value is not valid JSON")

// newApprovalsCmd builds `ctl approvals`: list human-approval records, the
// pending inbox by default (read scope). Filter with --status and --run.
func newApprovalsCmd() *cobra.Command {
	var (
		status string
		run    string
		limit  int
	)
	cmd := &cobra.Command{
		Use:   "approvals",
		Short: "List human-approval requests (the pending inbox)",
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
			if run != "" {
				q.Set("run_id", run)
			}
			if limit > 0 {
				q.Set("limit", strconv.Itoa(limit))
			}
			resp, err := cl.listApprovals(cmd.Context(), q.Encode())
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
			fprintln(tw, "ID\tRUN\tSTEP\tSTATUS\tEDIT\tTIMEOUT_AT\tTITLE")
			for _, a := range resp.Approvals {
				timeout := "-"
				if a.TimeoutAt != nil {
					timeout = a.TimeoutAt.Format("2006-01-02T15:04Z")
				}
				edit := "no"
				if a.AllowEdit {
					edit = "yes"
				}
				fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					a.ID, a.RunID, a.StepID, a.Status, edit, timeout, truncateValue(a.Title))
			}
			if resp.NextCursor != "" {
				fprintf(tw, "# more: --cursor via the API (next_cursor=%s)\n", resp.NextCursor)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&status, "status", "pending", "filter by status (pending|approved|rejected|expired|cancelled; empty = all)")
	cmd.Flags().StringVar(&run, "run", "", "filter to one run id")
	cmd.Flags().IntVar(&limit, "limit", 0, "max rows per page")
	return cmd
}

// newApproveCmd builds `ctl approve <approval-id>`: approve a pending
// approval, optionally with an edited payload (--edit) and a comment.
func newApproveCmd() *cobra.Command {
	var (
		comment string
		edit    string
	)
	cmd := &cobra.Command{
		Use:   "approve <approval-id>",
		Short: "Approve a pending human-approval request",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			edited, err := readEditPayload(edit)
			if err != nil {
				return err
			}
			return runDecide(cmd, args[0], api.DecideApprovalRequest{
				Decision: "approve", EditedPayload: edited, Comment: comment,
			})
		},
	}
	cmd.Flags().StringVar(&comment, "comment", "", "approver note recorded in the audit trail")
	cmd.Flags().StringVar(&edit, "edit", "", "edited payload as inline JSON or @file (approve only; must satisfy the edit schema)")
	return cmd
}

// newRejectCmd builds `ctl reject <approval-id>`: reject a pending approval.
func newRejectCmd() *cobra.Command {
	var comment string
	cmd := &cobra.Command{
		Use:   "reject <approval-id>",
		Short: "Reject a pending human-approval request",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDecide(cmd, args[0], api.DecideApprovalRequest{
				Decision: "reject", Comment: comment,
			})
		},
	}
	cmd.Flags().StringVar(&comment, "comment", "", "rejection note recorded in the audit trail")
	return cmd
}

// runDecide POSTs a decision and prints the resulting step/run state.
func runDecide(cmd *cobra.Command, approvalID string, req api.DecideApprovalRequest) error {
	cl, err := clientFromCmd(cmd)
	if err != nil {
		return err
	}
	resp, err := cl.decideApproval(cmd.Context(), approvalID, req)
	if err != nil {
		return err
	}
	fprintf(cmd.ErrOrStderr(), "approval %s: %s → step %s, run %s (%d step(s) readied)\n",
		resp.Approval.ID, resp.Approval.Status, resp.Approval.StepID, resp.Run.Status, len(resp.ReadiedSteps))
	// The approval id on stdout keeps the command composable.
	fprintln(cmd.OutOrStdout(), resp.Approval.ID)
	return nil
}

// readEditPayload resolves an --edit value: inline JSON, or @file to read
// JSON from a file. Empty means no edit.
func readEditPayload(spec string) (json.RawMessage, error) {
	if spec == "" {
		return nil, nil
	}
	raw := []byte(spec)
	if strings.HasPrefix(spec, "@") {
		b, err := os.ReadFile(spec[1:]) //nolint:gosec // operator-supplied path
		if err != nil {
			return nil, err
		}
		raw = b
	}
	if !json.Valid(raw) {
		return nil, errInvalidEditJSON
	}
	return json.RawMessage(raw), nil
}
