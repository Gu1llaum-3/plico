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
//
// TOML comments are left untouched, so commenting out an optional setting
// (`# token = "${X}"`) never blocks startup because of an unset variable.
func Interpolate(text string) (string, error) {
	var missing []string
	var out strings.Builder
	for i, line := range strings.Split(text, "\n") {
		if i > 0 {
			out.WriteByte('\n')
		}
		code, comment := splitComment(line)
		out.WriteString(interpRe.ReplaceAllStringFunc(code, func(m string) string {
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
		}))
		out.WriteString(comment)
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("undefined environment variable(s): %s", strings.Join(missing, ", "))
	}
	return out.String(), nil
}

// splitComment splits a line at the first # that is outside a TOML string.
// Good enough for config files: multi-line strings are not supported by this
// scanner (single-line basic "…" and literal '…' strings are).
func splitComment(line string) (code, comment string) {
	inBasic, inLiteral := false, false
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case inBasic:
			switch c {
			case '\\':
				i++ // skip escaped char
			case '"':
				inBasic = false
			}
		case inLiteral:
			if c == '\'' {
				inLiteral = false
			}
		case c == '"':
			inBasic = true
		case c == '\'':
			inLiteral = true
		case c == '#':
			return line[:i], line[i:]
		}
	}
	return line, ""
}
