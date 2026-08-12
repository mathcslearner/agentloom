package main

import (
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/mathcslearner/agentloom/internal/api"
)

// newKeysCmd builds `ctl keys` (ticket 6.1): API key management against
// the admin-gated /v1/keys routes. The caller authenticates with --key /
// AGENTLOOM_API_KEY — an admin-scoped stored key, or the server's
// bootstrap root key when no stored key exists yet.
func newKeysCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "keys",
		Short: "Manage API keys (requires an admin credential)",
	}
	cmd.AddCommand(newKeysCreateCmd(), newKeysListCmd(), newKeysRevokeCmd())
	return cmd
}

// newKeysCreateCmd builds `ctl keys create`: mint a key. The plaintext is
// the only thing written to stdout — `KEY=$(ctl keys create …)` composes —
// and it is shown this once; everything else goes to stderr.
func newKeysCreateCmd() *cobra.Command {
	var (
		name   string
		scopes string
		ttl    string
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create an API key (plaintext printed once, to stdout)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, err := clientFromCmd(cmd)
			if err != nil {
				return err
			}
			req := api.CreateKeyRequest{Name: name, TTL: ttl}
			for _, s := range strings.Split(scopes, ",") {
				if s = strings.TrimSpace(s); s != "" {
					req.Scopes = append(req.Scopes, s)
				}
			}
			resp, err := cl.createKey(cmd.Context(), req)
			if err != nil {
				return err
			}
			expires := "never"
			if resp.ExpiresAt != nil {
				expires = resp.ExpiresAt.Format(time.RFC3339)
			}
			fprintf(cmd.ErrOrStderr(), "key %s created: name %q, scopes %s, expires %s\n",
				resp.ID, resp.Name, strings.Join(resp.Scopes, ","), expires)
			fprintln(cmd.ErrOrStderr(), "the key on stdout is shown once and cannot be recovered — store it now")
			fprintln(cmd.OutOrStdout(), resp.Key)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "human label for the key (required)")
	cmd.Flags().StringVar(&scopes, "scopes", "", "comma-separated scopes: submit,read,approve,admin (required)")
	cmd.Flags().StringVar(&ttl, "ttl", "", "optional lifetime as a Go duration (e.g. 720h); empty = never expires")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("scopes")
	return cmd
}

// newKeysListCmd builds `ctl keys list`: every key, newest first. The
// listing carries lookup prefixes only — plaintext exists nowhere to show.
func newKeysListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List API keys (prefixes only)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, err := clientFromCmd(cmd)
			if err != nil {
				return err
			}
			resp, err := cl.listKeys(cmd.Context())
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
			fprintln(tw, "ID\tPREFIX\tNAME\tSCOPES\tCREATED\tEXPIRES\tREVOKED")
			for _, k := range resp.Keys {
				expires, revoked := "-", "-"
				if k.ExpiresAt != nil {
					expires = k.ExpiresAt.Format(time.RFC3339)
				}
				if k.RevokedAt != nil {
					revoked = k.RevokedAt.Format(time.RFC3339)
				}
				fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					k.ID, k.Prefix, k.Name, strings.Join(k.Scopes, ","),
					k.CreatedAt.Format(time.RFC3339), expires, revoked)
			}
			return tw.Flush()
		},
	}
}

// newKeysRevokeCmd builds `ctl keys revoke <id>`: soft revocation,
// idempotent on the server side.
func newKeysRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <key-id>",
		Short: "Revoke an API key (soft; keeps the row for audit)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := clientFromCmd(cmd)
			if err != nil {
				return err
			}
			if err := cl.revokeKey(cmd.Context(), args[0]); err != nil {
				return err
			}
			fprintf(cmd.ErrOrStderr(), "key %s revoked\n", args[0])
			return nil
		},
	}
}
