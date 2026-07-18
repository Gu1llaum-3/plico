package compose

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/Gu1llaum-3/plico/internal/execx"
)

func TestArgvConstruction(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		opts     Options
		call     func(Runtime, Options) error
		wantName string
		wantArgs []string
	}{
		{
			name:     "up without prefix",
			opts:     Options{Dir: "/opt/docker/web", ComposeFile: "docker-compose.yml", Project: "web"},
			call:     func(rt Runtime, o Options) error { return rt.Up(context.Background(), o) },
			wantName: "docker",
			wantArgs: []string{"compose", "-f", "docker-compose.yml", "-p", "web", "up", "-d", "--remove-orphans"},
		},
		{
			name: "pull with a wrap (sops exec-env single-string form)",
			opts: Options{
				Dir: "/opt/docker/web", ComposeFile: "compose.yaml", Project: "web",
				Wrap: func(argv []string) []string {
					return []string{"sops", "exec-env", "a.enc.env", strings.Join(argv, " ")}
				},
			},
			call:     func(rt Runtime, o Options) error { return rt.Pull(context.Background(), o) },
			wantName: "sops",
			wantArgs: []string{"exec-env", "a.enc.env", "docker compose -f compose.yaml -p web pull"},
		},
		{
			name: "up with tmpfs env-files",
			opts: Options{
				Dir: "/opt/docker/web", ComposeFile: "docker-compose.yml", Project: "web",
				ExtraArgs: []string{"--env-file", "/dev/shm/plico/web-r1/secrets-0.env"},
			},
			call:     func(rt Runtime, o Options) error { return rt.Up(context.Background(), o) },
			wantName: "docker",
			wantArgs: []string{"compose", "-f", "docker-compose.yml", "-p", "web",
				"--env-file", "/dev/shm/plico/web-r1/secrets-0.env", "up", "-d", "--remove-orphans"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &execx.FakeRunner{Script: []execx.Response{{}}}
			if err := tt.call(NewDocker(fake), tt.opts); err != nil {
				t.Fatal(err)
			}
			c := fake.Calls[0]
			if c.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", c.Name, tt.wantName)
			}
			if !reflect.DeepEqual(c.Args, tt.wantArgs) {
				t.Errorf("Args = %v\nwant  %v", c.Args, tt.wantArgs)
			}
			if c.Dir != tt.opts.Dir {
				t.Errorf("Dir = %q, want %q", c.Dir, tt.opts.Dir)
			}
		})
	}
}

// PS must address the project by name only: no -f (no compose-file parsing),
// no sops prefix, no env-files — inspecting containers must never re-trigger
// secret decryption during the verify polls.
func TestPSBypassesComposeFileAndSopsPrefix(t *testing.T) {
	t.Parallel()
	fake := &execx.FakeRunner{Script: []execx.Response{{}}}
	opts := Options{
		Dir: "/opt/docker/web", ComposeFile: "docker-compose.yml", Project: "web",
		Wrap: func(argv []string) []string {
			return append([]string{"sops", "exec-env", "a.enc.env"}, strings.Join(argv, " "))
		},
		ExtraArgs: []string{"--env-file", "/dev/shm/x.env"},
	}
	if _, err := NewDocker(fake).PS(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	c := fake.Calls[0]
	if c.Name != "docker" {
		t.Errorf("PS must not go through the sops prefix, Name = %q", c.Name)
	}
	want := []string{"compose", "-p", "web", "ps", "-a", "--format", "json"}
	if !reflect.DeepEqual(c.Args, want) {
		t.Errorf("Args = %v\nwant  %v", c.Args, want)
	}
}

// Realistic NDJSON output of `docker compose ps -a --format json` (compose >= 2.21).
const ndjsonFixture = `{"Command":"nginx","ExitCode":0,"Health":"healthy","ID":"1a","Name":"web-nginx-1","Service":"nginx","State":"running"}
{"Command":"postgres","ExitCode":0,"Health":"starting","ID":"2b","Name":"web-db-1","Service":"db","State":"running"}
{"Command":"migrate.sh","ExitCode":0,"Health":"","ID":"3c","Name":"web-migrate-1","Service":"migrate","State":"exited"}
{"Command":"worker","ExitCode":137,"Health":"","ID":"4d","Name":"web-worker-1","Service":"worker","State":"exited"}
`

func TestParsePSNDJSON(t *testing.T) {
	t.Parallel()
	services, err := parsePS([]byte(ndjsonFixture))
	if err != nil {
		t.Fatal(err)
	}
	want := []Service{
		{Name: "nginx", State: "running", Health: "healthy"},
		{Name: "db", State: "running", Health: "starting"},
		{Name: "migrate", State: "exited"},
		{Name: "worker", State: "exited", ExitCode: 137},
	}
	if !reflect.DeepEqual(services, want) {
		t.Errorf("got  %+v\nwant %+v", services, want)
	}
}

func TestParsePSLegacyArray(t *testing.T) {
	t.Parallel()
	arr := `[{"Service":"nginx","State":"running","Health":"healthy","ExitCode":0}]`
	services, err := parsePS([]byte(arr))
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 1 || services[0].Name != "nginx" {
		t.Errorf("got %+v", services)
	}
}

func TestParsePSEmpty(t *testing.T) {
	t.Parallel()
	services, err := parsePS([]byte("  \n"))
	if err != nil || services != nil {
		t.Errorf("empty output: services=%v err=%v", services, err)
	}
}

func TestParsePSGarbage(t *testing.T) {
	t.Parallel()
	if _, err := parsePS([]byte("not json at all")); err == nil {
		t.Fatal("want parse error")
	}
}
