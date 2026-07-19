package main

import (
	"os"
	"path/filepath"
	"strings"
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

func TestPublicHelpDoesNotExposeFeatureIDs(t *testing.T) {
	for _, cmd := range rootCmd.Commands() {
		if strings.Contains(cmd.Short, "(F") {
			t.Errorf("%s help exposes an internal feature ID: %q", cmd.Name(), cmd.Short)
		}
	}
}

func TestCommandsRejectPositionalArguments(t *testing.T) {
	commands := make(map[string]*cobra.Command)
	for _, cmd := range rootCmd.Commands() {
		commands[cmd.Name()] = cmd
	}
	for _, name := range []string{"check-now", "deploy-now", "dry-run", "serve", "status", "validate", "version"} {
		cmd := commands[name]
		if cmd == nil {
			t.Fatalf("command %q is not registered", name)
		}
		if err := cmd.Args(cmd, []string{"unexpected"}); err == nil {
			t.Errorf("%s accepts an unexpected positional argument", name)
		}
	}
}

func TestClientSocketResolutionIgnoresDaemonSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	content := `
base_dir = "/opt/docker"
[api]
socket = "/run/plico/plico.sock"
[git.auths."example.com"]
password = "${PLICO_UNAVAILABLE_CLIENT_TOKEN}"
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	conn := &clientConn{configPath: path}
	client, err := conn.client()
	if err != nil {
		t.Fatalf("client should not require daemon secrets: %v", err)
	}
	client.CloseIdleConnections()
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
