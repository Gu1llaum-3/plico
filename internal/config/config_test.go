package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const validConfig = `
base_dir = "/opt/docker"
timezone = "Europe/Paris"
poll_interval = "30s"

[ntfy]
url = "https://ntfy.sh/plico-test"

[hooks]
timeout = "2m"

[git.auths."bitbucket.org"]
username = "bot"
password = "${PLICO_TEST_TOKEN}"

[[stack]]
name = "webapp"
repo = "https://bitbucket.org/acme/webapp.git"

[[stack]]
name = "db-tools"
repo = "https://bitbucket.org/acme/db.git"
ref = "prod"
compose_file = "compose.yaml"
force_pull = false
sops_files = [".deploy/secrets.enc.env"]
sops_mode = "exec-env"
verify_timeout = "3m"
`

func TestLoadValidConfigWithDefaults(t *testing.T) {
	t.Setenv("PLICO_TEST_TOKEN", "tok123")
	cfg, err := Load(writeConfig(t, validConfig))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Git.Auths["bitbucket.org"].Password != "tok123" {
		t.Errorf("interpolation failed: %q", cfg.Git.Auths["bitbucket.org"].Password)
	}
	// defaults
	if cfg.RunTimeout.Duration != 30*time.Minute {
		t.Errorf("run_timeout default = %s", cfg.RunTimeout.Duration)
	}
	if cfg.MaxConcurrentDeploys != 2 {
		t.Errorf("max_concurrent_deploys default = %d", cfg.MaxConcurrentDeploys)
	}
	if cfg.Health.Listen != "127.0.0.1:9444" {
		t.Errorf("health.listen default = %q", cfg.Health.Listen)
	}
	web := cfg.Stacks[0]
	if web.Ref != "main" || web.ComposeFile != "docker-compose.yml" || !web.ForcePullEnabled() {
		t.Errorf("stack defaults not applied: %+v", web)
	}
	if web.HookTimeout.Duration != 2*time.Minute {
		t.Errorf("stack hook_timeout should inherit [hooks].timeout, got %s", web.HookTimeout.Duration)
	}
	if web.VerifyTimeout.Duration != 90*time.Second {
		t.Errorf("verify_timeout default = %s", web.VerifyTimeout.Duration)
	}
	db := cfg.Stacks[1]
	if db.ForcePullEnabled() {
		t.Error("force_pull=false not honored")
	}
	if db.VerifyTimeout.Duration != 3*time.Minute {
		t.Errorf("explicit verify_timeout lost: %s", db.VerifyTimeout.Duration)
	}
}

