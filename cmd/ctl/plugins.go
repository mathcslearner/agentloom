package main

import (
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/mathcslearner/agentloom/internal/api"
)

// newPluginsCmd builds `ctl plugins` (ticket 8.1): the plugin catalog
// the API compiles in (ADR-009), read scope.
func newPluginsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plugins",
		Short: "Inspect the plugin catalog",
	}
	cmd.AddCommand(newPluginsListCmd())
	return cmd
}

// newPluginsListCmd builds `ctl plugins list`: every plugin's kind,
// name, version, and capability flags. Config schemas are deliberately
// not rendered — they are machine-usable documents; fetch
// GET /v1/plugins directly for them.
func newPluginsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List plugins (kind, name, version, capability flags)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, err := clientFromCmd(cmd)
			if err != nil {
				return err
			}
			resp, err := cl.listPlugins(cmd.Context())
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
			fprintln(tw, "KIND\tNAME\tVERSION\tCAPABILITIES\tDESCRIPTION")
			for _, p := range resp.Plugins {
				fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
					p.Kind, p.Name, p.Version, renderCapabilities(p.Capabilities), p.Description)
			}
			return tw.Flush()
		},
	}
}

// renderCapabilities is the compact flag list ("-" when none are set).
func renderCapabilities(c api.PluginCapabilities) string {
	var flags []string
	if c.SideEffectful {
		flags = append(flags, "side_effectful")
	}
	if c.Cacheable {
		flags = append(flags, "cacheable")
	}
	if c.CostBearing {
		flags = append(flags, "cost_bearing")
	}
	if len(flags) == 0 {
		return "-"
	}
	return strings.Join(flags, ",")
}
