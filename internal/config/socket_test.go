package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveSocket(t *testing.T) {
	t.Setenv("PLICO_SOCKET_TEST", "/run/custom/plico.sock")
	tests := []struct {
		name    string
		config  string
		want    string
		wantErr string
	}{
		{
			name: "legacy base dir",
			config: `base_dir = "/opt/docker"
[[stack]]
name = "ignored"
repo = "https://example.com/repo.git"`,
			want: "/opt/docker/plico.sock",
		},
		{
			name: "explicit socket ignores unrelated secrets and invalid daemon fields",
			config: `base_dir = "${UNSET_BASE_DIR}"
timezone = "Not/AZone"
[api]
socket = "/run/plico/plico.sock"
[git.auths."example.com"]
password = "${UNSET_GIT_TOKEN}"`,
			want: "/run/plico/plico.sock",
		},
		{
			name: "selected socket is interpolated",
			config: `[api]
socket = "${PLICO_SOCKET_TEST}"`,
			want: "/run/custom/plico.sock",
		},
		{
			name: "hash in decoded path is not a comment",
			config: `[api]
socket = "/run/plico/with#hash.sock"`,
			want: "/run/plico/with#hash.sock",
		},
		{
			name: "missing selected variable",
			config: `[api]
socket = "${UNSET_SELECTED_SOCKET}"`,
			wantErr: "UNSET_SELECTED_SOCKET",
		},
		{
			name:    "missing locator",
			config:  `timezone = "Europe/Paris"`,
			wantErr: "set [api].socket or base_dir",
		},
		{
			name: "relative socket",
			config: `[api]
socket = "plico.sock"`,
			wantErr: "absolute path",
		},
		{
			name:    "malformed TOML",
			config:  `[api`,
			wantErr: "expected '.' or ']'",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tt.config), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := ResolveSocket(path)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ResolveSocket() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("ResolveSocket() = %q, want %q", got, tt.want)
			}
		})
	}
}
