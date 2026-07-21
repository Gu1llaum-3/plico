package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Manifest is the repo-side deployment recipe (`.plico.toml`), read from the
// stack's content root at the deployed revision. It carries only the
// NON-SENSITIVE recipe: trust and bootstrap (repo, ref, auth, base_dir,
// env_passthrough, sops_mode, schedule…) live exclusively in the server
// plico.toml and are not expressible here — a repo can never grant itself a
// secret or a privilege (the age key, git auth and env_passthrough stay
// server-side). Encrypted files live IN the repo; the manifest only declares
// WHICH ones to decrypt.
type Manifest struct {
	Compose ComposeManifest `toml:"compose"`
	// Sops declares repo-committed, sops-encrypted ENV files to inject as
	// environment (sops exec-env / tmpfs --env-file), for compose ${VAR}
	// interpolation. The server's sops_files, when set, override this.
	Sops SopsManifest `toml:"sops"`
	// Secrets declares repo-committed, sops-encrypted FILES to decrypt onto a
	// plico-controlled tmpfs dir (exposed as $PLICO_SECRETS_DIR) so a container
	// can bind-mount them — for certs/keys an app reads as files, not env.
	Secrets SecretsManifest `toml:"secrets"`
	// PreDeploy / PostDeploy are ordered, repo-relative hook scripts. Pre-hooks
	// are a blocking gate (first non-zero exit aborts); post-hooks are
	// non-blocking. They run in the same server-scoped environment as the
	// default .plico/*.sh hooks (no daemon secrets unless env_passthrough).
	PreDeploy  []string `toml:"pre_deploy"`
	PostDeploy []string `toml:"post_deploy"`
}

// ComposeManifest holds the per-stack compose options a repo may set.
type ComposeManifest struct {
	ComposeFile   string   `toml:"compose_file"`   // relative to the content root; server compose_file overrides
	Profiles      []string `toml:"profiles"`       // --profile, applied to pull AND up
	EnvFiles      []string `toml:"env_files"`      // extra --env-file, repo-relative to the content root
	RemoveOrphans *bool    `toml:"remove_orphans"` // default true
	ForcePull     *bool    `toml:"force_pull"`     // default true; server force_pull overrides
	UpArgs        []string `toml:"up_args"`        // extra args appended after `up -d`
}

// SopsManifest declares encrypted env files and the decryption mode.
type SopsManifest struct {
	Files []string `toml:"files"` // repo-relative, sops-encrypted dotenv files
	Mode  string   `toml:"mode"`  // "exec-env" (default) | "tmpfs"
}

// SecretsManifest declares encrypted files to materialise on tmpfs.
type SecretsManifest struct {
	Files []string `toml:"files"` // repo-relative, sops-encrypted files
}

// Recipe is the resolved, non-pointer result of merging a stack's server config
// with its repo manifest. It is what the deploy pipeline consumes.
type Recipe struct {
	ComposeFile   string
	Profiles      []string
	EnvFiles      []string
	RemoveOrphans bool
	ForcePull     bool
	UpArgs        []string
	SopsFiles     []string
	SopsMode      string
	SecretFiles   []string
	PreHooks      []string
	PostHooks     []string
}

// TopLevelArgs are the flags that belong BEFORE the compose subcommand, so they
// apply to both `pull` and `up`: --profile (repeated) then --env-file
// (repeated). Paths are repo-relative and resolve against the compose working
// directory (the content root).
func (r Recipe) TopLevelArgs() []string {
	var a []string
	for _, p := range r.Profiles {
		a = append(a, "--profile", p)
	}
	for _, f := range r.EnvFiles {
		a = append(a, "--env-file", f)
	}
	return a
}

// UpArgsResolved are the args appended AFTER `up -d`: --remove-orphans (unless
// disabled) then any manifest up_args.
func (r Recipe) UpArgsResolved() []string {
	var a []string
	if r.RemoveOrphans {
		a = append(a, "--remove-orphans")
	}
	return append(a, r.UpArgs...)
}

