// Package compose abstracts the compose runtime behind a small interface so
// a Podman implementation can be added later without touching the pipeline.
package compose

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"plico/internal/execx"
)

// Service is one service of a compose project as reported by `ps`.
type Service struct {
	Name     string
	State    string // "running", "exited", ...
	Health   string // "healthy", "unhealthy", "starting", "" (no healthcheck)
	ExitCode int
}

// Options describes one invocation against a stack.
type Options struct {
	Dir         string   // stack worktree
	ComposeFile string   // relative to Dir
	Project     string   // compose project name (-p), stable per stack
	CmdPrefix   []string // argv prefix, e.g. ["sops","exec-env","f.env","--"]; nil = none
	ExtraArgs   []string // e.g. --env-file /dev/shm/... in tmpfs mode
}

type Runtime interface {
	Pull(ctx context.Context, o Options) error
	Up(ctx context.Context, o Options) error
	PS(ctx context.Context, o Options) ([]Service, error)
}

// NewDocker returns the `docker compose` implementation.
func NewDocker(r execx.Runner) Runtime {
	return &docker{runner: r}
}

type docker struct {
	runner execx.Runner
}

func (d *docker) Pull(ctx context.Context, o Options) error {
	_, err := d.run(ctx, o, "pull")
	return err
}

func (d *docker) Up(ctx context.Context, o Options) error {
	_, err := d.run(ctx, o, "up", "-d", "--remove-orphans")
	return err
}

// PS deliberately addresses the project by name only (-p, no -f, no sops
// prefix, no env-files): inspecting running containers must not re-parse the
// compose file nor re-trigger secret decryption — a transient sops/KMS flake
// during the verify polls would otherwise fail a healthy deployment.
func (d *docker) PS(ctx context.Context, o Options) ([]Service, error) {
	res, err := d.runner.Run(ctx, execx.Cmd{
		Name: "docker",
		Args: []string{"compose", "-p", o.Project, "ps", "-a", "--format", "json"},
		Dir:  o.Dir,
	})
	if err != nil {
		return nil, err
	}
	return parsePS(res.Stdout)
}

// run builds `docker compose -f <file> -p <project> [extra] <verb...>` and
// prepends Options.CmdPrefix (the sops exec-env chain) when present.
func (d *docker) run(ctx context.Context, o Options, verb ...string) (execx.Result, error) {
	argv := []string{"docker", "compose", "-f", o.ComposeFile, "-p", o.Project}
	argv = append(argv, o.ExtraArgs...)
	argv = append(argv, verb...)
	if len(o.CmdPrefix) > 0 {
		argv = append(append([]string{}, o.CmdPrefix...), argv...)
	}
	return d.runner.Run(ctx, execx.Cmd{Name: argv[0], Args: argv[1:], Dir: o.Dir})
}

// psLine matches the JSON emitted by `docker compose ps --format json`
// (NDJSON since compose v2.21; a plain array before that).
type psLine struct {
	Service  string `json:"Service"`
	State    string `json:"State"`
	Health   string `json:"Health"`
	ExitCode int    `json:"ExitCode"`
}

func parsePS(out []byte) ([]Service, error) {
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return nil, nil
	}
	var lines []psLine
	if trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &lines); err != nil {
			return nil, fmt.Errorf("parsing compose ps output: %w", err)
		}
	} else {
		sc := bufio.NewScanner(bytes.NewReader(trimmed))
		sc.Buffer(make([]byte, 0, 64*1024), execx.MaxCapture)
		for sc.Scan() {
			line := bytes.TrimSpace(sc.Bytes())
			if len(line) == 0 {
				continue
			}
			var l psLine
			if err := json.Unmarshal(line, &l); err != nil {
				return nil, fmt.Errorf("parsing compose ps line %q: %w", line, err)
			}
			lines = append(lines, l)
		}
		if err := sc.Err(); err != nil {
			return nil, err
		}
	}
	services := make([]Service, len(lines))
	for i, l := range lines {
		services[i] = Service{Name: l.Service, State: l.State, Health: l.Health, ExitCode: l.ExitCode}
	}
	return services, nil
}
