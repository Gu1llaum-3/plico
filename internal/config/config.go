// Package config loads and validates the plico TOML configuration.
package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Duration wraps time.Duration to accept TOML strings like "30s" or "5m".
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

type LogConfig struct {
	Path  string `toml:"path"`  // "" = stderr
	Level string `toml:"level"` // debug|info|warn|error, default info
}

type HealthConfig struct {
	Listen string `toml:"listen"` // default 127.0.0.1:9444
}

type NtfyConfig struct {
	URL   string `toml:"url"`   // full topic URL, e.g. https://ntfy.sh/plico-prod
	Token string `toml:"token"` // optional, sent as Authorization: Bearer
}

type HooksConfig struct {
	PreDeployPath  string   `toml:"pre_deploy_path"`  // global fallback (F10)
	PostDeployPath string   `toml:"post_deploy_path"` // global fallback (F15)
	Timeout        Duration `toml:"timeout"`          // F13, default 10m
}

type GitAuth struct {
	Username string `toml:"username"`
	Password string `toml:"password"` // token / app password, via ${ENV} interpolation
}

type GitConfig struct {
	Auths map[string]GitAuth `toml:"auths"` // key = host, e.g. "bitbucket.org"
}

type StackConfig struct {
	Name          string   `toml:"name"`
	Repo          string   `toml:"repo"`
	Ref           string   `toml:"ref"`          // default "main"
	ComposeFile   string   `toml:"compose_file"` // default "docker-compose.yml"
	ForcePull     *bool    `toml:"force_pull"`   // default true (F17)
	SopsFiles     []string `toml:"sops_files"`   // repo-relative; empty = no sops
	SopsMode      string   `toml:"sops_mode"`    // "exec-env" (default) | "tmpfs"
	HookTimeout   Duration `toml:"hook_timeout"` // 0 = inherit [hooks].timeout
	VerifyTimeout Duration `toml:"verify_timeout"`
}

// ForcePullEnabled resolves the *bool default (true when unset).
func (s StackConfig) ForcePullEnabled() bool {
	return s.ForcePull == nil || *s.ForcePull
}

type Config struct {
	BaseDir              string   `toml:"base_dir"`
	Timezone             string   `toml:"timezone"`
	PollInterval         Duration `toml:"poll_interval"`
	RunTimeout           Duration `toml:"run_timeout"`
	MaxConcurrentDeploys int      `toml:"max_concurrent_deploys"`

	Log    LogConfig    `toml:"log"`
	Health HealthConfig `toml:"health"`
	Ntfy   NtfyConfig   `toml:"ntfy"`
	Hooks  HooksConfig  `toml:"hooks"`
	Git    GitConfig    `toml:"git"`

	Stacks []StackConfig `toml:"stack"`
}

var stackNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// Load reads path, interpolates ${ENV_VAR} references, decodes the TOML,
// applies defaults and validates. Unknown keys are an error.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text, err := Interpolate(string(raw))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var cfg Config
	md, err := toml.Decode(text, &cfg)
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
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.PollInterval.Duration == 0 {
		c.PollInterval.Duration = 60 * time.Second
	}
	if c.RunTimeout.Duration == 0 {
		c.RunTimeout.Duration = 30 * time.Minute
	}
	if c.MaxConcurrentDeploys == 0 {
		c.MaxConcurrentDeploys = 2
	}
	if c.Timezone == "" {
		c.Timezone = "Local"
	}
	if c.Health.Listen == "" {
		c.Health.Listen = "127.0.0.1:9444"
	}
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	if c.Hooks.Timeout.Duration == 0 {
		c.Hooks.Timeout.Duration = 10 * time.Minute
	}
	for i := range c.Stacks {
		st := &c.Stacks[i]
		if st.Ref == "" {
			st.Ref = "main"
		}
		if st.ComposeFile == "" {
			st.ComposeFile = "docker-compose.yml"
		}
		if st.SopsMode == "" {
			st.SopsMode = "exec-env"
		}
		if st.HookTimeout.Duration == 0 {
			st.HookTimeout = c.Hooks.Timeout
		}
		if st.VerifyTimeout.Duration == 0 {
			st.VerifyTimeout.Duration = 90 * time.Second
		}
	}
}

// Validate checks the whole config; it returns the first error found.
func (c *Config) Validate() error {
	if c.BaseDir == "" {
		return fmt.Errorf("base_dir is required")
	}
	if !filepath.IsAbs(c.BaseDir) {
		return fmt.Errorf("base_dir must be an absolute path, got %q", c.BaseDir)
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		return fmt.Errorf("timezone: %w", err)
	}
	if c.PollInterval.Duration < 5*time.Second {
		return fmt.Errorf("poll_interval must be >= 5s, got %s", c.PollInterval.Duration)
	}
	if c.Ntfy.URL != "" {
		if _, err := url.ParseRequestURI(c.Ntfy.URL); err != nil {
			return fmt.Errorf("ntfy.url: %w", err)
		}
	}
	if len(c.Stacks) == 0 {
		return fmt.Errorf("at least one [[stack]] is required")
	}
	seen := map[string]bool{}
	for i, st := range c.Stacks {
		where := fmt.Sprintf("stack[%d]", i)
		if st.Name == "" {
			return fmt.Errorf("%s: name is required", where)
		}
		if !stackNameRe.MatchString(st.Name) {
			return fmt.Errorf("%s: invalid name %q (must match %s)", where, st.Name, stackNameRe)
		}
		if seen[st.Name] {
			return fmt.Errorf("duplicate stack name %q", st.Name)
		}
		seen[st.Name] = true
		if st.Repo == "" {
			return fmt.Errorf("stack %q: repo is required", st.Name)
		}
		if st.SopsMode != "exec-env" && st.SopsMode != "tmpfs" {
			return fmt.Errorf("stack %q: sops_mode must be \"exec-env\" or \"tmpfs\", got %q", st.Name, st.SopsMode)
		}
		for _, f := range st.SopsFiles {
			if filepath.IsAbs(f) || strings.Contains(f, "..") {
				return fmt.Errorf("stack %q: sops file %q must be a repo-relative path", st.Name, f)
			}
		}
		if strings.Contains(st.ComposeFile, "..") {
			return fmt.Errorf("stack %q: compose_file %q must not escape the repo", st.Name, st.ComposeFile)
		}
	}
	return nil
}

// Location returns the parsed timezone (validated beforehand).
func (c *Config) Location() *time.Location {
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return time.Local
	}
	return loc
}
