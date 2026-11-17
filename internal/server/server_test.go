package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jefflinse/slorp/internal/hub"
	"github.com/jefflinse/slorp/internal/kb"
	"github.com/jefflinse/slorp/internal/testdb"
)

type fixture struct {
	t   *testing.T
	srv *httptest.Server
	hub *hub.Hub
	kb  *kb.Store
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st := testdb.Open(t)
	k := testdb.KB(t, st)
	h := hub.New(k, st)
	page := filepath.Join(t.TempDir(), "index.html")
	if err := os.WriteFile(page, []byte("<title>slorp</title>"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(h, k, page))
	t.Cleanup(srv.Close)
	return &fixture{t, srv, h, k}
}

// do sends a request the way the web UI or a bridge would. edit may change
// the request before it goes out.
func (f *fixture) do(method, path, body string, edit func(*http.Request)) (int, string) {
	f.t.Helper()
	req, err := http.NewRequest(method, f.srv.URL+path, strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	if edit != nil {
		edit(req)
	}
	res, err := f.srv.Client().Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func (f *fixture) post(path, body string) int {
	f.t.Helper()
	code, _ := f.do(http.MethodPost, path, body, nil)
	return code
}

func (f *fixture) state() hub.State {
	f.t.Helper()
	code, body := f.do(http.MethodGet, "/api/sessions", "", nil)
	if code != http.StatusOK {
		f.t.Fatalf("GET /api/sessions = %d", code)
	}
	var st hub.State
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		f.t.Fatal(err)
	}
	return st
}

// connect makes a session exist the way a bridge's first poll does.
func (f *fixture) connect(id string) {
	f.t.Helper()
	if code := f.post("/api/bridge/poll", `{"sessionId":"`+id+`","cwd":"/work/a"}`); code != http.StatusOK {
		f.t.Fatalf("bridge poll = %d", code)
	}
}

func TestRefusesRequestsAWebPageElsewhereCouldCause(t *testing.T) {
	f := newFixture(t)
	f.connect("s1")
	reply := "/api/sessions/s1/reply"
	own := f.srv.URL

	tests := []struct {
		name   string
		method string
		path   string
		edit   func(*http.Request)
		want   int
	}{
		{"the page itself", "GET", "/", nil, 200},
		{"same-origin post", "POST", reply, func(r *http.Request) { r.Header.Set("Origin", own) }, 202},
		{"post with no origin, as a bridge sends", "POST", reply, nil, 202},
		{"post from another site", "POST", reply, func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }, 403},
		{"read from another site", "GET", "/api/sessions", func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }, 403},
		{"another site's name pointed at this address", "GET", "/api/sessions", func(r *http.Request) { r.Host = "evil.example" }, 403},
		{"form post, which needs no preflight", "POST", reply, func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, 415},
		{"cross-site post that hides its origin", "POST", reply, func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, 403},
		{"cross-site delete", "DELETE", "/api/efforts/x", func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }, 403},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if code, _ := f.do(tt.method, tt.path, `{"text":"hi"}`, tt.edit); code != tt.want {
				t.Errorf("status = %d, want %d", code, tt.want)
			}
		})
	}
	if n := len(f.state().Sessions[0].Replies); n != 2 {
		t.Errorf("%d replies were queued; only the two allowed posts should have been", n)
	}
}

func TestStateIsShapedForTheUI(t *testing.T) {
	f := newFixture(t)
	_, body := f.do("GET", "/api/sessions", "", nil)
	for _, field := range []string{`"sessions":[]`, `"items":[]`, `"efforts":[]`, `"watch":[]`, `"questions":[]`, `"context":[]`, `"routing":[]`, `"answerPermissions":true`} {
		if !strings.Contains(body, field) {
			t.Errorf("empty state lacks %s, which the page reads without checking: %s", field, body)
		}
	}
}

