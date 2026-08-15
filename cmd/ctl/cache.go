package main

import (
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/mathcslearner/agentloom/internal/api"
)

// newCacheCmd builds `ctl cache` (ticket 9.6): the response-cache ops
// surface — bust entries by namespace and show per-plugin hit rates. Both
// subcommands need the admin scope (ADR-011).
func newCacheCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cache",
		Short: "Inspect and invalidate the response cache (admin)",
	}
	cmd.AddCommand(newCacheBustCmd(), newCacheStatsCmd())
	return cmd
}

// newCacheBustCmd builds `ctl cache bust`: remove cache entries by
// namespace. With no flags it busts everything; --kind narrows to one plugin
// kind; --kind + --name to one concrete plugin. --name without --kind is
// rejected by the API (names are unique only within a kind).
func newCacheBustCmd() *cobra.Command {
	var kind, name string
	cmd := &cobra.Command{
		Use:   "bust",
		Short: "Invalidate cache entries by namespace (all, one kind, or one plugin)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, err := clientFromCmd(cmd)
			if err != nil {
				return err
			}
			resp, err := cl.bustCache(cmd.Context(), api.CacheBustRequest{
				PluginKind: kind,
				PluginName: name,
			})
			if err != nil {
				return err
			}
			fprintf(cmd.OutOrStdout(), "busted %d cache entr%s\n", resp.Deleted, plural(resp.Deleted))
			return nil
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "", "plugin kind to bust: model_provider, tool, retriever (empty = all)")
	cmd.Flags().StringVar(&name, "name", "", "concrete plugin name within --kind (requires --kind)")
	return cmd
}

// newCacheStatsCmd builds `ctl cache stats`: per-plugin cumulative cache
// counters with hit rates, as a table.
func newCacheStatsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stats",
		Short: "Show per-plugin cache hit/miss/store counters and hit rates",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, err := clientFromCmd(cmd)
			if err != nil {
				return err
			}
			resp, err := cl.cacheStats(cmd.Context())
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
			fprintln(tw, "KIND\tNAME\tHITS\tMISSES\tSTORES\tHIT RATE")
			for _, p := range resp.Plugins {
				fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%.1f%%\n",
					p.Kind, p.Name, p.Hits, p.Misses, p.Stores, p.HitRate*100)
			}
			return tw.Flush()
		},
	}
}

// plural returns the entry-count suffix ("y" for 1, "ies" otherwise) so the
// bust message reads naturally.
func plural(n int64) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
