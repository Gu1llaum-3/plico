package main

import (
	"fmt"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

type statusResponse struct {
	Status   string                     `json:"status"`
	LastTick time.Time                  `json:"last_tick"`
	Stacks   map[string]stackStatusView `json:"stacks"`
}

type stackStatusView struct {
	LastDeployedSHA string     `json:"last_deployed_sha"`
	LastStatus      string     `json:"last_status"`
	LastRunID       string     `json:"last_run_id"`
	UpdatedAt       *time.Time `json:"updated_at"`
	RunningSince    *time.Time `json:"running_since"`
	LastOutcome     string     `json:"last_outcome"`
	NextRun         *time.Time `json:"next_run"`
	QueuedSHA       string     `json:"queued_sha"`
}

func init() {
	conn := &clientConn{}
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Per-stack status: last run, deployed SHA, pending revision, next window (F27)",
		RunE: func(cmd *cobra.Command, args []string) error {
			var resp statusResponse
			if err := conn.call("GET", "/v1/status", nil, &resp); err != nil {
				return err
			}
			fmt.Printf("daemon: %s (last tick %s)\n\n", resp.Status, resp.LastTick.Format(time.RFC3339))

			names := make([]string, 0, len(resp.Stacks))
			for n := range resp.Stacks {
				names = append(names, n)
			}
			sort.Strings(names)

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "STACK\tSTATUS\tDEPLOYED\tUPDATED\tNEXT RUN\tPENDING")
			for _, n := range names {
				st := resp.Stacks[n]
				status := st.LastStatus
				if st.RunningSince != nil {
					status = "running"
				} else if status == "" {
					status = "-"
				}
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
