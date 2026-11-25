package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestConfirmStackRequiresExactName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		ok    bool
	}{
		{"exact name", "web\n", true},
		{"exact name without newline", "web", true},
		{"surrounding spaces", "  web \n", true},
		{"typo", "wbe\n", false},
		{"prefix only", "we\n", false},
		{"empty line", "\n", false},
		{"no input at all", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			err := confirmStack(strings.NewReader(tt.input), &out, "web")
			if (err == nil) != tt.ok {
				t.Errorf("confirmStack(%q) err = %v, want ok=%v", tt.input, err, tt.ok)
			}
			if !strings.Contains(out.String(), `"web"`) {
				t.Errorf("prompt must name the stack, got %q", out.String())
			}
		})
	}
}
