// Package registryauth lets plico authenticate `docker compose pull` against
// private registries the same way gitrepo authenticates git: without ever
// writing a credential to disk.
//
// Mechanism: plico is a Docker credential helper. Setup writes a DOCKER_CONFIG
// whose credHelpers route the configured registries to a `docker-credential-
// plico` symlink of the plico binary, and points the compose subprocess at it.
// When docker needs a credential it execs that helper (main() detects EnvFlag
// and calls Serve), which reads the registry host on stdin and returns the
// token — resolved on demand from the config, never materialised.
package registryauth

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/Gu1llaum-3/plico/internal/config"
)

// Contract between the compose subprocess environment and main()'s helper mode.
const (
	// EnvFlag, set on the compose subprocess, tells main() to run as the
	// credential helper instead of the CLI.
	EnvFlag = "PLICO_INTERNAL_CREDHELPER"
	// EnvConfig points the helper at the config file to resolve tokens from.
	EnvConfig = "PLICO_CREDHELPER_CONFIG"
	// HelperName is the on-PATH binary name docker resolves from credHelpers.
	HelperName = "docker-credential-plico"

	// dockerHubKey is the credHelpers/get key docker uses for Docker Hub.
	dockerHubKey = "https://index.docker.io/v1/"
	// notFoundMessage is docker's sentinel: on this exact stdout + non-zero
	// exit, docker treats the registry as having no credentials and proceeds
	// anonymously instead of failing the pull.
	notFoundMessage = "credentials not found in native keychain"
)

// cred is the JSON docker expects from a credential helper `get` (field names
// are the docker-credential-helpers contract: ServerURL/Username/Secret).
type cred struct {
	ServerURL string
	Username  string
	Secret    string
}

// canonicalHost is config.CanonicalRegistryHost — the single shared definition,
// so routing (Setup) and resolution (here) can never disagree.
func canonicalHost(s string) string { return config.CanonicalRegistryHost(s) }

// credHelperKey is the key to write under credHelpers for a configured host so
// docker routes that registry's requests to us. Docker Hub needs its canonical
// index URL; every other host is written in canonical form (scheme/path
// stripped) so a config value like "https://ghcr.io" still matches the bare
// "ghcr.io" docker looks up.
func credHelperKey(host string) string {
	ch := canonicalHost(host)
	if ch == "docker.io" {
		return dockerHubKey
	}
	return ch
}

// lookup finds the auth for the registry docker asked about, matching by
// canonical host so a config keyed "docker.io" answers an index.docker.io get.
// An entry whose username or token is empty is treated as unconfigured (a live
// config edit can add such an entry after startup validation), so docker falls
// back to an anonymous pull rather than authenticating with a blank credential.
func lookup(auths map[string]config.RegistryAuth, server string) (config.RegistryAuth, bool) {
	want := canonicalHost(server)
	for h, a := range auths {
		if canonicalHost(h) == want {
			if a.Username == "" || a.Password == "" {
				return config.RegistryAuth{}, false
			}
			return a, true
		}
	}
	return config.RegistryAuth{}, false
}

// Serve handles one credential-helper invocation and returns the process exit
// code. load resolves the configured auths on demand (re-read per call so a
// config edit is honoured without restarting the daemon).
func Serve(args []string, in io.Reader, out, errOut io.Writer, load func() (map[string]config.RegistryAuth, error)) int {
	verb := ""
	if len(args) > 0 {
		verb = args[0]
	}
	switch verb {
	case "get":
		return handleGet(in, out, errOut, load)
	case "store", "erase":
		// plico is a read-only credential source: swallow the request body so a
		// stray `docker login`/`logout` against our helper cannot error a run.
		_, _ = io.Copy(io.Discard, in)
		return 0
	case "list":
		// `get` is authoritative; we do not enumerate stored usernames.
		_, _ = fmt.Fprintln(out, "{}")
		return 0
	default:
		_, _ = fmt.Fprintf(errOut, "plico credential helper: unknown verb %q\n", verb)
		return 1
	}
}

func handleGet(in io.Reader, out, errOut io.Writer, load func() (map[string]config.RegistryAuth, error)) int {
	b, err := io.ReadAll(io.LimitReader(in, 4096))
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "plico credential helper: reading request: %v\n", err)
		return 1
	}
	server := strings.TrimSpace(string(b))
	auths, err := load()
	if err != nil {
		// A transient read error (e.g. an atomic config rewrite) must not abort
		// the whole pull — including public images on this registry. Emit the
		// not-found sentinel so docker falls back to an anonymous pull; the
		// reason is logged to stderr for diagnosis.
		_, _ = fmt.Fprintf(errOut, "plico credential helper: loading config: %v\n", err)
		_, _ = fmt.Fprintln(out, notFoundMessage)
		return 1
	}
	a, ok := lookup(auths, server)
	if !ok {
		_, _ = fmt.Fprintln(out, notFoundMessage)
		return 1
	}
	if err := json.NewEncoder(out).Encode(cred{ServerURL: server, Username: a.Username, Secret: a.Password}); err != nil {
		_, _ = fmt.Fprintf(errOut, "plico credential helper: encoding response: %v\n", err)
		return 1
	}
	return 0
}
