package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mathcslearner/agentloom/internal/api"
)

// newSubmitCmd builds `ctl submit <file>`: POST the definition to the API
// as a new run. The run id is the only thing written to stdout, so
// `ctl watch "$(ctl submit …)"` composes; context goes to stderr.
func newSubmitCmd() *cobra.Command {
	var (
		params string
		token  string
	)
	cmd := &cobra.Command{
		Use:   "submit <file>",
		Short: "Submit a workflow definition as a new run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			req := api.SubmitRunRequest{Definition: raw}
			if params != "" {
				if !json.Valid([]byte(params)) {
					return fmt.Errorf("--params is not valid JSON")
				}
				req.Params = json.RawMessage(params)
			}

			cl, err := clientFromCmd(cmd)
			if err != nil {
				return err
			}
			resp, err := cl.submitRun(cmd.Context(), req, token)
			if err != nil {
				var ae *apiError
				if errors.As(err, &ae) && len(ae.Detail.Issues) > 0 {
					fprintln(cmd.ErrOrStderr(), ae.Error())
					printIssues(cmd.ErrOrStderr(), ae.Detail.Issues)
				}
				return err
			}

			verb := "created"
			if resp.Reused {
				verb = "reused (idempotency token matched)"
			}
			fprintf(cmd.ErrOrStderr(), "run %s: %s, status %s, entry steps: %s\n",
				resp.RunID, verb, resp.Status, strings.Join(resp.EntrySteps, ", "))
			fprintln(cmd.OutOrStdout(), resp.RunID)
			return nil
		},
	}
	cmd.Flags().StringVar(&params, "params", "", "run parameters as a JSON object")
	cmd.Flags().StringVar(&token, "token", "", "idempotency key, sent as the Idempotency-Key header (resubmitting the same key and payload returns the original run)")
	return cmd
}
