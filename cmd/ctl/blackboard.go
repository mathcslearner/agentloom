package main

import (
	"net/url"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// newBlackboardCmd builds `ctl blackboard <run-id>` (ticket 12.2, read
// scope): the run-scoped blackboard — each key's head by default, or the
// full version history with --history — filterable by --key and --tag.
func newBlackboardCmd() *cobra.Command {
	var (
		keys    []string
		tags    []string
		history bool
		limit   int
	)
	cmd := &cobra.Command{
		Use:   "blackboard <run-id>",
		Short: "Inspect a run's blackboard (heads or history)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := clientFromCmd(cmd)
			if err != nil {
				return err
			}
			q := url.Values{}
			for _, k := range keys {
				q.Add("key", k)
			}
			for _, tg := range tags {
				q.Add("tag", tg)
			}
			if history {
				q.Set("history", "true")
			}
			if limit > 0 {
				q.Set("limit", strconv.Itoa(limit))
			}
			resp, err := cl.getBlackboard(cmd.Context(), args[0], q.Encode())
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
			fprintln(tw, "KEY\tVERSION\tTOKENS\tTAGS\tAUTHOR\tVALUE")
			for _, e := range resp.Entries {
				tagStr := "-"
				if len(e.Tags) > 0 {
					tagStr = strings.Join(e.Tags, ",")
				}
				author := e.AuthorStepID
				if author == "" {
					author = "-"
				}
				fprintf(tw, "%s\t%d\t%d\t%s\t%s\t%s\n",
					e.Key, e.Version, e.TokenCount, tagStr, author, truncateValue(string(e.Value)))
			}
			if resp.NextCursor != "" {
				fprintf(tw, "# more: --cursor via the API (next_cursor=%s)\n", resp.NextCursor)
			}
			return tw.Flush()
		},
	}
	// --name (not --key) for the blackboard key filter: --key is the global
	// bearer-credential flag.
	cmd.Flags().StringSliceVar(&keys, "name", nil, "restrict to these blackboard keys (repeatable)")
	cmd.Flags().StringSliceVar(&tags, "tag", nil, "keep only entries carrying every listed tag (repeatable)")
	cmd.Flags().BoolVar(&history, "history", false, "show every version, not just each key's head")
	cmd.Flags().IntVar(&limit, "limit", 0, "maximum entries to fetch (server default 200)")
	return cmd
}

// truncateValue keeps the table one line per entry.
func truncateValue(s string) string {
	const maxLen = 60
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > maxLen {
		return s[:maxLen-1] + "…"
	}
	return s
}