func TestLoadErrors(t *testing.T) {
	t.Setenv("PLICO_TEST_TOKEN", "x")
	base := `
base_dir = "/opt/docker"
[[stack]]
name = "app"
repo = "https://example.com/repo.git"
`
	tests := []struct {
		name, content, wantErr string
	}{
		{"unknown key", base + "\nnot_a_key = true\n", "unknown key"},
		{"missing base_dir", strings.Replace(base, `base_dir = "/opt/docker"`, "", 1), "base_dir"},
		{"relative base_dir", strings.Replace(base, "/opt/docker", "opt/docker", 1), "absolute"},
		{"no stacks", `base_dir = "/opt/docker"`, "at least one"},
		{"duplicate names", base + "\n[[stack]]\nname = \"app\"\nrepo = \"https://x/y.git\"\n", "duplicate"},
		{"bad name", strings.Replace(base, `name = "app"`, `name = "-bad!"`, 1), "invalid name"},
		{"missing repo", strings.Replace(base, `repo = "https://example.com/repo.git"`, "", 1), "repo is required"},
		{"bad sops mode", base + "\nsops_mode = \"plain\"\n", "sops_mode"},
		{"absolute sops file", base + "\nsops_files = [\"/etc/passwd\"]\n", "repo-relative"},
		{"escaping compose file", base + "\ncompose_file = \"../../evil.yml\"\n", "escape"},
		{"poll too small", "poll_interval = \"1s\"\n" + base, ">= 5s"},
		{"bad timezone", "timezone = \"Mars/Olympus\"\n" + base, "timezone"},
		{"undefined env var", base + "\n[ntfy]\nurl = \"${PLICO_UNDEFINED_VAR_42}\"\n", "PLICO_UNDEFINED_VAR_42"},
		{"negative max_concurrent_deploys", "max_concurrent_deploys = -1\n" + base, ">= 1"},
		{"traversal sops segment", base + "\nsops_files = [\"a/../../evil.env\"]\n", "repo-relative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tt.content))
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestScheduleInheritanceAndOptOut(t *testing.T) {
	t.Parallel()
	cfg := `
base_dir = "/opt/docker"
schedule = "0 22 * * *"
window = "2h"

[[stack]]
name = "inherits"
repo = "https://example.com/a.git"

[[stack]]
name = "overrides"
repo = "https://example.com/b.git"
schedule = "0 4 * * *"
window = "30m"

[[stack]]
name = "optout"
repo = "https://example.com/c.git"
schedule = "@poll"
`
	c, err := Load(writeConfig(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Stacks[0]; got.Schedule != "0 22 * * *" || got.Window.Duration != 2*time.Hour {
		t.Errorf("inherits: schedule=%q window=%s", got.Schedule, got.Window.Duration)
	}
	if got := c.Stacks[1]; got.Schedule != "0 4 * * *" || got.Window.Duration != 30*time.Minute {
		t.Errorf("overrides: schedule=%q window=%s", got.Schedule, got.Window.Duration)
	}
	if got := c.Stacks[2]; got.Schedule != "" {
		t.Errorf("@poll must opt out of the global schedule, got %q", got.Schedule)
	}
}

func TestScheduleValidation(t *testing.T) {
	t.Parallel()
	base := `
base_dir = "/opt/docker"
[[stack]]
name = "app"
repo = "https://example.com/repo.git"
`
	if _, err := Load(writeConfig(t, base+"\nschedule = \"not a cron\"\n")); err == nil {
		t.Error("invalid stack schedule must fail validation")
	}
	if _, err := Load(writeConfig(t, "schedule = \"61 4 * * *\"\n"+base)); err == nil {
		t.Error("invalid global schedule must fail validation")
	}
	for _, ok := range []string{"0 4 * * *", "@daily", "@every 6h", "*/15 8-18 * * 1-5"} {
		if _, err := Load(writeConfig(t, base+"\nschedule = \""+ok+"\"\n")); err != nil {
			t.Errorf("schedule %q should be valid: %v", ok, err)
		}
	}

	// Syntactically valid but never-firing expressions (Feb 30) would give
	// the scheduler zero times: rejected at load.
	_, err := Load(writeConfig(t, base+"\nschedule = \"0 0 30 2 *\"\n"))
	if err == nil || !strings.Contains(err.Error(), "never fires") {
		t.Errorf("never-firing schedule must fail validation, got %v", err)
	}

	// Windows are validated at their own level (the error must blame the
	// right section) and even without a schedule.
	_, err = Load(writeConfig(t, "window = \"-30m\"\nschedule = \"0 4 * * *\"\n"+base))
	if err == nil || strings.Contains(err.Error(), "stack") {
		t.Errorf("negative global window must blame the global section, got %v", err)
	}
	if _, err := Load(writeConfig(t, base+"\nwindow = \"-30m\"\nschedule = \"@poll\"\n")); err == nil {
		t.Error("negative stack window must fail even with schedule = \"@poll\"")
	}
}

func TestDoubleDotsInFilenamesAreValid(t *testing.T) {
	t.Parallel()
	cfg := `
base_dir = "/opt/docker"
[[stack]]
name = "app"
repo = "https://example.com/repo.git"
compose_file = "docker-compose..prod.yml"
sops_files = ["secrets..enc.env"]
`
	if _, err := Load(writeConfig(t, cfg)); err != nil {
		t.Fatalf("filenames containing '..' must be valid: %v", err)
	}
}

func TestCommentedEnvVarDoesNotBlockStartup(t *testing.T) {
	t.Parallel()
	cfg := `
base_dir = "/opt/docker"
# token = "${PLICO_SURELY_UNSET_VAR}"
[[stack]]
name = "app"  # inline comment with ${PLICO_SURELY_UNSET_TOO}
repo = "https://example.com/repo.git"
`
	if _, err := Load(writeConfig(t, cfg)); err != nil {
		t.Fatalf("unset var in a comment must not be fatal: %v", err)
	}
}

func TestInterpolate(t *testing.T) {
	t.Setenv("PLICO_A", "va")
	tests := []struct {
		in, want string
		wantErr  bool
	}{
		{"x = \"${PLICO_A}\"", "x = \"va\"", false},
		{"x = \"$${PLICO_A}\"", "x = \"${PLICO_A}\"", false}, // escape
		{"x = \"$PLICO_A\"", "x = \"$PLICO_A\"", false},      // bare $VAR untouched
		{"x = \"${PLICO_MISSING_XYZ}\"", "", true},
		{"# x = \"${PLICO_MISSING_XYZ}\"", "# x = \"${PLICO_MISSING_XYZ}\"", false}, // full-line comment
		{"x = \"${PLICO_A}\" # ${PLICO_MISSING_XYZ}", "x = \"va\" # ${PLICO_MISSING_XYZ}", false},
		{"x = \"a#${PLICO_A}\"", "x = \"a#va\"", false},             // '#' inside a basic string is not a comment
		{"x = 'lit#${PLICO_A}'", "x = 'lit#va'", false},             // '#' inside a literal string either
		{"x = \"esc\\\"#${PLICO_A}\"", "x = \"esc\\\"#va\"", false}, // escaped quote does not end the string
	}
	for _, tt := range tests {
		got, err := Interpolate(tt.in)
		if tt.wantErr != (err != nil) {
			t.Errorf("Interpolate(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && got != tt.want {
			t.Errorf("Interpolate(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
