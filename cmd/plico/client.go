package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"

	"github.com/spf13/cobra"

	"github.com/Gu1llaum-3/plico/internal/config"
)

// clientConn resolves the daemon's unix socket (from --socket, or the config
// file) and returns an HTTP client bound to it (F24).
type clientConn struct {
	configPath string
	socket     string
}

// registerClientFlags adds the connection flags shared by every client
// command.
func (c *clientConn) registerClientFlags(cmd *cobra.Command) {
	cmd.Flags().StringVarP(&c.configPath, "config", "c", "/etc/plico/config.toml",
		"config file used to locate the daemon socket")
	cmd.Flags().StringVar(&c.socket, "socket", "",
		"daemon unix socket (overrides the config-derived path)")
}

func (c *clientConn) client() (*http.Client, error) {
	socket := c.socket
	if socket == "" {
		cfg, err := config.Load(c.configPath)
		if err != nil {
			return nil, fmt.Errorf("cannot locate the daemon socket from %s (%w); use --socket", c.configPath, err)
		}
		socket = cfg.Api.Socket
	}
	return &http.Client{
		// No timeout: deploy-now legitimately takes minutes; the daemon
		// bounds every run with run_timeout.
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		},
	}, nil
}

// call POSTs (or GETs when body is nil) to the daemon API and decodes the
// JSON response into out. Non-2xx responses surface the server's error.
func (c *clientConn) call(method, path string, body, out any) error {
	client, err := c.client()
	if err != nil {
		return err
	}
	var reqBody bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&reqBody).Encode(body); err != nil {
			return err
		}
	}
	// The host is ignored for unix sockets but required by net/http.
	req, err := http.NewRequest(method, "http://plico"+path, &reqBody)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach the plico daemon (is it running?): %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.NewDecoder(resp.Body).Decode(&e) == nil && e.Error != "" {
			return fmt.Errorf("%s", e.Error)
		}
		return fmt.Errorf("daemon returned %s", resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
