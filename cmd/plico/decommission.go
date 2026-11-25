package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Gu1llaum-3/plico/internal/api"
)

// confirmStack asks the operator to type the stack name before a destructive
// action. Anything but the exact name (EOF included) aborts.
func confirmStack(in io.Reader, out io.Writer, stack string) error {
	_, _ = fmt.Fprintf(out, "This stops stack %q on this host, keeps its volumes, and stops plico from ever deploying it here.\nType the stack name to confirm: ", stack)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("reading confirmation: %w", err)
	}
	if strings.TrimSpace(line) != stack {
		return fmt.Errorf("confirmation mismatch: aborted, nothing was stopped")
	}
	return nil
}

func init() {
	conn := &clientConn{}
	var stack string
	var yes bool
	cmd := &cobra.Command{
		Use:   "decommission",
		Short: "Stop a stack for good on this host (volumes kept), so plico never redeploys it",
		Long: "Runs `docker compose down` (never with volumes) for one stack and records a " +
			"tombstone so no scheduler tick deploys, fetches or probes it again. The stack must " +
			"still be in config.toml; remove it from the config and restart plico to forget it. " +
			"Use this before moving a stack to another host.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if stack == "" {
				return fmt.Errorf("--stack is required")
			}
			if !yes {
				if err := confirmStack(cmd.InOrStdin(), cmd.OutOrStdout(), stack); err != nil {
					return err
				}
			}
			var results []api.ActionResult
			req := api.ActionRequest{Stack: stack, Confirm: true}
			if err := conn.call(cmd.Context(), "POST", "/v1/decommission", req, &results); err != nil {
				return err
			}
			return printResults(cmd, results, "decommission failed")
		},
	}
	conn.registerClientFlags(cmd)
	cmd.Flags().StringVar(&stack, "stack", "", "stack to decommission (required, one stack only)")
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the interactive confirmation (scripts)")
	rootCmd.AddCommand(cmd)
}
