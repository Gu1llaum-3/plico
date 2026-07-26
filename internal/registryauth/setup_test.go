package registryauth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Gu1llaum-3/plico/internal/config"
)

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, e := range env {
		if i := strings.IndexByte(e, '='); i >= 0 {
			m[e[:i]] = e[i+1:]
		}
	}
	return m
}

func TestSetupEmptyIsNoop(t *testing.T) {
	t.Parallel()
	env, cleanup, err := Setup(nil, t.TempDir(), "/etc/plico/config.toml", "/usr/local/bin/plico")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if env != nil {
		t.Errorf("no auths should yield no env, got %v", env)
	}
}

func TestSetupProvisionsHelperConfig(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	self := filepath.Join(t.TempDir(), "plico") // symlink target need not exist
	auths := map[string]config.RegistryAuth{
		"ghcr.io":   {Username: "bot", Password: "tok"},
		"docker.io": {Username: "u", Password: "p"},
	}
	env, cleanup, err := Setup(auths, parent, "/etc/plico/config.toml", self)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	m := envMap(env)
	dir := m["DOCKER_CONFIG"]
	if dir == "" {
		t.Fatal("DOCKER_CONFIG not set")
	}

	// config.json routes each registry to the plico helper, no secret in it.
	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "tok") || strings.Contains(string(raw), "\"p\"") {
		t.Errorf("config.json must not contain any secret: %s", raw)
	}
	var dc struct {
		CredHelpers map[string]string `json:"credHelpers"`
	}
	if err := json.Unmarshal(raw, &dc); err != nil {
		t.Fatal(err)
	}
	if dc.CredHelpers["ghcr.io"] != "plico" {
		t.Errorf("ghcr.io not routed to plico: %v", dc.CredHelpers)
	}
	// Docker Hub must be keyed by the canonical index URL, not "docker.io".
	if dc.CredHelpers["https://index.docker.io/v1/"] != "plico" {
		t.Errorf("docker hub not routed under the index key: %v", dc.CredHelpers)
	}

	// The helper binary is discoverable as docker-credential-plico on PATH.
	link := filepath.Join(dir, HelperName)
	if target, err := os.Readlink(link); err != nil || target != self {
		t.Errorf("symlink %s -> %q, err=%v (want %q)", HelperName, target, err, self)
	}
	if !strings.HasPrefix(m["PATH"], dir+string(os.PathListSeparator)) {
		t.Errorf("PATH not prefixed with helper dir: %q", m["PATH"])
	}
	if m[EnvFlag] != "1" {
		t.Errorf("%s not set", EnvFlag)
	}
	if m[EnvConfig] != "/etc/plico/config.toml" {
		t.Errorf("%s = %q", EnvConfig, m[EnvConfig])
	}

	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("cleanup did not remove %s", dir)
	}
}
