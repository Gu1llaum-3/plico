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
	"github.com/robfig/cron/v3"

	"github.com/Gu1llaum-3/plico/internal/notify"
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

type HeartbeatConfig struct {
	// URL is an outbound liveness push target (Uptime Kuma push,
	// Healthchecks.io, …), pinged while the daemon is healthy — a
	// dead-man's switch. Empty = disabled. Paste the monitor's push URL
	// verbatim (query string included); plico GETs it as-is.
	URL string `toml:"url"`
	// Interval between beats; nil = default 30s, min 5s when set. A pointer
	// so an explicit "0s" (typo) is rejected rather than silently defaulted.
	// Decoupled from poll_interval so a death is detected quickly.
	Interval *Duration `toml:"interval"`
}

// IntervalOr returns the configured beat interval, or fallback when unset.
func (h HeartbeatConfig) IntervalOr(fallback time.Duration) time.Duration {
	if h.Interval == nil {
		return fallback
	}
	return h.Interval.Duration
}

type ApiConfig struct {
	// Socket is the unix socket the client CLI talks to (F24).
	// Default: <base_dir>/plico.sock.
	Socket string `toml:"socket"`
}

type NtfyConfig struct {
	URL   string `toml:"url"`   // full topic URL, e.g. https://ntfy.sh/plico-prod
	Token string `toml:"token"` // optional, sent as Authorization: Bearer
	// Events this channel receives (F32). Empty = failure-oriented default
	// (pre_hook_failed, pre_hook_skipped, deploy_failed, window_missed,
	// git_sync_failed); "all" = everything. deploy_success, deploy_queued
	// and deploy_start are opt-in.
	Events []string `toml:"events"`
}

type WebhookConfig struct {
	URL    string   `toml:"url"`
	Token  string   `toml:"token"` // optional, sent as Authorization: Bearer
	Events []string `toml:"events"`
}

type SmtpConfig struct {
	Host     string   `toml:"host"`
	Port     int      `toml:"port"` // default 587
	From     string   `toml:"from"`
	To       []string `toml:"to"`
	Username string   `toml:"username"`
	Password string   `toml:"password"` // via ${ENV} interpolation
	Events   []string `toml:"events"`
}

type HooksConfig struct {
	PreDeployPath  string   `toml:"pre_deploy_path"`  // global fallback (F10)
	PostDeployPath string   `toml:"post_deploy_path"` // global fallback (F15)
	Timeout        Duration `toml:"timeout"`          // F13, default 10m
	// EnvPassthrough lists environment variable NAMES a hook may receive on
	// top of the safe baseline + DEPLOY_* (e.g. SOPS_AGE_KEY_FILE, AWS creds
	// for a restic backup). Everything else in the daemon environment is
	// withheld from repo-controlled hooks. Per-stack passthrough is added
	// (union) to this global list.
	EnvPassthrough []string `toml:"env_passthrough"`
}

type GitAuth struct {
	Username string `toml:"username"`
	Password string `toml:"password"` // token / app password, via ${ENV} interpolation
}

type GitConfig struct {
	Auths map[string]GitAuth `toml:"auths"` // key = host, e.g. "bitbucket.org"
}

type StackConfig struct {
	Name string `toml:"name"`
	Repo string `toml:"repo"`
	Ref  string `toml:"ref"` // default "main"
	// Path is a repo-relative subdirectory that becomes this stack's content
	// root (monorepo support): compose_file, sops_files and the .deploy hooks
	// resolve under it, and docker compose runs with it as its working
	// directory. Empty = the repo root (one repo == one stack, unchanged).
	// A stack rooted at a subdirectory only redeploys when a commit touches a
	// file under it (see the deploy pipeline's path-scoped change detection).
	Path          string   `toml:"path"`
	ComposeFile   string   `toml:"compose_file"` // default "docker-compose.yml"; relative to Path
	ForcePull     *bool    `toml:"force_pull"`   // default true (F17)
	SopsFiles     []string `toml:"sops_files"`   // repo-relative; empty = no sops
	SopsMode      string   `toml:"sops_mode"`    // "exec-env" (default) | "tmpfs"
	HookTimeout   Duration `toml:"hook_timeout"` // 0 = inherit [hooks].timeout
	VerifyTimeout Duration `toml:"verify_timeout"`
	// Schedule is a cron expression (5 fields or @daily/@every …) gating
	// WHEN this stack is processed (F5). Empty = inherit the global
	// schedule; "@poll" = opt out of a global schedule and run every poll
	// tick. Evaluated in the configured timezone (F8).
	Schedule string `toml:"schedule"`
	// Window is how long the deployment window stays open after each
	// schedule firing (F7); during the window every poll tick processes
	// the stack. 0 = inherit the global window (default 1h).
	Window Duration `toml:"window"`
	// Check enables out-of-window checks (F6): fetch + SHA diff at every
	// poll tick, notifying "deployment queued" once per pending revision,
	// without deploying. nil = inherit the global default (false). Only
	// meaningful with a schedule; ignored otherwise.
	Check *bool `toml:"check"`
	// EnvPassthrough is added (union) to [hooks].env_passthrough for this
	// stack's hooks, so a secret can be exposed only to the hook that needs
	// it rather than every stack's hooks.
	EnvPassthrough []string `toml:"env_passthrough"`
}

