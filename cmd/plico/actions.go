package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Gu1llaum-3/plico/internal/api"
	"github.com/Gu1llaum-3/plico/internal/deploy"
)

// stackSelection handles the shared --stack/--all pair (F25/F26).
type stackSelection struct {
	stack string
	all   bool
}

func (s *stackSelection) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&s.stack, "stack", "", "target a single stack")
	cmd.Flags().BoolVar(&s.all, "all", false, "target every stack")
	cmd.MarkFlagsMutuallyExclusive("stack", "all")
}

func (s *stackSelection) target() (string, error) {
	switch {
	case s.all:
		return "*", nil
	case s.stack != "":
		return s.stack, nil
	default:
		return "", fmt.Errorf("either --stack <name> or --all is required")
	}
}

// printResults prints per-stack outcomes and returns an error when any
// outcome is in badOutcomes, so scripts can rely on the exit code. What
// counts as bad depends on the command: a "skipped" deploy-now means the
// requested deployment did not happen, while a "skipped" check-now just
// means a deploy currently owns the stack — a healthy condition.
func printResults(cmd *cobra.Command, results []api.ActionResult, verb string, badOutcomes ...string) error {
	var bad []string
	for _, r := range results {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", r.Stack, r.Outcome)
		for _, b := range badOutcomes {
			if r.Outcome == b {
				bad = append(bad, fmt.Sprintf("%s (%s)", r.Stack, r.Outcome))
			}
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("%s: %s", verb, strings.Join(bad, ", "))
	}
	return nil
}

func init() {
	// check-now (F25): fetch + diff + "queued" notification, never a deploy.
	{
		conn, sel := &clientConn{}, &stackSelection{}
		cmd := &cobra.Command{
			Use:   "check-now",
			Short: "Force an immediate check (fetch + diff, no deploy) outside any schedule (F25)",
			RunE: func(cmd *cobra.Command, args []string) error {
				target, err := sel.target()
				if err != nil {
					return err
				}
				var results []api.ActionResult
				if err := conn.call("POST", "/v1/check", api.ActionRequest{Stack: target}, &results); err != nil {
					return err
				}
				return printResults(cmd, results, "check failed",
					deploy.OutcomeFailed.String())
			},
		}
		conn.registerClientFlags(cmd)
		sel.register(cmd)
		rootCmd.AddCommand(cmd)
	}

	// deploy-now (F26/F30): full pipeline, immediately, window or not.
	{
		conn, sel := &clientConn{}, &stackSelection{}
		var force, skipPre, skipPost bool
		cmd := &cobra.Command{
			Use:   "deploy-now",
			Short: "Force an immediate deployment, bypassing the schedule window (F26)",
			RunE: func(cmd *cobra.Command, args []string) error {
				target, err := sel.target()
				if err != nil {
					return err
				}
				if skipPre && !force {
					return fmt.Errorf("--skip-pre bypasses the backup gate and requires --force (F30)")
				}
				var results []api.ActionResult
				req := api.ActionRequest{Stack: target, Force: force, SkipPre: skipPre, SkipPost: skipPost}
				if err := conn.call("POST", "/v1/deploy", req, &results); err != nil {
					return err
				}
				return printResults(cmd, results, "not deployed",
					deploy.OutcomeFailed.String(), deploy.OutcomeSkipped.String())
			},
		}
		conn.registerClientFlags(cmd)
		sel.register(cmd)
		cmd.Flags().BoolVar(&force, "force", false, "deploy even without a git delta (redeploy current revision)")
		cmd.Flags().BoolVar(&skipPre, "skip-pre", false, "DANGEROUS: skip the pre-deploy backup gate (requires --force)")
		cmd.Flags().BoolVar(&skipPost, "skip-post", false, "skip the post-deploy hook (low risk)")
		rootCmd.AddCommand(cmd)
	}

	// dry-run (F28): what would be deployed.
	{
		conn := &clientConn{}
		var stack string
		cmd := &cobra.Command{
			Use:   "dry-run",
			Short: "Show what would be deployed (git delta, pending commits) without acting (F28)",
			RunE: func(cmd *cobra.Command, args []string) error {
				if stack == "" {
					return fmt.Errorf("--stack is required")
				}
				var report deploy.DryRunReport
				if err := conn.call("POST", "/v1/dry-run", api.ActionRequest{Stack: stack}, &report); err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				if report.UpToDate {
					_, _ = fmt.Fprintf(out, "%s: up to date at %s (ref %s)\n", report.Stack, short(report.NewSHA), report.Ref)
					return nil
				}
				_, _ = fmt.Fprintf(out, "%s: would deploy %s → %s (ref %s)\n",
					report.Stack, short(report.OldSHA), short(report.NewSHA), report.Ref)
				for _, c := range report.Commits {
					_, _ = fmt.Fprintf(out, "  %s\n", c)
				}
				return nil
			},
		}
		conn.registerClientFlags(cmd)
		cmd.Flags().StringVar(&stack, "stack", "", "stack to inspect (required)")
		rootCmd.AddCommand(cmd)
	}
}
