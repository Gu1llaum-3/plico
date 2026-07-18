package sopsx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"plico/internal/execx"
)

func TestExecEnvArgvNoFiles(t *testing.T) {
	t.Parallel()
	argv := []string{"docker", "compose", "up", "-d"}
	if got := ExecEnvArgv(nil, argv); !reflect.DeepEqual(got, argv) {
		t.Errorf("no files must return argv unchanged, got %v", got)
	}
}

// sops exec-env takes exactly two arguments: the file and the command as ONE
// shell string (run via sh -c). The wrapped command must be a single,
// correctly quoted argument.
func TestExecEnvArgvSingleFile(t *testing.T) {
	t.Parallel()
	got := ExecEnvArgv([]string{".deploy/secrets.enc.env"},
		[]string{"docker", "compose", "-f", "docker-compose.yml", "-p", "web", "up", "-d"})
	want := []string{"sops", "exec-env", ".deploy/secrets.enc.env",
		"'docker' 'compose' '-f' 'docker-compose.yml' '-p' 'web' 'up' '-d'"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestExecEnvArgvNestedFiles(t *testing.T) {
	t.Parallel()
	got := ExecEnvArgv([]string{"a.enc.env", "b.enc.env"}, []string{"docker", "compose", "pull"})
	if len(got) != 4 || got[0] != "sops" || got[1] != "exec-env" || got[2] != "a.enc.env" {
		t.Fatalf("outer level wrong: %q", got)
	}
	// The inner command is one string: `sops exec-env 'b.enc.env' '<quoted docker cmd>'`.
	inner := got[3]
	if !strings.HasPrefix(inner, "sops exec-env 'b.enc.env' ") {
		t.Errorf("b.enc.env must be the inner (winning) layer: %q", inner)
	}
	if !strings.Contains(inner, "docker") || !strings.Contains(inner, "pull") {
		t.Errorf("wrapped command lost: %q", inner)
	}
}

func TestShQuote(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"plain", "'plain'"},
		{"with space", "'with space'"},
		{"it's", `'it'\''s'`},
		{"$VAR `cmd` \"q\"", "'$VAR `cmd` \"q\"'"}, // no expansion inside single quotes
	}
	for _, tt := range tests {
		if got := shQuote(tt.in); got != tt.want {
			t.Errorf("shQuote(%q) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

func TestDecryptToTmpfs(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// The fake "sops decrypt --output <out> <in>" writes the output file.
	fake := &execx.FakeRunner{Match: func(c execx.Cmd) (execx.Result, error) {
		if c.Name != "sops" || c.Args[0] != "decrypt" || c.Args[1] != "--output" {
			t.Errorf("unexpected command: %s %v", c.Name, c.Args)
		}
		if err := os.WriteFile(c.Args[2], []byte("KEY=val\n"), 0o600); err != nil {
			return execx.Result{}, err
		}
		return execx.Result{}, nil
	}}

	env, err := DecryptToTmpfs(context.Background(), fake, []string{"/repo/a.enc.env", "/repo/b.enc.env"}, root, "web", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(env.Args) != 4 || env.Args[0] != "--env-file" {
		t.Fatalf("Args = %v", env.Args)
	}
	dir := filepath.Join(root, "plico", "web-r1")
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("dir perms = %v, want 0700", info.Mode().Perm())
	}
	finfo, err := os.Stat(env.Args[1])
	if err != nil {
		t.Fatal(err)
	}
	if finfo.Mode().Perm() != 0o600 {
		t.Errorf("file perms = %v, want 0600", finfo.Mode().Perm())
	}

	env.Cleanup()
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Error("cleanup did not remove the tmpfs dir")
	}
}

func TestDecryptToTmpfsFailureCleansUp(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	fake := &execx.FakeRunner{Match: func(c execx.Cmd) (execx.Result, error) {
		return execx.Result{ExitCode: 1}, errors.New("sops: no key found")
	}}
	if _, err := DecryptToTmpfs(context.Background(), fake, []string{"/repo/a.enc.env"}, root, "web", "r1"); err == nil {
		t.Fatal("want decryption error")
	}
	if _, err := os.Stat(filepath.Join(root, "plico", "web-r1")); !errors.Is(err, os.ErrNotExist) {
		t.Error("failed decryption must remove the tmpfs dir")
	}
}