func TestReplyTravelsFromTheUIToTheBridge(t *testing.T) {
	f := newFixture(t)
	if code := f.post("/api/sessions/ghost/reply", `{"text":"hi"}`); code != 404 {
		t.Errorf("reply to an unknown session = %d, want 404", code)
	}
	f.connect("s1")
	if code := f.post("/api/sessions/s1/reply", `{"text":"   "}`); code != 400 {
		t.Errorf("blank reply = %d, want 400", code)
	}
	code, body := f.do("POST", "/api/sessions/s1/reply", `{"text":"go ahead"}`, nil)
	var queued hub.Reply
	if json.Unmarshal([]byte(body), &queued); code != 202 || queued.ID == "" || queued.State != hub.Queued {
		t.Fatalf("reply = %d %s", code, body)
	}

	poll := func() []hub.Reply {
		_, body := f.do("POST", "/api/bridge/poll", `{"sessionId":"s1"}`, nil)
		var out struct{ Replies []hub.Reply }
		if err := json.Unmarshal([]byte(body), &out); err != nil || out.Replies == nil {
			t.Fatalf("poll body %s: replies must be a list, even when empty", body)
		}
		return out.Replies
	}
	if got := poll(); len(got) != 1 || got[0].Text != "go ahead" {
		t.Fatalf("first poll = %+v", got)
	}
	if got := poll(); len(got) != 0 {
		t.Fatalf("second poll handed the reply out again: %+v", got)
	}

	if code := f.post("/api/bridge/ack", `{"id":"`+queued.ID+`","state":"queued"}`); code != 400 {
		t.Errorf("ack with a state a bridge may not set = %d, want 400", code)
	}
	if code := f.post("/api/bridge/ack", `{"id":"`+queued.ID+`","state":"delivered"}`); code != 204 {
		t.Errorf("ack = %d", code)
	}
	if r := f.state().Sessions[0].Replies; len(r) != 1 || r[0].State != hub.Delivered {
		t.Errorf("after ack: %+v", r)
	}
}

func TestBridgeEventsDriveSessionState(t *testing.T) {
	f := newFixture(t)
	event := func(body string) {
		t.Helper()
		if code := f.post("/api/bridge/event", body); code != 204 {
			t.Fatalf("event %s = %d", body, code)
		}
	}
	only := func() hub.Session { return f.state().Sessions[0] }

	event(`{"sessionId":"s1","type":"turn.start","text":"fix the bug"}`)
	if s := only(); s.Status != hub.Working || s.LastPrompt != "fix the bug" {
		t.Errorf("after turn.start: %+v", s)
	}
	event(`{"sessionId":"s1","type":"turn.start","text":""}`)
	if s := only(); s.LastPrompt != "fix the bug" {
		t.Errorf("a continuation with no text erased the prompt: %q", s.LastPrompt)
	}
	event(`{"sessionId":"s1","type":"turn.complete","text":"fixed"}`)
	if s := only(); s.Status != hub.Idle || s.LastReply != "fixed" {
		t.Errorf("after turn.complete: %+v", s)
	}

	f.post("/api/bridge/permission", `{"sessionId":"s1","tool":"Bash","summary":"ls","input":"ls","canAnswer":true}`)
	event(`{"sessionId":"s1","type":"tool.done"}`)
	if s := only(); s.Pending != nil || s.Status != hub.Working {
		t.Errorf("after the tool ran: status %q pending %+v", s.Status, s.Pending)
	}
	event(`{"sessionId":"s1","type":"tool.done"}`)
	if s := only(); s.Status != hub.Working {
		t.Errorf("a tool finishing with nothing pending changed the status to %q", s.Status)
	}
}

