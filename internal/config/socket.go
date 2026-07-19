package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// ResolveSocket reads only the fields needed by client commands to locate
// the daemon. Daemon-only settings and secrets are deliberately not expanded
// or validated: access to the Unix socket is the client's authorization.
func ResolveSocket(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var locator struct {
		BaseDir string `toml:"base_dir"`
		Api     struct {
			Socket string `toml:"socket"`
		} `toml:"api"`
	}
	if _, err := toml.Decode(string(raw), &locator); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}

	field, value := "api.socket", locator.Api.Socket
	if value == "" {
		field, value = "base_dir", locator.BaseDir
	}
	if value == "" {
		return "", fmt.Errorf("%s: cannot locate daemon socket: set [api].socket or base_dir", path)
	}
	value, err = interpolateValue(value)
	if err != nil {
		return "", fmt.Errorf("%s: %s: %w", path, field, err)
	}
	if field == "base_dir" {
		value = filepath.Join(value, "plico.sock")
	}
	if !filepath.IsAbs(value) {
		return "", fmt.Errorf("%s: %s must resolve to an absolute path, got %q", path, field, value)
	}
	return value, nil
}

// interpolateValue expands a decoded TOML value. Unlike Interpolate it must
// not treat '#' as a comment marker: at this point it is ordinary path data.
func interpolateValue(value string) (string, error) {
	var missing []string
	value = interpRe.ReplaceAllStringFunc(value, func(match string) string {
		if strings.HasPrefix(match, "$$") {
			return match[1:]
		}
		name := match[2 : len(match)-1]
		expanded, ok := os.LookupEnv(name)
		if !ok {
			missing = append(missing, name)
			return match
		}
		return expanded
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("undefined environment variable(s): %s", strings.Join(missing, ", "))
	}
	return value, nil
}
