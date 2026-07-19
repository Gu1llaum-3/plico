package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// actionRequest mirrors api.ActionRequest.
type actionRequest struct {
	Stack    string `json:"stack"`
	Force    bool   `json:"force,omitempty"`
	SkipPre  bool   `json:"skip_pre,omitempty"`
	SkipPost bool   `json:"skip_post,omitempty"`
}

type actionResult struct {
	Stack   string `json:"stack"`
	Outcome string `json:"outcome"`
}

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

func printResults(cmd *cobra.Command, results []actionResult) {
	for _, r := range results {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", r.Stack, r.Outcome)
	}
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
				var results []actionResult
				if err := conn.call("POST", "/v1/check", actionRequest{Stack: target}, &results); err != nil {
					return err
				}
				printResults(cmd, results)
				return nil
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
				var results []actionResult
				req := actionRequest{Stack: target, Force: force, SkipPre: skipPre, SkipPost: skipPost}
				if err := conn.call("POST", "/v1/deploy", req, &results); err != nil {
					return err
				}
				printResults(cmd, results)
				return nil
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
				var report struct {
					Stack    string   `json:"stack"`
					Ref      string   `json:"ref"`
					OldSHA   string   `json:"old_sha"`
					NewSHA   string   `json:"new_sha"`
					UpToDate bool     `json:"up_to_date"`
					Commits  []string `json:"commits"`
				}
				if err := conn.call("POST", "/v1/dry-run", actionRequest{Stack: stack}, &report); err != nil {
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
