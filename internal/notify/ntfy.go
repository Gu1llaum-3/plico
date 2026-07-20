package notify

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Ntfy posts events to a ntfy topic URL (https://docs.ntfy.sh).
type Ntfy struct {
	URL    string // full topic URL
	Token  string // optional bearer token
	Client *http.Client
}

func NewNtfy(url, token string) *Ntfy {
	return &Ntfy{URL: url, Token: token, Client: &http.Client{Timeout: 10 * time.Second}}
}

func (n *Ntfy) Notify(ctx context.Context, ev Event) error {
	body := formatBody(ev)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.URL, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Title", fmt.Sprintf("[plico] %s: %s", ev.Stack, ev.Type))
	req.Header.Set("Priority", priorityFor(ev.Type))
	req.Header.Set("Tags", tagsFor(ev.Type))
	if n.Token != "" {
		req.Header.Set("Authorization", "Bearer "+n.Token)
	}
	resp, err := n.Client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy: unexpected status %s", resp.Status)
	}
	return nil
}

func formatBody(ev Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, "stack: %s\nrun: %s\nref: %s\n", ev.Stack, ev.RunID, ev.Ref)
	if ev.OldSHA != "" || ev.NewSHA != "" {
		fmt.Fprintf(&b, "%.12s → %.12s\n", ev.OldSHA, ev.NewSHA)
	}
	if ev.Stage != "" {
		fmt.Fprintf(&b, "stage: %s\n", ev.Stage)
	}
	if ev.Detail != "" {
		fmt.Fprintf(&b, "\n%s\n", ev.Detail)
	}
	return b.String()
}

func priorityFor(t EventType) string {
	switch t {
	case PreHookFailed, DeployFailed, GitSyncFailed, DriftDetected:
		return "high"
	case PreHookSkipped, WindowMissed:
		return "default"
	case DeploySuccess, DriftResolved:
		return "default"
	default: // deploy_queued, deploy_start
		return "low"
	}
}

func tagsFor(t EventType) string {
	switch t {
	case PreHookFailed, DeployFailed, GitSyncFailed, DriftDetected:
		return "rotating_light"
	case PreHookSkipped, WindowMissed:
		return "warning"
	case DeploySuccess, DriftResolved:
		return "white_check_mark"
	default:
		return "package"
	}
}