// HookEnvPassthrough is the union of the global and this stack's passthrough
// lists — the env var names this stack's hooks may receive beyond the
// baseline + DEPLOY_*. Duplicates are harmless: buildEnv dedups the final
// KEY=VALUE list downstream, so this stays a plain concatenation.
func (s StackConfig) HookEnvPassthrough(global []string) []string {
	return append(append([]string{}, global...), s.EnvPassthrough...)
}

// CheckEnabled resolves the *bool (false when unset after defaults).
func (s StackConfig) CheckEnabled() bool {
	return s.Check != nil && *s.Check
}

// ForcePullEnabled resolves the *bool default (true when unset).
func (s StackConfig) ForcePullEnabled() bool {
	return s.ForcePull == nil || *s.ForcePull
}

type Config struct {
	BaseDir              string   `toml:"base_dir"`
	StateFile            string   `toml:"state_file"`
	Timezone             string   `toml:"timezone"`
	PollInterval         Duration `toml:"poll_interval"`
	RunTimeout           Duration `toml:"run_timeout"`
	MaxConcurrentDeploys int      `toml:"max_concurrent_deploys"`
	Schedule             string   `toml:"schedule"` // global default for stacks (F7); empty = every poll tick
	Window               Duration `toml:"window"`   // global default window, 1h
	Check                bool     `toml:"check"`    // global default for out-of-window checks (F6)

	Log       LogConfig       `toml:"log"`
	Health    HealthConfig    `toml:"health"`
	Heartbeat HeartbeatConfig `toml:"heartbeat"`
	Api       ApiConfig       `toml:"api"`
	Ntfy      NtfyConfig      `toml:"ntfy"`
	Webhooks  []WebhookConfig `toml:"webhook"` // [[webhook]]
	Smtp      SmtpConfig      `toml:"smtp"`
	Hooks     HooksConfig     `toml:"hooks"`
	Git       GitConfig       `toml:"git"`

	// GitSyncAlertAfter fires git_sync_failed after N consecutive git sync
	// failures for a stack (0 disables; unset = 5).
	GitSyncAlertAfter *int `toml:"git_sync_alert_after"`

	Stacks []StackConfig `toml:"stack"`
}

var stackNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// envNameRe matches a bare environment variable NAME (not KEY=VALUE): the
// passthrough config lists names to let through, never assignments.
var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validateEnvNames(where string, names []string) error {
	for _, n := range names {
		if !envNameRe.MatchString(n) {
			return fmt.Errorf("%s: %q is not a valid variable name (list names to pass through, not KEY=VALUE)", where, n)
		}
	}
	return nil
}

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
	if c.Heartbeat.URL != "" && c.Heartbeat.Interval == nil {
		c.Heartbeat.Interval = &Duration{30 * time.Second} // unset → default; explicit 0 stays 0 (rejected in Validate)
	}
	if c.Api.Socket == "" && c.BaseDir != "" {
		c.Api.Socket = filepath.Join(c.BaseDir, "plico.sock")
	}
	if c.StateFile == "" && c.BaseDir != "" {
		c.StateFile = filepath.Join(c.BaseDir, "state.json")
	}
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	if c.Hooks.Timeout.Duration == 0 {
		c.Hooks.Timeout.Duration = 10 * time.Minute
	}
	if c.Smtp.Host != "" && c.Smtp.Port == 0 {
		c.Smtp.Port = 587
	}
	if c.Window.Duration == 0 {
		c.Window.Duration = time.Hour
	}
	// "@poll" (no schedule, run every poll tick) is normalized here, once:
	// after applyDefaults no consumer ever sees the sentinel.
	if c.Schedule == "@poll" {
		c.Schedule = ""
	}
	for i := range c.Stacks {
		st := &c.Stacks[i]
		if st.Ref == "" {
			st.Ref = "main"
		}
		// Schedule inheritance (F7): empty = global; "@poll" = explicit
		// opt-out back to every-poll-tick behavior.
		if st.Schedule == "" {
			st.Schedule = c.Schedule
		}
		if st.Schedule == "@poll" {
			st.Schedule = ""
		}
		if st.Window.Duration == 0 {
			st.Window = c.Window
		}
		if st.Check == nil {
			v := c.Check
			st.Check = &v
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
	if !filepath.IsAbs(c.Api.Socket) {
		return fmt.Errorf("api.socket must be an absolute path, got %q", c.Api.Socket)
	}
	if !filepath.IsAbs(c.StateFile) {
		return fmt.Errorf("state_file must be an absolute path, got %q", c.StateFile)
	}
	if err := validateEnvNames("hooks.env_passthrough", c.Hooks.EnvPassthrough); err != nil {
		return err
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		return fmt.Errorf("timezone: %w", err)
	}
	if c.PollInterval.Duration < 5*time.Second {
		return fmt.Errorf("poll_interval must be >= 5s, got %s", c.PollInterval.Duration)
	}
	if c.MaxConcurrentDeploys < 1 {
		return fmt.Errorf("max_concurrent_deploys must be >= 1, got %d", c.MaxConcurrentDeploys)
	}
	if c.Heartbeat.URL != "" {
		if err := validateHTTPURL(c.Heartbeat.URL); err != nil {
			return fmt.Errorf("heartbeat.url: %w", err)
		}
		if d := c.Heartbeat.IntervalOr(0); d < 5*time.Second {
			return fmt.Errorf("heartbeat.interval must be >= 5s, got %s", d)
		}
	}
	if c.Schedule != "" {
		if err := validateSchedule(c.Schedule); err != nil {
			return fmt.Errorf("schedule %q: %w", c.Schedule, err)
		}
	}
	// Windows are validated at their own level so the error blames the
	// right config section, and unconditionally so a bad value cannot lie
	// dormant on a stack that currently has no schedule.
	if c.Window.Duration < 0 {
		return fmt.Errorf("window must be positive, got %s", c.Window.Duration)
	}
	if c.Ntfy.URL != "" {
		if err := validateHTTPURL(c.Ntfy.URL); err != nil {
			return fmt.Errorf("ntfy.url: %w", err)
		}
	}
	if _, err := notify.ParseEvents(c.Ntfy.Events); err != nil {
		return fmt.Errorf("ntfy.events: %w", err)
	}
	for i, w := range c.Webhooks {
		if w.URL == "" {
			return fmt.Errorf("webhook[%d]: url is required", i)
		}
		if err := validateHTTPURL(w.URL); err != nil {
			return fmt.Errorf("webhook[%d].url: %w", i, err)
		}
		if _, err := notify.ParseEvents(w.Events); err != nil {
			return fmt.Errorf("webhook[%d].events: %w", i, err)
		}
	}
	if c.Smtp.Host != "" {
		if c.Smtp.From == "" || len(c.Smtp.To) == 0 {
			return fmt.Errorf("smtp: from and to are required")
		}
		if c.Smtp.Port < 1 || c.Smtp.Port > 65535 {
			return fmt.Errorf("smtp.port must be within 1-65535, got %d", c.Smtp.Port)
		}
		if _, err := notify.ParseEvents(c.Smtp.Events); err != nil {
			return fmt.Errorf("smtp.events: %w", err)
		}
	}
	if c.GitSyncAlertAfter != nil && *c.GitSyncAlertAfter < 0 {
		return fmt.Errorf("git_sync_alert_after must be >= 0, got %d", *c.GitSyncAlertAfter)
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
		if st.Window.Duration < 0 {
			return fmt.Errorf("stack %q: window must be positive, got %s", st.Name, st.Window.Duration)
		}
		if err := validateEnvNames(fmt.Sprintf("stack %q: env_passthrough", st.Name), st.EnvPassthrough); err != nil {
			return err
		}
		if st.Schedule != "" {
			if err := validateSchedule(st.Schedule); err != nil {
				return fmt.Errorf("stack %q: schedule %q: %w", st.Name, st.Schedule, err)
			}
		}
		if st.Path != "" && (filepath.IsAbs(st.Path) || escapesRepo(st.Path)) {
			return fmt.Errorf("stack %q: path %q must be a repo-relative subdirectory", st.Name, st.Path)
		}
		for _, f := range st.SopsFiles {
			if filepath.IsAbs(f) || escapesRepo(f) {
				return fmt.Errorf("stack %q: sops file %q must be a repo-relative path", st.Name, f)
			}
		}
		if filepath.IsAbs(st.ComposeFile) || escapesRepo(st.ComposeFile) {
			return fmt.Errorf("stack %q: compose_file %q must not escape the repo", st.Name, st.ComposeFile)
		}
	}
	return nil
}

// validateSchedule rejects both unparsable expressions and syntactically
// valid ones that never fire (e.g. "0 0 30 2 *", Feb 30): robfig/cron
// returns the zero time for those, which downstream code must never see.
func validateSchedule(expr string) error {
	sched, err := cron.ParseStandard(expr)
	if err != nil {
		return err
	}
	if sched.Next(time.Now()).IsZero() {
		return fmt.Errorf("expression never fires")
	}
	return nil
}

// validateHTTPURL rejects notification endpoints that would fail on every
// send: url.ParseRequestURI alone accepts scheme-less paths like
// "/services/T00/...", which a log-fallback-wrapped channel would then
// swallow silently at runtime.
func validateHTTPURL(raw string) error {
	u, err := url.ParseRequestURI(raw)
	if err != nil {
		return err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("must be an absolute http(s) URL, got %q", raw)
	}
	return nil
}

// GitSyncAlertThreshold resolves the *int (5 when unset, 0 = disabled).
func (c *Config) GitSyncAlertThreshold() int {
	if c.GitSyncAlertAfter == nil {
		return 5
	}
	return *c.GitSyncAlertAfter
}

// escapesRepo reports whether a relative path climbs out of the repo. It
// checks path segments, so filenames merely containing ".." (a..b.env) pass.
func escapesRepo(p string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(p), "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// Location returns the parsed timezone (validated beforehand).
func (c *Config) Location() *time.Location {
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return time.Local
	}
	return loc
}