// upArgAllowed is the ALLOWLIST of `docker compose up` flags a repo manifest
// may pass. An allowlist (not a blacklist) is deliberate: pflag honours root
// persistent flags after the subcommand and accepts bundled/short forms
// (-pfoo, -dp foo, -df x), so a blacklist of canonical spellings is bypassable
// — a repo could reclaim -p (the project name that isolates stacks) or inject
// -f/--project-directory. Entries must be long-form --flag or --flag=value;
// short flags and bare value tokens are rejected (so no bundling is possible).
var upArgAllowed = map[string]bool{
	"--wait": true, "--wait-timeout": true,
	"--build": true, "--no-build": true,
	"--force-recreate": true, "--no-recreate": true,
	"--no-deps":            true,
	"--pull":               true,
	"--quiet-pull":         true,
	"--scale":              true,
	"--timeout":            true,
	"--renew-anon-volumes": true,
}

// ResolveManifest applies the layout rule under root and returns the manifest
// path ("" = no manifest, identical to today's behaviour). It fails loudly on
// an ambiguous or half-declared layout.
func ResolveManifest(root string) (string, error) {
	fileP := filepath.Join(root, ".plico.toml")
	dirP := filepath.Join(root, ".plico")
	dirManifest := filepath.Join(dirP, "plico.toml")

	// Symlinks are rejected loudly: a repo could otherwise point .plico.toml
	// (or .plico/) at a host path, escaping the repo and — via a decode error
	// echoing a source line — leaking host-file content into a notification.
	fileOK, err := isRegularNoSymlink(fileP)
	if err != nil {
		return "", err
	}
	dirOK, err := isDirNoSymlink(dirP)
	if err != nil {
		return "", err
	}
	switch {
	case fileOK && dirOK:
		return "", fmt.Errorf("both .plico.toml and .plico/ exist under the stack root; keep only one")
	case fileOK:
		return fileP, nil
	case dirOK:
		// .plico/ is plico's directory: it may hold the manifest AND/OR hook
		// scripts (.plico/pre-deploy.sh, …). A .plico/ without plico.toml is
		// valid (hooks only) — not an error, just "no manifest".
		mOK, err := isRegularNoSymlink(dirManifest)
		if err != nil {
			return "", err
		}
		if !mOK {
			return "", nil
		}
		return dirManifest, nil
	default:
		return "", nil
	}
}

