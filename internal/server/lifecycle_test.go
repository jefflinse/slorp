package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/claude"
	"github.com/jefflinse/slorp/internal/hub"
	"github.com/jefflinse/slorp/internal/integrations"
	"github.com/jefflinse/slorp/internal/testdb"
)

type managedLauncher struct {
	launcher
	h          *hub.Hub
	afterStart func()
}

func (l *managedLauncher) LaunchOptions(context.Context, string) (hub.LaunchOptions, error) {
	return hub.LaunchOptions{Agents: []hub.LaunchAgent{{Name: "build"}}, Models: []hub.LaunchModel{{ID: "provider/org/model", Variants: []string{"high"}}}, Defaults: hub.LaunchDefaults{AgentName: "build", Model: "provider/org/model"}}, l.err
}

func (l *managedLauncher) Start(ctx context.Context, s hub.Launch) (string, error) {
	id, err := l.launcher.Start(ctx, s)
	if err != nil {
		return id, err
	}
	err = l.h.OpenManaged("opencode", id, "owned-job", "private-run", func(session *hub.Session) { session.Cwd = s.Dir })
	if err == nil {
		hub.ReportLaunchJob(ctx, id, "owned-job")
	}
	if l.afterStart != nil {
		l.afterStart()
	}
	return id, err
}

func (l *managedLauncher) Stop(ctx context.Context, job string) error {
	err := l.launcher.Stop(ctx, job)
	if err == nil {
		l.h.CloseManaged("private-run")
	}
	return err
}

func (l *managedLauncher) ResumeCommand(id, dir string) string {
	return "cd -- " + hub.ShellQuote(dir) + " && 'custom opencode' --session " + hub.ShellQuote(id)
}

