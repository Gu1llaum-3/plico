package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Deliberately stricter than os.Expand: only ${VAR} is interpolated (never
// bare $VAR), and $${VAR} escapes to a literal ${VAR}.
var interpRe = regexp.MustCompile(`\$?\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Interpolate replaces ${ENV_VAR} references in raw config text. A reference
// to an unset variable is a fatal error: an infra daemon must not start with
// an empty token. (F20)
func Interpolate(text string) (string, error) {
	var missing []string
	out := interpRe.ReplaceAllStringFunc(text, func(m string) string {
		if strings.HasPrefix(m, "$$") {
			return m[1:] // $${VAR} -> literal ${VAR}
		}
		name := m[2 : len(m)-1]
		val, ok := os.LookupEnv(name)
		if !ok {
			missing = append(missing, name)
			return m
		}
		return val
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("undefined environment variable(s): %s", strings.Join(missing, ", "))
	}
	return out, nil
}
