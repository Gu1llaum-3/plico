package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeManifest(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolveManifest(t *testing.T) {
	t.Parallel()

	t.Run("none", func(t *testing.T) {
		t.Parallel()
		p, err := ResolveManifest(t.TempDir())
		if err != nil || p != "" {
			t.Fatalf("got (%q, %v), want (\"\", nil)", p, err)
		}
	})
	t.Run("file form", func(t *testing.T) {
		t.Parallel()
		d := t.TempDir()
		writeManifest(t, d, ".plico.toml", "")
		p, err := ResolveManifest(d)
		if err != nil || p != filepath.Join(d, ".plico.toml") {
			t.Fatalf("got (%q, %v)", p, err)
		}
	})
	t.Run("dir form", func(t *testing.T) {
		t.Parallel()
		d := t.TempDir()
		writeManifest(t, d, ".plico/plico.toml", "")
		p, err := ResolveManifest(d)
		if err != nil || p != filepath.Join(d, ".plico", "plico.toml") {
			t.Fatalf("got (%q, %v)", p, err)
		}
	})
	t.Run("both is ambiguous", func(t *testing.T) {
		t.Parallel()
		d := t.TempDir()
		writeManifest(t, d, ".plico.toml", "")
		writeManifest(t, d, ".plico/plico.toml", "")
		if _, err := ResolveManifest(d); err == nil {
			t.Fatal("want error for ambiguous layout")
		}
	})
	t.Run("dir without manifest is valid (hooks-only .plico/)", func(t *testing.T) {
		t.Parallel()
		d := t.TempDir()
		// a .plico/ holding only hook scripts (no plico.toml) is legitimate:
		// no manifest, but .plico/pre-deploy.sh may still run.
		if err := os.MkdirAll(filepath.Join(d, ".plico"), 0o755); err != nil {
			t.Fatal(err)
		}
		p, err := ResolveManifest(d)
		if err != nil || p != "" {
			t.Fatalf("got (%q, %v), want (\"\", nil)", p, err)
		}
	})
	t.Run("symlinked manifest is rejected", func(t *testing.T) {
		t.Parallel()
		d := t.TempDir()
		target := filepath.Join(t.TempDir(), "outside.toml")
		if err := os.WriteFile(target, []byte("[compose]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(d, ".plico.toml")); err != nil {
			t.Fatal(err)
		}
		if _, err := ResolveManifest(d); err == nil {
			t.Fatal("want error: a symlinked .plico.toml must not be followed")
		}
	})
}

func TestLoadManifest(t *testing.T) {
	t.Parallel()
	d := t.TempDir()

	t.Run("happy path, no interpolation", func(t *testing.T) {
		t.Parallel()
		p := filepath.Join(d, "ok.toml")
		if err := os.WriteFile(p, []byte(`
pre_deploy  = [".plico/hooks/backup.sh", ".plico/hooks/migrate.sh"]
post_deploy = [".plico/hooks/notify.sh"]

[compose]
compose_file = "docker-compose.prod.yml"
profiles = ["web", "worker"]
env_files = ["config/app.env"]
remove_orphans = false
up_args = ["--wait", "--pull=always", "--scale=web=2"]

[sops]
files = ["secrets/app.enc.env"]
mode  = "tmpfs"

[secrets]
files = ["secrets/tls.enc.key"]
`), 0o644); err != nil {
			t.Fatal(err)
		}
		m, err := LoadManifest(p)
		if err != nil {
			t.Fatal(err)
		}
		if m.Compose.ComposeFile != "docker-compose.prod.yml" {
			t.Errorf("compose_file = %q", m.Compose.ComposeFile)
		}
		if len(m.Compose.Profiles) != 2 || len(m.Compose.EnvFiles) != 1 {
			t.Errorf("profiles/env_files = %v / %v", m.Compose.Profiles, m.Compose.EnvFiles)
		}
		if m.Compose.RemoveOrphans == nil || *m.Compose.RemoveOrphans {
			t.Errorf("remove_orphans should be explicit false")
		}
		if len(m.Sops.Files) != 1 || m.Sops.Mode != "tmpfs" {
			t.Errorf("sops = %+v", m.Sops)
		}
		if len(m.Secrets.Files) != 1 {
			t.Errorf("secrets = %+v", m.Secrets)
		}
		if len(m.PreDeploy) != 2 || len(m.PostDeploy) != 1 {
			t.Errorf("hooks = %v / %v", m.PreDeploy, m.PostDeploy)
		}
	})

	t.Run("dollar var stays literal", func(t *testing.T) {
		t.Parallel()
		p := filepath.Join(d, "literal.toml")
		if err := os.WriteFile(p, []byte("[compose]\nenv_files = [\"${HOME}/x.env\"]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Absolute after interpolation would escape; but plico must NOT
		// interpolate, so ${HOME}/x.env is a literal relative path and passes.
		m, err := LoadManifest(p)
		if err != nil {
			t.Fatal(err)
		}
		if m.Compose.EnvFiles[0] != "${HOME}/x.env" {
			t.Errorf("env_file was interpolated: %q", m.Compose.EnvFiles[0])
		}
	})

	errCases := []struct{ name, content, want string }{
		{"unknown key", "[compose]\nnope = true\n", "unknown key"},
		{"unknown top-level key", "wat = true\n", "unknown key"},
		{"bad sops mode", "[sops]\nmode = \"plain\"\n", "sops.mode"},
		{"absolute sops file", "[sops]\nfiles = [\"/etc/x.env\"]\n", "repo-relative"},
		{"escaping secret file", "[secrets]\nfiles = [\"../../k\"]\n", "repo-relative"},
		{"absolute pre_deploy", "pre_deploy = [\"/etc/evil.sh\"]\n", "repo-relative"},
		{"escaping post_deploy", "post_deploy = [\"a/../../evil.sh\"]\n", "repo-relative"},
		{"secret name collision", "[secrets]\nfiles = [\"a/tls.enc.key\", \"b/tls.enc.key\"]\n", "collide"},
		{"secret degenerate name", "[secrets]\nfiles = [\"secrets/.enc\"]\n", "invalid name"},
		{"absolute compose_file", "[compose]\ncompose_file = \"/etc/x.yml\"\n", "repo-relative"},
		{"escaping compose_file", "[compose]\ncompose_file = \"../../x.yml\"\n", "repo-relative"},
		{"absolute env_file", "[compose]\nenv_files = [\"/etc/x.env\"]\n", "repo-relative"},
		{"escaping env_file", "[compose]\nenv_files = [\"a/../../x.env\"]\n", "repo-relative"},
		{"up_arg short flag -d", "[compose]\nup_args = [\"-d\"]\n", "long-form"},
		{"up_arg bundled short -pevil", "[compose]\nup_args = [\"-pevil\"]\n", "long-form"},
		{"up_arg bundled -dp", "[compose]\nup_args = [\"-dp\", \"evil\"]\n", "long-form"},
		{"up_arg bare value token", "[compose]\nup_args = [\"--pull\", \"always\"]\n", "long-form"},
		{"up_arg disallowed remove-orphans", "[compose]\nup_args = [\"--remove-orphans\"]\n", "not allowed"},
		{"up_arg disallowed --project-name", "[compose]\nup_args = [\"--project-name=evil\"]\n", "not allowed"},
		{"up_arg disallowed --file", "[compose]\nup_args = [\"--file=other.yml\"]\n", "not allowed"},
		{"up_arg disallowed --profile", "[compose]\nup_args = [\"--profile=x\"]\n", "not allowed"},
		{"up_arg disallowed --project-directory", "[compose]\nup_args = [\"--project-directory=/etc\"]\n", "not allowed"},
	}
	for _, tc := range errCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := filepath.Join(t.TempDir(), "m.toml")
			if err := os.WriteFile(p, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := LoadManifest(p)
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func boolp(b bool) *bool { return &b }

func TestMerge(t *testing.T) {
	t.Parallel()

	t.Run("nil manifest = defaults", func(t *testing.T) {
		t.Parallel()
		r := Merge(StackConfig{}, nil)
		if r.ComposeFile != "docker-compose.yml" || !r.RemoveOrphans || !r.ForcePull {
			t.Errorf("defaults wrong: %+v", r)
		}
	})

	t.Run("server compose_file overrides repo", func(t *testing.T) {
		t.Parallel()
		m := &Manifest{Compose: ComposeManifest{ComposeFile: "repo.yml"}}
		r := Merge(StackConfig{ComposeFile: "server.yml"}, m)
		if r.ComposeFile != "server.yml" {
			t.Errorf("compose_file = %q, want server.yml", r.ComposeFile)
		}
	})

	t.Run("repo compose_file when server empty", func(t *testing.T) {
		t.Parallel()
		m := &Manifest{Compose: ComposeManifest{ComposeFile: "repo.yml"}}
		if r := Merge(StackConfig{}, m); r.ComposeFile != "repo.yml" {
			t.Errorf("compose_file = %q, want repo.yml", r.ComposeFile)
		}
	})

	t.Run("force_pull precedence", func(t *testing.T) {
		t.Parallel()
		m := &Manifest{Compose: ComposeManifest{ForcePull: boolp(true)}}
		// server false beats repo true
		if r := Merge(StackConfig{ForcePull: boolp(false)}, m); r.ForcePull {
			t.Error("server force_pull=false must win over repo true")
		}
		// repo true when server unset
		if r := Merge(StackConfig{}, m); !r.ForcePull {
			t.Error("repo force_pull=true should apply when server unset")
		}
	})

	t.Run("remove_orphans from repo, default true", func(t *testing.T) {
		t.Parallel()
		if r := Merge(StackConfig{}, &Manifest{Compose: ComposeManifest{RemoveOrphans: boolp(false)}}); r.RemoveOrphans {
			t.Error("repo remove_orphans=false must apply")
		}
		if r := Merge(StackConfig{}, &Manifest{}); !r.RemoveOrphans {
			t.Error("remove_orphans should default true")
		}
	})

	t.Run("sops server overrides repo wholesale", func(t *testing.T) {
		t.Parallel()
		m := &Manifest{Sops: SopsManifest{Files: []string{"repo.enc.env"}, Mode: "tmpfs"}}
		// server sops_files set → server wins (files + mode)
		r := Merge(StackConfig{SopsFiles: []string{"srv.enc.env"}, SopsMode: "exec-env"}, m)
		if len(r.SopsFiles) != 1 || r.SopsFiles[0] != "srv.enc.env" || r.SopsMode != "exec-env" {
			t.Errorf("server sops should win: %+v / %s", r.SopsFiles, r.SopsMode)
		}
		// server unset → repo applies, its mode honoured
		r = Merge(StackConfig{}, m)
		if len(r.SopsFiles) != 1 || r.SopsFiles[0] != "repo.enc.env" || r.SopsMode != "tmpfs" {
			t.Errorf("repo sops should apply: %+v / %s", r.SopsFiles, r.SopsMode)
		}
		// repo sops with no mode → default exec-env
		r = Merge(StackConfig{}, &Manifest{Sops: SopsManifest{Files: []string{"x.enc.env"}}})
		if r.SopsMode != "exec-env" {
			t.Errorf("repo sops default mode = %q, want exec-env", r.SopsMode)
		}
	})

	t.Run("secrets + hooks pass through from repo", func(t *testing.T) {
		t.Parallel()
		m := &Manifest{
			Secrets:    SecretsManifest{Files: []string{"tls.enc.key"}},
			PreDeploy:  []string{".plico/pre.sh"},
			PostDeploy: []string{".plico/post.sh"},
		}
		r := Merge(StackConfig{}, m)
		if len(r.SecretFiles) != 1 || len(r.PreHooks) != 1 || len(r.PostHooks) != 1 {
			t.Errorf("secrets/hooks not passed through: %+v", r)
		}
	})

	t.Run("profiles/env_files/up_args pass through", func(t *testing.T) {
		t.Parallel()
		m := &Manifest{Compose: ComposeManifest{
			Profiles: []string{"a"}, EnvFiles: []string{"e.env"}, UpArgs: []string{"--wait"},
		}}
		r := Merge(StackConfig{}, m)
		want := []string{"--profile", "a", "--env-file", "e.env"}
		if got := r.TopLevelArgs(); !equalStrings(got, want) {
			t.Errorf("TopLevelArgs = %v, want %v", got, want)
		}
		if got := r.UpArgsResolved(); !equalStrings(got, []string{"--remove-orphans", "--wait"}) {
			t.Errorf("UpArgsResolved = %v", got)
		}
	})
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