func TestLifecycleHTTPContracts(t *testing.T) {
	cache := testdb.Open(t)
	k := testdb.KB(t, cache)
	h := hub.New(k, cache)
	l := &managedLauncher{launcher: launcher{id: "ses_native"}, h: h}
	m := integrations.NewManager(func() map[string]bool { return nil })
	handler := New(h, k, m, &asked{}, map[string]History{"opencode": func(context.Context, string, int) ([]hub.Exchange, error) { return nil, nil }}, map[string]Launcher{"opencode": l, "claude": &claude.Adapter{Bin: "/custom/claude"}}, nil, nil, nil, Page(nil, ""))
	do := func(method, path, body, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if token != "" {
			r.Header.Set("X-Slorp-Run", token)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	dir := t.TempDir()
	if w := do("GET", "/api/agents/opencode/launch-options?dir=relative", "", ""); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if w := do("GET", "/api/agents/opencode/launch-options?dir="+url.QueryEscape(dir), "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"agentName":"build"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, extra := range []string{`,"effortLevel":"high"`, `,"local":{}`} {
		if w := do("POST", "/api/sessions", `{"agent":"opencode","dir":`+quotedJSON(dir)+extra+`}`, ""); w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	w := do("POST", "/api/sessions", `{"agent":"opencode","dir":`+quotedJSON(dir)+`,"agentName":"review","model":"provider/org/model","variant":"high"}`, "")
	if w.Code != 202 || !strings.Contains(w.Body.String(), `"sessionId":"ses_native"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if len(l.started) != 1 || l.started[0].AgentName != "review" || l.started[0].Variant != "high" || l.started[0].Model != "provider/org/model" {
		t.Fatal(l.started)
	}
	w = do("GET", "/api/sessions", "", "")
	var state struct {
		hub.State
		Capabilities map[string]agentCapabilities `json:"capabilities"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	c := state.Capabilities["opencode"]
	if !c.Start || !c.Resume || !c.LaunchOptions || !c.ResumeCommand || !c.History || !state.Sessions[0].CanStop {
		t.Fatalf("state: %+v", state)
	}
	if strings.Contains(w.Body.String(), "private-run") {
		t.Fatal("token published")
	}
	for _, token := range []string{"", "unknown"} {
		if w := do("POST", "/api/bridge/poll", `{"agent":"opencode","sessionId":"ses_native"}`, token); w.Code != 409 {
			t.Fatal("unfenced poll", w.Code)
		}
	}
	for _, path := range []string{"poll", "event", "permission", "subagent"} {
		if w := do("POST", "/api/bridge/"+path, `{"agent":"opencode","sessionId":"other"}`, "private-run"); w.Code != 409 {
			t.Fatal("foreign root", path, w.Code)
		}
	}
	if w := do("POST", "/api/bridge/poll", `{"agent":"opencode","sessionId":"ses_native"}`, "private-run"); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	commandBefore := do("GET", "/api/sessions/ses_native/resume-command", "", "").Body.String()
	if !strings.Contains(commandBefore, "custom opencode") || strings.Contains(commandBefore, "private-run") {
		t.Fatal(commandBefore)
	}
	if w := do("POST", "/api/sessions/ses_native/stop", `{}`, ""); w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	if after := do("GET", "/api/sessions/ses_native/resume-command", "", "").Body.String(); after != commandBefore {
		t.Fatal(after, commandBefore)
	}
	for _, path := range []string{"poll", "event", "permission", "permission/wait", "permission/done", "subagent", "ack"} {
		if w := do("POST", "/api/bridge/"+path, `{"agent":"opencode","sessionId":"ses_native","id":"old"}`, "private-run"); w.Code != 409 {
			t.Fatal("stopped token", path, w.Code)
		}
	}
	if len(h.Sessions()) != 0 {
		t.Fatal("stale bridge resurrected archive")
	}
	if w := do("POST", "/api/archive/ses_native/resume", `{}`, ""); w.Code != 202 || !strings.Contains(w.Body.String(), `"sessionId":"ses_native"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	l.err = hub.ErrLifecycleConflict
	if w := do("POST", "/api/archive/ses_native/resume", `{}`, ""); w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	l.err = hub.ErrLaunchInvalid
	if w := do("POST", "/api/sessions", `{"agent":"opencode","dir":`+quotedJSON(dir)+`}`, ""); w.Code != 400 || !strings.Contains(w.Body.String(), `"sessionId":"ses_native"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	h.Apply("claude", "claude-id", func(s *hub.Session) { s.Cwd = dir; s.Job = "claude-job" })
	if w := do("GET", "/api/sessions/claude-id/resume-command", "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "'/custom/claude' --resume 'claude-id'") {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestBindingFailureReturnsKnownIDAndStopsOwnedRuntime(t *testing.T) {
	cache := testdb.Open(t)
	k := testdb.KB(t, cache)
	h := hub.New(k, cache)
	e, err := k.Create("Assignment", "")
	if err != nil {
		t.Fatal(err)
	}
	l := &managedLauncher{launcher: launcher{id: "ses_created"}, h: h, afterStart: func() {
		if err := k.Delete(e.ID); err != nil {
			t.Error(err)
		}
	}}
	handler := New(h, k, integrations.NewManager(func() map[string]bool { return nil }), &asked{}, nil, map[string]Launcher{"opencode": l}, nil, nil, nil, Page(nil, ""))
	body := `{"agent":"opencode","dir":` + quotedJSON(t.TempDir()) + `,"effort":` + quotedJSON(e.ID) + `}`
	r := httptest.NewRequest("POST", "http://127.0.0.1/api/sessions", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 500 || !strings.Contains(w.Body.String(), `"sessionId":"ses_created"`) || !strings.Contains(w.Body.String(), "conversation retained") {
		t.Fatal(w.Code, w.Body.String())
	}
	if len(h.Sessions()) != 0 {
		t.Fatal("binding failure left owned runtime active")
	}
	if _, ok := h.Archive("ses_created"); !ok {
		t.Fatal("binding failure lost native identity")
	}
	if len(l.calls) != 2 || l.calls[1] != "stop owned-job" {
		t.Fatal(l.calls)
	}
}

func TestLifecycleErrorStatus(t *testing.T) {
	for err, code := range map[error]int{hub.ErrLaunchInvalid: 400, hub.ErrLifecycleConflict: 409, context.Canceled: 504, context.DeadlineExceeded: 504, errors.New("native unavailable"): 502} {
		w := httptest.NewRecorder()
		lifecycleError(w, err, "")
		if w.Code != code {
			t.Fatal(err, w.Code)
		}
	}
}

func quotedJSON(s string) string { b, _ := json.Marshal(s); return string(b) }

type blockedBridgeWriter struct {
	header  http.Header
	entered chan struct{}
	unblock chan struct{}
}

func (w *blockedBridgeWriter) Header() http.Header         { return w.header }
func (w *blockedBridgeWriter) WriteHeader(int)             { close(w.entered); <-w.unblock }
func (w *blockedBridgeWriter) Write(b []byte) (int, error) { return len(b), nil }

func TestBridgeDrainsBeforeNetworkWrite(t *testing.T) {
	cache := testdb.Open(t)
	h := hub.New(testdb.KB(t, cache), cache)
	if err := h.OpenManaged("opencode", "root", "job", "token", nil); err != nil {
		t.Fatal(err)
	}
	handler := bridgeFenced(h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.Apply("opencode", "root", func(s *hub.Session) { s.LastReply = "accepted" })
		writeJSON(w, 200, map[string]bool{"ok": true})
	}))
	w := &blockedBridgeWriter{header: make(http.Header), entered: make(chan struct{}), unblock: make(chan struct{})}
	r := httptest.NewRequest("POST", "http://127.0.0.1/api/bridge/event", strings.NewReader(`{"agent":"opencode","sessionId":"root"}`))
	r.Header.Set("X-Slorp-Run", "token")
	served := make(chan struct{})
	go func() { handler.ServeHTTP(w, r); close(served) }()
	defer func() { close(w.unblock); <-served }()
	select {
	case <-w.entered:
	case <-time.After(time.Second):
		t.Fatal("handler never wrote")
	}
	closed := make(chan struct{})
	go func() { h.CloseManaged("token"); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("network backpressure held a bridge lease")
	}
	if _, ok := h.Archive("root"); !ok {
		t.Fatal("session not archived")
	}
}
