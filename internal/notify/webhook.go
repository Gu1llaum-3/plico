package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Webhook posts events as JSON to a generic endpoint. The body carries a
// human "text" field — accepted as-is by Google Chat and Teams incoming
// webhooks — plus the structured event for anything richer.
type Webhook struct {
	URL    string
	Token  string // optional bearer token
	Client *http.Client
}

func NewWebhook(url, token string) *Webhook {
	return &Webhook{URL: url, Token: token, Client: &http.Client{Timeout: 10 * time.Second}}
}

type webhookBody struct {
	Text  string `json:"text"`
	Event struct {
		Type   string    `json:"type"`
		Stack  string    `json:"stack"`
		RunID  string    `json:"run_id,omitempty"`
		Ref    string    `json:"ref,omitempty"`
		OldSHA string    `json:"old_sha,omitempty"`
		NewSHA string    `json:"new_sha,omitempty"`
		Stage  string    `json:"stage,omitempty"`
		Detail string    `json:"detail,omitempty"`
		Time   time.Time `json:"time"`
	} `json:"event"`
}

func (w *Webhook) Notify(ctx context.Context, ev Event) error {
	var body webhookBody
	body.Text = fmt.Sprintf("[plico] %s: %s\n%s", ev.Stack, ev.Type, formatBody(ev))
	body.Event.Type = string(ev.Type)
	body.Event.Stack = ev.Stack
	body.Event.RunID = ev.RunID
	body.Event.Ref = ev.Ref
	body.Event.OldSHA = ev.OldSHA
	body.Event.NewSHA = ev.NewSHA
	body.Event.Stage = ev.Stage
	body.Event.Detail = ev.Detail
	body.Event.Time = ev.Time

	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if w.Token != "" {
		req.Header.Set("Authorization", "Bearer "+w.Token)
	}
	resp, err := w.Client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook: unexpected status %s", resp.Status)
	}
	return nil
}
