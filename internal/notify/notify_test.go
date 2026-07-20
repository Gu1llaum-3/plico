package notify

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/smtp"
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

func TestParseEvents(t *testing.T) {
	t.Parallel()
	if evs, err := ParseEvents(nil); err != nil || len(evs) != len(DefaultEvents) {
		t.Errorf("empty list must yield the failure-oriented defaults, got %v (%v)", evs, err)
	}
	if evs, err := ParseEvents([]string{"all"}); err != nil || len(evs) != len(AllEvents) {
		t.Errorf("'all' must yield every event, got %v (%v)", evs, err)
	}
	if evs, err := ParseEvents([]string{"deploy_success", "deploy_queued"}); err != nil || len(evs) != 2 {
		t.Errorf("explicit opt-in list rejected: %v (%v)", evs, err)
	}
	if _, err := ParseEvents([]string{"deploy_sucess"}); err == nil {
		t.Error("typoed event name must be rejected")
	}
	// "all" anywhere selects everything...
	if evs, err := ParseEvents([]string{"deploy_success", "all"}); err != nil || len(evs) != len(AllEvents) {
		t.Errorf("'all' anywhere in the list must select everything, got %v (%v)", evs, err)
	}
	// ...but a typo alongside "all" must still be rejected: the operator may
	// later drop "all" and silently lose the mistyped event.
	if _, err := ParseEvents([]string{"all", "deploy_sucess"}); err == nil {
		t.Error("a typo alongside 'all' must be rejected, not short-circuited away")
	}
}

func TestCRLFNormalization(t *testing.T) {
	t.Parallel()
	// Mixed endings from hook output (curl/docker progress) must yield
	// strict CRLF: bare CR bytes get failure mails rejected by strict MTAs.
	got := crlf("a\r\nb\rc\nd")
	want := "a\r\nb\r\nc\r\nd"
	if got != want {
		t.Errorf("crlf() = %q, want %q", got, want)
	}
	if strings.Contains(strings.ReplaceAll(got, "\r\n", ""), "\r") {
		t.Error("bare CR survived normalization")
	}
}

func TestMultiIsConcurrent(t *testing.T) {
	t.Parallel()
	// A hung channel must not starve a healthy one out of the shared
	// per-event deadline.
	blocker := notifierFunc(func(ctx context.Context, _ Event) error {
		<-ctx.Done()
		return ctx.Err()
	})
	rec := &recorder{}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_ = Multi(blocker, rec).Notify(ctx, Event{Type: DeployFailed})
	if time.Since(start) > 2*time.Second {
		t.Fatal("Multi did not return promptly after ctx expiry")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.events) != 1 {
		t.Errorf("healthy channel starved by the hung one: %d events", len(rec.events))
	}
}

type notifierFunc func(context.Context, Event) error

func (f notifierFunc) Notify(ctx context.Context, ev Event) error { return f(ctx, ev) }

func TestDefaultEventsAreFailureOriented(t *testing.T) {
	t.Parallel()
	for _, e := range DefaultEvents {
		if e == DeploySuccess || e == DeployQueued || e == DeployStart {
			t.Errorf("%s must be opt-in, not part of the defaults", e)
		}
	}
	for _, want := range []EventType{PreHookFailed, DeployFailed, WindowMissed, GitSyncFailed} {
		found := false
		for _, e := range DefaultEvents {
			if e == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s missing from the failure-oriented defaults", want)
		}
	}
}

func TestFilterEvents(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	n := FilterEvents(rec, []EventType{DeployFailed, DeploySuccess})
	for _, tpe := range []EventType{DeployQueued, DeployFailed, DeployStart, DeploySuccess} {
		if err := n.Notify(context.Background(), Event{Type: tpe}); err != nil {
			t.Fatal(err)
		}
	}
	if len(rec.events) != 2 || rec.events[0].Type != DeployFailed || rec.events[1].Type != DeploySuccess {
		t.Errorf("filter let through %v", rec.events)
	}
}

func TestWebhookPostsTextAndStructuredEvent(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var gotBody, gotAuth, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
	}))
	defer srv.Close()

	n := NewWebhook(srv.URL, "hook-tok")
	err := n.Notify(context.Background(), Event{
		Type: DeployFailed, Stack: "webapp", RunID: "r1", Stage: "pull",
		Detail: "manifest unknown", Time: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotAuth != "Bearer hook-tok" || gotCT != "application/json" {
		t.Errorf("headers: auth=%q ct=%q", gotAuth, gotCT)
	}
	// "text" for chat apps + structured event for anything richer.
	for _, want := range []string{`"text"`, "deploy_failed", `"stage":"pull"`, "manifest unknown", `"stack":"webapp"`} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("body missing %s:\n%s", want, gotBody)
		}
	}
}

func TestSMTPMessage(t *testing.T) {
	t.Parallel()
	var gotAddr, gotFrom string
	var gotTo []string
	var gotMsg []byte
	s := NewSMTP("mail.example.com", 587, "plico@example.com",
		[]string{"ops@example.com"}, "user", "pass")
	s.sendMail = func(addr string, _ smtp.Auth, from string, to []string, msg []byte) error {
		gotAddr, gotFrom, gotTo, gotMsg = addr, from, to, msg
		return nil
	}
	err := s.Notify(context.Background(), Event{
		Type: WindowMissed, Stack: "webapp",
		Detail: "window of firing 2026-07-19T04:00:00 elapsed without any run",
		Time:   time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotAddr != "mail.example.com:587" || gotFrom != "plico@example.com" || len(gotTo) != 1 {
		t.Errorf("addr=%q from=%q to=%v", gotAddr, gotFrom, gotTo)
	}
	msg := string(gotMsg)
	for _, want := range []string{
		"Subject: [plico] webapp: window_missed",
		"To: ops@example.com",
		"elapsed without any run",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
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
