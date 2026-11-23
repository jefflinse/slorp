package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/hub"
	"github.com/jefflinse/slorp/internal/integrations"
	"github.com/jefflinse/slorp/internal/kb"
	"github.com/jefflinse/slorp/internal/testdb"
)

type journalLauncher struct {
	start  func(context.Context, hub.Launch) (string, error)
	resume func(context.Context, string, string, string) error
}

func (l *journalLauncher) Start(ctx context.Context, s hub.Launch) (string, error) {
	return l.start(ctx, s)
}
func (l *journalLauncher) Resume(ctx context.Context, id, dir, prompt string) error {
	return l.resume(ctx, id, dir, prompt)
}
func (l *journalLauncher) Stop(context.Context, string) error { return nil }

func journalHandler(h *hub.Hub, k *kb.Store, l Launcher) http.Handler {
	return New(h, k, integrations.NewManager(func() map[string]bool { return nil }), &asked{}, nil, map[string]Launcher{"opencode": l}, nil, nil, nil, nil, nil, Page(nil, ""))
}

func journalRequest(ctx context.Context, path, body string) *http.Request {
	r := httptest.NewRequest("POST", "http://127.0.0.1"+path, strings.NewReader(body)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	return r
}

type lostResponse struct{ header http.Header }

func (w *lostResponse) Header() http.Header       { return w.header }
func (w *lostResponse) WriteHeader(int)           {}
func (w *lostResponse) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestLifecycleAcceptedResponseLostReplaysAfterRestart(t *testing.T) {
	cache := testdb.Open(t)
	k := testdb.KB(t, cache)
	h := hub.New(k, cache)
	var calls atomic.Int32
	l := &journalLauncher{start: func(ctx context.Context, _ hub.Launch) (string, error) {
		calls.Add(1)
		return "ses_created", hub.CheckpointLaunch(ctx, "ses_created")
	}}
	body := `{"agent":"opencode","dir":` + quotedJSON(t.TempDir()) + `,"prompt":"one turn","requestId":"attempt-1"}`
	handler := journalHandler(h, k, l)
	handler.ServeHTTP(&lostResponse{header: make(http.Header)}, journalRequest(context.Background(), "/api/sessions", body))
	handler = journalHandler(hub.New(k, cache), k, l)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, journalRequest(context.Background(), "/api/sessions", body))
	if w.Code != 202 || !strings.Contains(w.Body.String(), `"sessionId":"ses_created"`) || !strings.Contains(w.Body.String(), `"outcome":"accepted"`) || calls.Load() != 1 {
		t.Fatal(w.Code, w.Body.String(), calls.Load())
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, journalRequest(context.Background(), "/api/sessions", strings.Replace(body, "one turn", "different turn", 1)))
	if w.Code != 409 || calls.Load() != 1 {
		t.Fatal("key reuse was dispatched", w.Code, calls.Load())
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, journalRequest(context.Background(), "/api/sessions", strings.Replace(body, "attempt-1", "attempt-2", 1)))
	if w.Code != 202 || calls.Load() != 2 {
		t.Fatal("deduplicated by prompt text", w.Code, calls.Load())
	}
}

func TestLifecycleRecordedNativeEffectWithheldResponse(t *testing.T) {
	cache := testdb.Open(t)
	k := testdb.KB(t, cache)
	h := hub.New(k, cache)
	var calls atomic.Int32
	created := make(chan struct{})
	l := &journalLauncher{start: func(ctx context.Context, _ hub.Launch) (string, error) {
		calls.Add(1)
		if err := hub.CheckpointLaunch(ctx, "ses_created"); err != nil {
			return "ses_created", err
		}
		close(created)
		<-ctx.Done()
		return "ses_created", fmt.Errorf("%w: %w", hub.ErrLifecycleUnknown, ctx.Err())
	}}
	body := `{"agent":"opencode","dir":` + quotedJSON(t.TempDir()) + `,"prompt":"recorded once","requestId":"attempt"}`
	handler := journalHandler(h, k, l)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { handler.ServeHTTP(first, journalRequest(ctx, "/api/sessions", body)); close(done) }()
	select {
	case <-created:
	case <-time.After(time.Second):
		t.Fatal("creation did not checkpoint")
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, journalRequest(context.Background(), "/api/sessions", body))
	if w.Code != 409 || !strings.Contains(w.Body.String(), `"outcome":"pending"`) || !strings.Contains(w.Body.String(), "ses_created") || calls.Load() != 1 {
		t.Fatal(w.Code, w.Body.String(), calls.Load())
	}
	cancel()
	<-done
	if first.Code != 504 || !strings.Contains(first.Body.String(), `"outcome":"unknown"`) {
		t.Fatal(first.Code, first.Body.String())
	}
	for _, current := range []http.Handler{handler, journalHandler(hub.New(k, cache), k, l)} {
		w = httptest.NewRecorder()
		current.ServeHTTP(w, journalRequest(context.Background(), "/api/sessions", body))
		if w.Code != 409 || !strings.Contains(w.Body.String(), `"outcome":"unknown"`) || !strings.Contains(w.Body.String(), "ses_created") || calls.Load() != 1 {
			t.Fatal(w.Code, w.Body.String(), calls.Load())
		}
	}
}

