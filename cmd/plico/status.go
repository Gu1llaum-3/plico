package main

import (
	"fmt"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Gu1llaum-3/plico/internal/api"
)

func init() {
	conn := &clientConn{}
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Per-stack status: last run, deployed SHA, pending revision, next window",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			var resp api.StatusResponse
			if err := conn.call(cmd.Context(), "GET", "/v1/status", nil, &resp); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "daemon: %s (last tick %s)\n\n", resp.Status, resp.LastTick.Format(time.RFC3339))

			names := make([]string, 0, len(resp.Stacks))
			for n := range resp.Stacks {
				names = append(names, n)
			}
			sort.Strings(names)

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "STACK\tSTATUS\tDEPLOYED\tUPDATED\tNEXT RUN\tPENDING")
			for _, n := range names {
				st := resp.Stacks[n]
				status := displayStatus(st)
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					n, status, short(st.LastDeployedSHA),
					fmtTime(st.UpdatedAt), fmtTime(st.NextRun), short(st.QueuedSHA))
			}
			return w.Flush()
		},
	}
	conn.registerClientFlags(cmd)
	rootCmd.AddCommand(cmd)
}

// displayStatus picks the STATUS column value. The persisted last_status is
// the truth; a more recent live outcome only overrides it when it carries
// NEW information ("failed", "skipped", "queued") — "deployed" and
// "up_to_date" are routine confirmations of the persisted state, and a
// no-op poll tick right before `plico status` must not mask a "success".
func displayStatus(st api.StackHealth) string {
	status := st.LastStatus
	switch st.LastOutcome {
	case "", "deployed", "up_to_date":
		// keep the persisted status
	default:
		if st.LastOutcomeAt != nil && (st.UpdatedAt == nil || st.LastOutcomeAt.After(*st.UpdatedAt)) {
			status = st.LastOutcome
		}
	}
	if st.RunningSince != nil {
		return "running"
	}
	if status == "" {
		return "-"
	}
	return status
}

func short(sha string) string {
	if sha == "" {
		return "-"
	}
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func fmtTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}
