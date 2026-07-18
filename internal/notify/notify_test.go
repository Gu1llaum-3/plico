package notify

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNtfySendsExpectedRequest(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var gotReq *http.Request
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotReq = r.Clone(context.Background())
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
	}))
	defer srv.Close()

	n := NewNtfy(srv.URL, "tok123")
	err := n.Notify(context.Background(), Event{
		Type: PreHookFailed, Stack: "webapp", RunID: "r1", Ref: "main",
		OldSHA: "1111111111111111", NewSHA: "2222222222222222",
		Stage: "pre_hook", Detail: "pg_dump: connection refused",
		Time: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if got := gotReq.Header.Get("Title"); !strings.Contains(got, "webapp") || !strings.Contains(got, "pre_hook_failed") {
		t.Errorf("Title = %q", got)
	}
	if got := gotReq.Header.Get("Priority"); got != "high" {
		t.Errorf("Priority = %q, want high", got)
	}
	if got := gotReq.Header.Get("Authorization"); got != "Bearer tok123" {
		t.Errorf("Authorization = %q", got)
	}
	for _, want := range []string{"stack: webapp", "111111111111 → 222222222222", "stage: pre_hook", "pg_dump"} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("body missing %q:\n%s", want, gotBody)
		}
	}
}

func TestNtfyErrorOnBadStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	if err := NewNtfy(srv.URL, "").Notify(context.Background(), Event{Type: DeploySuccess}); err == nil {
		t.Fatal("want error on 403")
	}
}

type failing struct{}

func (failing) Notify(context.Context, Event) error { return errors.New("boom") }

func TestWithLogFallbackSwallowsError(t *testing.T) {
	t.Parallel()
	var buf strings.Builder
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	n := WithLogFallback(failing{}, log)
	if err := n.Notify(context.Background(), Event{Type: DeployFailed, Stack: "s"}); err != nil {
		t.Fatalf("fallback must swallow errors, got %v", err)
	}
	if !strings.Contains(buf.String(), "notification failed") {
		t.Error("send failure was not logged")
	}
}

type recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *recorder) Notify(_ context.Context, ev Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return nil
}

func TestMultiFansOut(t *testing.T) {
	t.Parallel()
	a, b := &recorder{}, &recorder{}
	if err := Multi(a, b).Notify(context.Background(), Event{Type: DeployStart}); err != nil {
		t.Fatal(err)
	}
	if len(a.events) != 1 || len(b.events) != 1 {
		t.Errorf("fan-out failed: a=%d b=%d", len(a.events), len(b.events))
	}
}