func TestResumeReplayPrecedesOpenSessionConflict(t *testing.T) {
	cache := testdb.Open(t)
	k := testdb.KB(t, cache)
	h := hub.New(k, cache)
	dir := t.TempDir()
	h.Apply("opencode", "ses_native", func(s *hub.Session) { s.Cwd = dir; s.LastPrompt = "earlier" })
	h.Retire("opencode", -time.Second)
	var calls atomic.Int32
	l := &journalLauncher{resume: func(context.Context, string, string, string) error {
		calls.Add(1)
		return h.OpenManaged("opencode", "ses_native", "job", "token", nil)
	}}
	handler := journalHandler(h, k, l)
	for range 2 {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, journalRequest(context.Background(), "/api/archive/ses_native/resume", `{"prompt":"continue","requestId":"resume-attempt"}`))
		if w.Code != 202 || !strings.Contains(w.Body.String(), `"outcome":"accepted"`) {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if calls.Load() != 1 {
		t.Fatal("resume dispatched twice")
	}
	h.CloseManaged("token")
}

func TestResumeUnknownPromptOutcomeNeverRedispatches(t *testing.T) {
	cache := testdb.Open(t)
	k := testdb.KB(t, cache)
	h := hub.New(k, cache)
	dir := t.TempDir()
	h.Apply("opencode", "ses_native", func(s *hub.Session) { s.Cwd = dir; s.LastPrompt = "earlier" })
	h.Retire("opencode", -time.Second)
	var calls atomic.Int32
	recorded := make(chan struct{})
	l := &journalLauncher{resume: func(ctx context.Context, _, _, _ string) error {
		calls.Add(1)
		if err := h.OpenManaged("opencode", "ses_native", "job", "token", nil); err != nil {
			return err
		}
		close(recorded)
		<-ctx.Done()
		h.CloseManaged("token")
		return fmt.Errorf("%w: %w", hub.ErrLifecycleUnknown, ctx.Err())
	}}
	handler := journalHandler(h, k, l)
	body := `{"prompt":"continue once","requestId":"resume-once"}`
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { handler.ServeHTTP(w, journalRequest(ctx, "/api/archive/ses_native/resume", body)); close(done) }()
	select {
	case <-recorded:
	case <-time.After(time.Second):
		t.Fatal("prompt not recorded")
	}
	pending := httptest.NewRecorder()
	handler.ServeHTTP(pending, journalRequest(context.Background(), "/api/archive/ses_native/resume", body))
	if pending.Code != 409 || !strings.Contains(pending.Body.String(), `"outcome":"pending"`) {
		t.Fatal(pending.Code, pending.Body.String())
	}
	cancel()
	<-done
	if w.Code != 504 || !strings.Contains(w.Body.String(), `"outcome":"unknown"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, current := range []http.Handler{handler, journalHandler(hub.New(k, cache), k, l)} {
		retry := httptest.NewRecorder()
		current.ServeHTTP(retry, journalRequest(context.Background(), "/api/archive/ses_native/resume", body))
		if retry.Code != 409 || !strings.Contains(retry.Body.String(), `"sessionId":"ses_native"`) || !strings.Contains(retry.Body.String(), `"outcome":"unknown"`) || calls.Load() != 1 {
			t.Fatal(retry.Code, retry.Body.String(), calls.Load())
		}
	}
}

func TestUnknownCreationWithoutNativeIDIsStillReserved(t *testing.T) {
	cache := testdb.Open(t)
	k := testdb.KB(t, cache)
	h := hub.New(k, cache)
	var calls atomic.Int32
	l := &journalLauncher{start: func(context.Context, hub.Launch) (string, error) {
		var reservation map[string]any
		found, err := cache.Get("lifecycle", "lost-create", &reservation)
		if err != nil || !found || reservation["result"].(map[string]any)["outcome"] != "pending" {
			t.Fatal("no durable pending reservation before native work", reservation, err)
		}
		calls.Add(1)
		return "", hub.ErrLifecycleUnknown
	}}
	body := `{"agent":"opencode","dir":` + quotedJSON(t.TempDir()) + `,"prompt":"create once","requestId":"lost-create"}`
	w := httptest.NewRecorder()
	journalHandler(h, k, l).ServeHTTP(w, journalRequest(context.Background(), "/api/sessions", body))
	if w.Code != 502 || !strings.Contains(w.Body.String(), `"outcome":"unknown"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	journalHandler(hub.New(k, cache), k, l).ServeHTTP(w, journalRequest(context.Background(), "/api/sessions", body))
	if w.Code != 409 || !strings.Contains(w.Body.String(), `"sessionId":""`) || !strings.Contains(w.Body.String(), `"outcome":"unknown"`) || calls.Load() != 1 {
		t.Fatal(w.Code, w.Body.String(), calls.Load())
	}
}

func TestClaudeResumeDoesNotUseOpenCodeJournal(t *testing.T) {
	cache := testdb.Open(t)
	k := testdb.KB(t, cache)
	h := hub.New(k, cache)
	h.Apply("claude", "claude-id", func(s *hub.Session) { s.Cwd = t.TempDir(); s.LastPrompt = "earlier" })
	h.Retire("claude", -time.Second)
	if _, _, err := h.BeginLifecycle("shared-key", "opencode-operation", ""); err != nil {
		t.Fatal(err)
	}
	calls := 0
	l := &journalLauncher{resume: func(context.Context, string, string, string) error { calls++; return nil }}
	handler := New(h, k, integrations.NewManager(func() map[string]bool { return nil }), &asked{}, nil, map[string]Launcher{"claude": l}, nil, nil, nil, nil, nil, Page(nil, ""))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, journalRequest(context.Background(), "/api/archive/claude-id/resume", `{"requestId":"shared-key"}`))
	if w.Code != 202 || calls != 1 {
		t.Fatal(w.Code, w.Body.String(), calls)
	}
}
