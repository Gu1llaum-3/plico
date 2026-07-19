package main

import (
	"testing"

	"github.com/spf13/cobra"

	"github.com/Gu1llaum-3/plico/internal/api"
	"github.com/Gu1llaum-3/plico/internal/deploy"
)

func TestPrintResultsExitSemantics(t *testing.T) {
	t.Parallel()
	cmd := &cobra.Command{}
	cmd.SetOut(nopWriter{})

	deployBad := []string{deploy.OutcomeFailed.String(), deploy.OutcomeSkipped.String()}

	// deploy-now: skipped means the requested deployment did not happen.
	err := printResults(cmd, []api.ActionResult{{Stack: "web", Outcome: "skipped"}}, "not deployed", deployBad...)
	if err == nil {
		t.Error("deploy-now must exit non-zero on a skipped stack")
	}
	err = printResults(cmd, []api.ActionResult{{Stack: "web", Outcome: "failed"}}, "not deployed", deployBad...)
	if err == nil {
		t.Error("deploy-now must exit non-zero on a failed stack")
	}
	err = printResults(cmd, []api.ActionResult{{Stack: "web", Outcome: "deployed"}}, "not deployed", deployBad...)
	if err != nil {
		t.Errorf("successful deploy must exit zero: %v", err)
	}

	// check-now: skipped is benign (a deploy owns the stack right now).
	checkBad := []string{deploy.OutcomeFailed.String()}
	err = printResults(cmd, []api.ActionResult{{Stack: "web", Outcome: "skipped"}}, "check failed", checkBad...)
	if err != nil {
		t.Errorf("check-now must exit zero on a skipped stack: %v", err)
	}
	err = printResults(cmd, []api.ActionResult{{Stack: "web", Outcome: "failed"}}, "check failed", checkBad...)
	if err == nil {
		t.Error("check-now must exit non-zero on a failed stack")
	}
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
