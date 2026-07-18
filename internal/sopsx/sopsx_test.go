package sopsx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"plico/internal/execx"
)

func TestPrefix(t *testing.T) {
	t.Parallel()
	if p := Prefix(nil); p != nil {
		t.Errorf("Prefix(nil) = %v, want nil", p)
	}
	got := Prefix([]string{"a.enc.env", "b.enc.env"})
	want := []string{"sops", "exec-env", "a.enc.env", "--", "sops", "exec-env", "b.enc.env", "--"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Prefix = %v\nwant   %v", got, want)
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
