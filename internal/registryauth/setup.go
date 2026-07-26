package registryauth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Gu1llaum-3/plico/internal/config"
)

// dockerConfigFile is the subset of ~/.docker/config.json plico writes: only
// the credHelpers routing table, never an `auths` block — no credential is ever
// serialised here.
type dockerConfigFile struct {
	CredHelpers map[string]string `json:"credHelpers"`
}

// Setup provisions a dedicated DOCKER_CONFIG that routes each configured
// registry to plico's credential helper, and returns the environment additions
// for the compose subprocess plus a cleanup func.
//
// It writes only a credHelpers routing table (no secret): the token is resolved
// on demand by the helper (see Serve), which re-reads configPath. A
// docker-credential-plico symlink to selfExe is placed in the config dir and
// that dir is prepended to PATH so docker resolves the helper.
//
// It is opt-in: with no auths it returns (nil, noop, nil) and does not touch
// DOCKER_CONFIG, so the current host-delegated behaviour is unchanged. parentDir
// is where the ephemeral config dir is created (it holds no secret, so it need
// not be tmpfs). cleanup is always safe to call.
func Setup(auths map[string]config.RegistryAuth, parentDir, configPath, selfExe string) (env []string, cleanup func(), err error) {
	noop := func() {}
	if len(auths) == 0 {
		return nil, noop, nil
	}

	dir, err := os.MkdirTemp(parentDir, "plico-docker-")
	if err != nil {
		return nil, noop, fmt.Errorf("registry auth: creating config dir: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(dir) }

	helpers := make(map[string]string, len(auths))
	for host := range auths {
		helpers[credHelperKey(host)] = "plico" // docker-credential-plico
	}
	body, err := json.MarshalIndent(dockerConfigFile{CredHelpers: helpers}, "", "  ")
	if err != nil {
		cleanup()
		return nil, noop, fmt.Errorf("registry auth: encoding config: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), body, 0o600); err != nil {
		cleanup()
		return nil, noop, fmt.Errorf("registry auth: writing config: %w", err)
	}
	if err := os.Symlink(selfExe, filepath.Join(dir, HelperName)); err != nil {
		cleanup()
		return nil, noop, fmt.Errorf("registry auth: linking helper: %w", err)
	}

	env = []string{
		"DOCKER_CONFIG=" + dir,
		"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"),
		EnvFlag + "=1",
		EnvConfig + "=" + configPath,
	}
	return env, cleanup, nil
}