func TestPermissionAnsweredFromTheUI(t *testing.T) {
	f := newFixture(t)
	ask := func(canAnswer string) (id string, answerable bool) {
		t.Helper()
		_, body := f.do("POST", "/api/bridge/permission", `{"sessionId":"s1","tool":"Bash","summary":"rm x","input":"rm x","canAnswer":`+canAnswer+`}`, nil)
		var out struct {
			ID         string
			Answerable bool
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil || out.ID == "" {
			t.Fatalf("permission = %s", body)
		}
		return out.ID, out.Answerable
	}
	wait := func(id string) string {
		t.Helper()
		_, body := f.do("POST", "/api/bridge/permission/wait", `{"id":"`+id+`"}`, nil)
		return strings.TrimSpace(body)
	}

	id, answerable := ask("true")
	if !answerable {
		t.Fatal("a request the bridge offered was not answerable")
	}
	if p := f.state().Sessions[0].Pending; p == nil || p.Input != "rm x" || !p.Answerable {
		t.Fatalf("the UI cannot see what it is being asked to allow: %+v", p)
	}
	if code := f.post("/api/permissions/"+id, `{"behavior":"always"}`); code != 400 {
		t.Errorf("an answer Slorp does not offer = %d, want 400", code)
	}
	if code := f.post("/api/permissions/"+id, `{"behavior":"deny","message":"not that file"}`); code != 204 {
		t.Fatalf("deny = %d", code)
	}
	if got := wait(id); got != `{"state":"deny","message":"not that file"}` {
		t.Errorf("the bridge was told %s", got)
	}
	if code := f.post("/api/permissions/"+id, `{"behavior":"allow"}`); code != 409 {
		t.Errorf("a second answer = %d, want 409", code)
	}

	id, _ = ask("true")
	f.post("/api/bridge/permission/done", `{"id":"`+id+`"}`)
	if got := wait(id); got != `{"state":"terminal"}` {
		t.Errorf("after the terminal answered, the bridge was told %s", got)
	}
	if code := f.post("/api/permissions/"+id, `{"behavior":"allow"}`); code != 409 {
		t.Errorf("answering a request the terminal settled = %d, want 409", code)
	}

	id, answerable = ask("false")
	if answerable || wait(id) != `{"state":"terminal"}` {
		t.Error("a request the bridge could not offer was treated as answerable")
	}

	if code := f.post("/api/settings", `{"answerPermissions":false}`); code != 204 {
		t.Fatalf("settings = %d", code)
	}
	if _, answerable = ask("true"); answerable {
		t.Error("the setting to keep permissions in the terminal was ignored")
	}
}

func TestEffortsOverHTTP(t *testing.T) {
	f := newFixture(t)
	f.connect("s1")
	if code := f.post("/api/efforts", `{"name":"  "}`); code != 400 {
		t.Errorf("blank effort = %d, want 400", code)
	}
	if code := f.post("/api/efforts", `{"name":"Snapshot store","about":"Postgres snapshots"}`); code != 204 {
		t.Fatalf("create = %d", code)
	}
	const id = "snapshot-store"

	if code := f.post("/api/sessions/s1/effort", `{"effort":"nope"}`); code != 400 {
		t.Errorf("filing under an unknown effort = %d, want 400", code)
	}
	if code := f.post("/api/sessions/s1/effort", `{"effort":"`+id+`"}`); code != 204 {
		t.Fatalf("file = %d", code)
	}
	if code := f.post("/api/efforts/"+id+"/dirs", `{"dir":"/work/a"}`); code != 204 {
		t.Fatalf("add dir = %d", code)
	}
	st := f.state()
	if e := st.Efforts[0]; e.ID != id || e.About != "Postgres snapshots" || len(e.Dirs) != 1 {
		t.Errorf("effort = %+v", e)
	}
	if s := st.Sessions[0]; s.Effort != id || s.EffortVia != kb.ViaUser {
		t.Errorf("session filing = %q via %q", s.Effort, s.EffortVia)
	}

	if code, _ := f.do("DELETE", "/api/efforts/"+id, "", nil); code != 204 {
		t.Fatalf("delete = %d", code)
	}
	if code, _ := f.do("DELETE", "/api/efforts/"+id, "", nil); code != 400 {
		t.Errorf("deleting it again = %d, want 400", code)
	}
	if st := f.state(); len(st.Efforts) != 0 || st.Sessions[0].Effort != "" {
		t.Errorf("after delete: %d efforts, session filed under %q", len(st.Efforts), st.Sessions[0].Effort)
	}
}

func TestKnowledgeOverHTTP(t *testing.T) {
	f := newFixture(t)
	if code := f.post("/api/kb/context", `{"text":"The fork only lags."}`); code != 204 {
		t.Fatalf("add = %d", code)
	}
	if code := f.post("/api/kb/nonsense", `{"text":"x"}`); code != 400 {
		t.Errorf("unknown section = %d, want 400", code)
	}
	if code := f.post("/api/kb/context", `not json`); code != 400 {
		t.Errorf("malformed body = %d, want 400", code)
	}
	facts := f.state().Knowledge.Context
	if len(facts) != 1 || facts[0].Text != "The fork only lags." {
		t.Fatalf("context = %+v", facts)
	}
	if code, _ := f.do("DELETE", "/api/kb/context/"+facts[0].ID, "", nil); code != 204 {
		t.Errorf("remove = %d", code)
	}
	if code, _ := f.do("DELETE", "/api/kb/context/"+facts[0].ID, "", nil); code != 400 {
		t.Errorf("removing it again = %d, want 400", code)
	}
}

func TestEventsStreamAnnouncesChanges(t *testing.T) {
	f := newFixture(t)
	res, err := f.srv.Client().Get(f.srv.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type = %q", ct)
	}
	read := func() string {
		buf := make([]byte, 64)
		n, err := res.Body.Read(buf)
		if err != nil {
			t.Fatalf("stream ended: %v", err)
		}
		return string(buf[:n])
	}
	if got := read(); got != "data: hello\n\n" {
		t.Fatalf("first event = %q", got)
	}
	f.connect("s1")
	if got := read(); got != "data: change\n\n" {
		t.Errorf("after a change, event = %q", got)
	}
	f.post("/api/efforts", `{"name":"A"}`)
	if got := read(); got != "data: change\n\n" {
		t.Errorf("after a knowledge base edit, event = %q", got)
	}
}
