package registryauth

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Gu1llaum-3/plico/internal/config"
)

func TestCanonicalHost(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"ghcr.io":                     "ghcr.io",
		"GHCR.IO":                     "ghcr.io",
		"registry.example.com:5000":   "registry.example.com:5000",
		"docker.io":                   "docker.io",
		"index.docker.io":             "docker.io",
		"registry-1.docker.io":        "docker.io",
		"https://index.docker.io/v1/": "docker.io",
		"https://ghcr.io/v2/":         "ghcr.io",
		"  ghcr.io\n":                 "ghcr.io",
	}
	for in, want := range cases {
		if got := canonicalHost(in); got != want {
			t.Errorf("canonicalHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCredHelperKey(t *testing.T) {
	t.Parallel()
	if got := credHelperKey("docker.io"); got != "https://index.docker.io/v1/" {
		t.Errorf("docker.io key = %q", got)
	}
	if got := credHelperKey("index.docker.io"); got != "https://index.docker.io/v1/" {
		t.Errorf("index.docker.io key = %q", got)
	}
	if got := credHelperKey("ghcr.io"); got != "ghcr.io" {
		t.Errorf("ghcr.io key = %q", got)
	}
	// A host written with a scheme must still route under the bare host docker
	// looks up, not verbatim (else the pull silently goes anonymous).
	if got := credHelperKey("https://ghcr.io"); got != "ghcr.io" {
		t.Errorf("https://ghcr.io key = %q, want ghcr.io", got)
	}
}

func TestServeGetHostWithSchemeRoutes(t *testing.T) {
	t.Parallel()
	// Config keyed "https://ghcr.io"; docker asks with the bare "ghcr.io".
	auths := map[string]config.RegistryAuth{"https://ghcr.io": {Username: "bot", Password: "tok"}}
	code, out, _ := serve(t, "get", "ghcr.io\n", auths)
	if code != 0 || !strings.Contains(out, `"Secret":"tok"`) {
		t.Errorf("scheme-prefixed config host did not match: %d %q", code, out)
	}
}

func TestServeGetEmptyCredentialIsNotFound(t *testing.T) {
	t.Parallel()
	// A live config edit can add an entry with a blank token (bypassing startup
	// validation); the helper must not hand docker a blank credential.
	auths := map[string]config.RegistryAuth{"ghcr.io": {Username: "bot", Password: ""}}
	code, out, _ := serve(t, "get", "ghcr.io\n", auths)
	if code == 0 || strings.TrimSpace(out) != "credentials not found in native keychain" {
		t.Errorf("empty credential must be treated as not-found: %d %q", code, out)
	}
}

func TestServeGetLoadErrorFallsBackAnonymous(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	code := Serve([]string{"get"}, strings.NewReader("ghcr.io"), &out, &errOut,
		func() (map[string]config.RegistryAuth, error) { return nil, errBoom })
	// Docker must fall back to anonymous (sentinel), not abort the whole pull.
	if strings.TrimSpace(out.String()) != "credentials not found in native keychain" {
		t.Errorf("load error should emit the not-found sentinel, got %q", out.String())
	}
	if code == 0 {
		t.Error("load error should still exit non-zero")
	}
}

var errBoom = errBoomType("boom")

type errBoomType string

func (e errBoomType) Error() string { return string(e) }

func serve(t *testing.T, verb, stdin string, auths map[string]config.RegistryAuth) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Serve([]string{verb}, strings.NewReader(stdin), &out, &errOut,
		func() (map[string]config.RegistryAuth, error) { return auths, nil })
	return code, out.String(), errOut.String()
}

func TestServeGetFound(t *testing.T) {
	t.Parallel()
	auths := map[string]config.RegistryAuth{"ghcr.io": {Username: "bot", Password: "tok"}}
	code, out, _ := serve(t, "get", "ghcr.io\n", auths)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	var c struct{ ServerURL, Username, Secret string }
	if err := json.Unmarshal([]byte(out), &c); err != nil {
		t.Fatalf("output not JSON: %q (%v)", out, err)
	}
	if c.Username != "bot" || c.Secret != "tok" {
		t.Errorf("creds = %+v", c)
	}
}

func TestServeGetDockerHubNormalized(t *testing.T) {
	t.Parallel()
	// Config keyed by the friendly "docker.io"; docker asks with the canonical
	// index URL. They must match.
	auths := map[string]config.RegistryAuth{"docker.io": {Username: "u", Password: "p"}}
	code, out, _ := serve(t, "get", "https://index.docker.io/v1/\n", auths)
	if code != 0 {
		t.Fatalf("exit = %d, out=%q", code, out)
	}
	if !strings.Contains(out, `"Secret":"p"`) {
		t.Errorf("expected docker hub creds, got %q", out)
	}
}

func TestServeGetNotFound(t *testing.T) {
	t.Parallel()
	code, out, _ := serve(t, "get", "unknown.example.com\n", map[string]config.RegistryAuth{})
	if code == 0 {
		t.Error("not-found must exit non-zero")
	}
	// The docker-documented sentinel so docker proceeds anonymously rather than
	// hard-failing the pull.
	if strings.TrimSpace(out) != "credentials not found in native keychain" {
		t.Errorf("not-found sentinel = %q", out)
	}
}

func TestServeStoreEraseListAreHarmless(t *testing.T) {
	t.Parallel()
	auths := map[string]config.RegistryAuth{"ghcr.io": {Username: "bot", Password: "tok"}}
	for _, verb := range []string{"store", "erase"} {
		if code, _, _ := serve(t, verb, `{"ServerURL":"ghcr.io"}`, auths); code != 0 {
			t.Errorf("%s should exit 0", verb)
		}
	}
	code, out, _ := serve(t, "list", "", auths)
	if code != 0 || strings.TrimSpace(out) != "{}" {
		t.Errorf("list = %d %q", code, out)
	}
}

func TestServeUnknownVerb(t *testing.T) {
	t.Parallel()
	if code, _, _ := serve(t, "frobnicate", "", nil); code == 0 {
		t.Error("unknown verb must exit non-zero")
	}
}