// LoadManifest decodes and validates a manifest. It does NOT interpolate ${ENV}
// (that would leak the daemon environment into a less-trusted artifact); any
// ${VAR} is left literal for docker compose to resolve.
func LoadManifest(path string) (*Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	md, err := toml.Decode(string(raw), &m)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if undec := md.Undecoded(); len(undec) > 0 {
		keys := make([]string, len(undec))
		for i, k := range undec {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("%s: unknown key(s): %s", path, strings.Join(keys, ", "))
	}
	if err := m.validate(path); err != nil {
		return nil, err
	}
	return &m, nil
}

func (m *Manifest) validate(path string) error {
	c := m.Compose
	if c.ComposeFile != "" && (filepath.IsAbs(c.ComposeFile) || escapesRepo(c.ComposeFile)) {
		return fmt.Errorf("%s: compose_file %q must be a repo-relative path", path, c.ComposeFile)
	}
	for _, f := range c.EnvFiles {
		if filepath.IsAbs(f) || escapesRepo(f) {
			return fmt.Errorf("%s: env_file %q must be a repo-relative path", path, f)
		}
	}
	for _, a := range c.UpArgs {
		if !strings.HasPrefix(a, "--") {
			return fmt.Errorf("%s: up_args entry %q must be a long-form --flag or --flag=value (short/bundled flags are not allowed)", path, a)
		}
		name := a
		if i := strings.IndexByte(name, '='); i >= 0 {
			name = name[:i]
		}
		if !upArgAllowed[name] {
			return fmt.Errorf("%s: up_args flag %q is not allowed (plico owns -d/-f/-p/--remove-orphans; use profiles/env_files/remove_orphans instead)", path, a)
		}
	}
	if m.Sops.Mode != "" && m.Sops.Mode != "exec-env" && m.Sops.Mode != "tmpfs" {
		return fmt.Errorf("%s: sops.mode must be \"exec-env\" or \"tmpfs\", got %q", path, m.Sops.Mode)
	}
	repoRel := func(kind string, files []string) error {
		for _, f := range files {
			if filepath.IsAbs(f) || escapesRepo(f) {
				return fmt.Errorf("%s: %s %q must be a repo-relative path", path, kind, f)
			}
		}
		return nil
	}
	for _, chk := range []struct {
		kind  string
		files []string
	}{
		{"sops file", m.Sops.Files},
		{"secret file", m.Secrets.Files},
		{"pre_deploy hook", m.PreDeploy},
		{"post_deploy hook", m.PostDeploy},
	} {
		if err := repoRel(chk.kind, chk.files); err != nil {
			return err
		}
	}
	// Secret files are decrypted to fixed names (`.enc` stripped); reject
	// degenerate names and collisions so a container never silently mounts the
	// wrong secret. Mirrors sopsx.decryptedName.
	seen := map[string]bool{}
	for _, f := range m.Secrets.Files {
		n := decryptedSecretName(f)
		if n == "" || n == "." || n == ".." {
			return fmt.Errorf("%s: secret file %q decrypts to an invalid name %q", path, f, n)
		}
		if seen[n] {
			return fmt.Errorf("%s: secret files collide on decrypted name %q", path, n)
		}
		seen[n] = true
	}
	return nil
}

// decryptedSecretName mirrors sopsx.decryptedName: the source basename with a
// single ".enc" marker removed. Kept here (not imported) to avoid a config→sops
// dependency; the two must stay in sync.
func decryptedSecretName(f string) string {
	base := filepath.Base(f)
	if strings.HasSuffix(base, ".enc") {
		return base[:len(base)-len(".enc")]
	}
	return strings.Replace(base, ".enc.", ".", 1)
}

// Merge resolves a stack's effective recipe. server config overrides the repo
// manifest; a nil manifest yields exactly the pre-manifest behaviour.
func Merge(st StackConfig, m *Manifest) Recipe {
	r := Recipe{
		ComposeFile:   "docker-compose.yml",
		RemoveOrphans: true,
		ForcePull:     true,
	}
	if m != nil {
		if m.Compose.ComposeFile != "" {
			r.ComposeFile = m.Compose.ComposeFile
		}
		if m.Compose.RemoveOrphans != nil {
			r.RemoveOrphans = *m.Compose.RemoveOrphans
		}
		if m.Compose.ForcePull != nil {
			r.ForcePull = *m.Compose.ForcePull
		}
		r.Profiles = m.Compose.Profiles
		r.EnvFiles = m.Compose.EnvFiles
		r.UpArgs = m.Compose.UpArgs
		r.SecretFiles = m.Secrets.Files
		r.PreHooks = m.PreDeploy
		r.PostHooks = m.PostDeploy
	}
	// Server overrides win over the repo.
	if st.ComposeFile != "" {
		r.ComposeFile = st.ComposeFile
	}
	if st.ForcePull != nil {
		r.ForcePull = *st.ForcePull
	}
	// sops env files are all-or-nothing per source (not a field-by-field
	// merge): if the operator declared sops_files server-side, that wins
	// wholesale (files + mode); otherwise the repo manifest's [sops] applies.
	switch {
	case len(st.SopsFiles) > 0:
		r.SopsFiles = st.SopsFiles
		r.SopsMode = st.SopsMode // defaulted to exec-env at config load
	case m != nil && len(m.Sops.Files) > 0:
		r.SopsFiles = m.Sops.Files
		r.SopsMode = m.Sops.Mode
		if r.SopsMode == "" {
			r.SopsMode = "exec-env"
		}
	}
	return r
}

// isRegularNoSymlink reports whether p is a regular file, treating a symlink as
// a hard error (never followed). A missing path is (false, nil).
func isRegularNoSymlink(p string) (bool, error) {
	fi, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("%s must not be a symlink", p)
	}
	return fi.Mode().IsRegular(), nil
}

// isDirNoSymlink reports whether p is a directory, treating a symlink as a hard
// error. A missing path is (false, nil).
func isDirNoSymlink(p string) (bool, error) {
	fi, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("%s must not be a symlink", p)
	}
	return fi.IsDir(), nil
}
