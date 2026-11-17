package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jefflinse/slorp/internal/hub"
	"github.com/jefflinse/slorp/internal/integrations"
	"github.com/jefflinse/slorp/internal/kb"
	"github.com/jefflinse/slorp/internal/testdb"
)

type fixture struct {
	t   *testing.T
	srv *httptest.Server
	hub *hub.Hub
	kb  *kb.Store
	// feeds counts what the server asked of the feed poller.
	feeds *asked
	// probes counts how often the integration's availability was checked.
	probes *int
}

type asked struct{ checks, rescans int }

func (a *asked) Check()  { a.checks++ }
func (a *asked) Rescan() { a.rescans++ }

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st := testdb.Open(t)
	k := testdb.KB(t, st)
	h := hub.New(k, st)
	page := filepath.Join(t.TempDir(), "index.html")
	if err := os.WriteFile(page, []byte("<title>slorp</title>"), 0o600); err != nil {
		t.Fatal(err)
	}
	feeds, probes := &asked{}, new(int)
	tools := integrations.NewManager(func() map[string]bool { return h.Settings().Integrations }, integrations.Integration{
		ID: "fake", Name: "Fake", About: "A stand-in.",
		Check: func(context.Context) (bool, string) {
			*probes++
			return true, "Ready."
		},
		Scope: func(string) (string, bool) { return "", true },
		Tools: []integrations.Tool{{Name: "fake_lookup", Description: "Look something up."}},
	})
	srv := httptest.NewServer(New(h, k, tools, feeds, page))
	t.Cleanup(srv.Close)
	return &fixture{t, srv, h, k, feeds, probes}
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

func TestBridgeSaysHowMuchItsModelCanTake(t *testing.T) {
	f := newFixture(t)
	window := func() int { return f.state().Sessions[0].ContextWindow }

	f.connect("s1")
	if got := window(); got != 0 {
		t.Fatalf("window = %d before the bridge gave one", got)
	}
	f.post("/api/bridge/poll", `{"sessionId":"s1","contextWindow":1000000}`)
	if got := window(); got != 1000000 {
		t.Fatalf("window = %d, want what the bridge reported", got)
	}
	f.post("/api/bridge/poll", `{"sessionId":"s1","contextWindow":0}`)
	if got := window(); got != 1000000 {
		t.Errorf("window = %d; a bridge that does not know yet erased what was known", got)
	}
	f.post("/api/bridge/poll", `{"sessionId":"s1","contextWindow":200000}`)
	if got := window(); got != 200000 {
		t.Errorf("window = %d after the session changed model", got)
	}
}

func TestBridgeSaysWhatItsSessionHasCost(t *testing.T) {
	f := newFixture(t)
	cost := func() float64 { return f.state().Sessions[0].CostUSD }

	f.post("/api/bridge/poll", `{"sessionId":"s1","costUsd":1.25}`)
	if got := cost(); got != 1.25 {
		t.Fatalf("cost = %v, want what the bridge reported", got)
	}
	f.post("/api/bridge/poll", `{"sessionId":"s1"}`)
	if got := cost(); got != 1.25 {
		t.Errorf("cost = %v; a bridge with no estimate erased the last one", got)
	}
	f.post("/api/bridge/poll", `{"sessionId":"s1","costUsd":2.5}`)
	if got := cost(); got != 2.5 {
		t.Errorf("cost = %v after the session went on", got)
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
	for _, field := range []string{`"sessions":[]`, `"items":[]`, `"efforts":[]`, `"watch":[]`, `"questions":[]`, `"context":[]`, `"routing":[]`, `"answerPermissions":true`, `"lookups":[]`, `"integrations":[{`, `"feeds":{}`, `"watches":[]`} {
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

func TestBridgeNamesItsAgent(t *testing.T) {
	f := newFixture(t)
	poll := func(body string) (fresh bool) {
		t.Helper()
		code, res := f.do("POST", "/api/bridge/poll", body, nil)
		var out struct{ Fresh *bool }
		if err := json.Unmarshal([]byte(res), &out); code != 200 || err != nil || out.Fresh == nil {
			t.Fatalf("poll %s = %d %s", body, code, res)
		}
		return *out.Fresh
	}
	byID := func(id string) hub.Session {
		t.Helper()
		for _, s := range f.state().Sessions {
			if s.ID == id {
				return s
			}
		}
		t.Fatalf("session %q is not listed", id)
		return hub.Session{}
	}

	if !poll(`{"agent":"opencode","sessionId":"o1","cwd":"/work/o","title":"Fix the build","branch":"main"}`) {
		t.Error("the poll that introduced a session did not say Slorp had not known it")
	}
	if s := byID("o1"); s.Agent != "opencode" || s.Cwd != "/work/o" || s.Title != "Fix the build" || s.Branch != "main" {
		t.Errorf("an OpenCode session was recorded as %+v", s)
	}
	if poll(`{"agent":"opencode","sessionId":"o1","cwd":"/elsewhere","title":"","branch":""}`) {
		t.Error("a later poll for the same session said Slorp had not known it")
	}
	if s := byID("o1"); s.Cwd != "/work/o" || s.Title != "Fix the build" || s.Branch != "main" {
		t.Errorf("a poll with less detail erased what was known: %+v", s)
	}
	poll(`{"agent":"opencode","sessionId":"o1","title":"Fix the release build","branch":"release"}`)
	if s := byID("o1"); s.Title != "Fix the release build" || s.Branch != "release" {
		t.Errorf("a renamed session on a new branch is still %q on %q", s.Title, s.Branch)
	}

	poll(`{"sessionId":"c1"}`)
	if s := byID("c1"); s.Agent != "claude" {
		t.Errorf("a bridge that names no agent was recorded as %q, want claude", s.Agent)
	}

	if code := f.post("/api/bridge/event", `{"agent":"opencode","sessionId":"o2","type":"turn.start","text":"hi"}`); code != 204 {
		t.Fatalf("event = %d", code)
	}
	if code := f.post("/api/bridge/permission", `{"agent":"opencode","sessionId":"o3","tool":"bash","summary":"ls","input":"ls","canAnswer":true}`); code != 200 {
		t.Fatalf("permission = %d", code)
	}
	if a, b := byID("o2").Agent, byID("o3").Agent; a != "opencode" || b != "opencode" {
		t.Errorf("sessions an OpenCode bridge reported on were recorded as %q and %q", a, b)
	}

	before := len(f.state().Sessions)
	for _, path := range []string{"/api/bridge/poll", "/api/bridge/event", "/api/bridge/permission"} {
		if code := f.post(path, `{"agent":"cursor","sessionId":"x1","type":"turn.start","tool":"bash"}`); code != 400 {
			t.Errorf("%s from an agent Slorp does not know = %d, want 400", path, code)
		}
	}
	if after := len(f.state().Sessions); after != before {
		t.Errorf("an unknown agent's report created %d session(s)", after-before)
	}
}

func TestAQuestionOverHTTP(t *testing.T) {
	f := newFixture(t)
	_, body := f.do("POST", "/api/bridge/permission", `{"sessionId":"s1","tool":"AskUserQuestion","summary":"Which one?","input":"{}","canAnswer":true,"question":true}`, nil)
	if !strings.Contains(body, `"answerable":false`) {
		t.Errorf("permission = %s, want a question refused for answering in Slorp", body)
	}
	st := f.state()
	if p := st.Sessions[0].Pending; p == nil || !p.Question || len(st.Items) != 1 || st.Items[0].Kind != hub.KindQuestion || st.Items[0].Ask != "Which one?" {
		t.Errorf("pending = %+v, items = %+v; want a question awaiting the user", p, st.Items)
	}
	if _, body := f.do("GET", "/api/sessions", "", nil); !strings.Contains(body, `"question":true`) {
		t.Errorf("state lacks the question mark the page reads: %s", body)
	}

	input := `{"questions":[{"question":"Which soup?","multiSelect":false,"options":[{"label":"Tomato"},{"label":"No soup"}]},{"question":"Which sides?","multiSelect":true,"options":[{"label":"Pickle"},{"label":"Crisps"}]}]}`
	req, _ := json.Marshal(map[string]any{"sessionId": "s2", "tool": "AskUserQuestion", "summary": "Which soup?", "input": input, "canAnswer": true, "question": true})
	_, body = f.do("POST", "/api/bridge/permission", string(req), nil)
	var asked struct {
		ID         string
		Answerable bool
	}
	if json.Unmarshal([]byte(body), &asked) != nil || !asked.Answerable {
		t.Fatalf("permission = %s, want a readable question offered for answering", body)
	}
	if code := f.post("/api/permissions/"+asked.ID, `{"behavior":"allow"}`); code != 409 {
		t.Errorf("allowing a question without answers = %d, want 409", code)
	}
	if code := f.post("/api/permissions/"+asked.ID, `{"answers":{"Which soup?":["Gazpacho"],"Which sides?":["Pickle"]}}`); code != 409 {
		t.Errorf("an answer that was not offered = %d, want 409", code)
	}
	if code := f.post("/api/permissions/"+asked.ID, `{"answers":{"Which soup?":["Tomato"],"Which sides?":["Crisps","Pickle"]}}`); code != 204 {
		t.Fatalf("answering a question = %d", code)
	}
	if _, body := f.do("POST", "/api/bridge/permission/wait", `{"id":"`+asked.ID+`"}`, nil); strings.TrimSpace(body) != `{"state":"allow","answers":{"Which sides?":"Pickle, Crisps","Which soup?":"Tomato"}}` {
		t.Errorf("the bridge was told %s", body)
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

func TestIntegrationsOverHTTP(t *testing.T) {
	f := newFixture(t)
	list := func() []integrations.Status {
		_, body := f.do("GET", "/api/sessions", "", nil)
		var out struct{ Integrations []integrations.Status }
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatal(err)
		}
		return out.Integrations
	}
	if got := list(); len(got) != 1 || got[0].ID != "fake" || got[0].Enabled || !got[0].Available || got[0].Lookups[0] != "Look something up." {
		t.Fatalf("integrations = %+v; want the one integration, off until the user turns it on", got)
	}
	if code := f.post("/api/integrations/nope", `{"enabled":true}`); code != 404 {
		t.Errorf("unknown integration = %d, want 404", code)
	}
	if code := f.post("/api/integrations/fake", `{"enabled":true}`); code != 204 {
		t.Fatalf("turn on = %d", code)
	}
	if !list()[0].Enabled {
		t.Error("the integration did not turn on")
	}
	if code := f.post("/api/settings", `{"answerPermissions":false}`); code != 204 || !list()[0].Enabled {
		t.Error("changing another setting turned the integration off")
	}
	f.post("/api/integrations/fake", `{"enabled":false}`)
	if list()[0].Enabled {
		t.Error("the integration did not turn off")
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

func TestFeedItemsOverHTTP(t *testing.T) {
	f := newFixture(t)
	f.post("/api/efforts", `{"name":"Widgets"}`)
	f.hub.SetFeed("fake", []hub.Item{{ID: "fake-1", Kind: "ready-to-merge", Title: "Ready", URL: "https://example.com/1", Repo: "Acme/Widgets"}},
		[]integrations.PullRequest{{Repo: "Acme/Widgets", Number: 7, Title: "Add a thing", URL: "https://example.com/7", Author: "sam", Approvals: 2, Awaiting: 1, Checks: "passing", Needs: "Waiting on review."}}, nil)

	_, body := f.do("GET", "/api/sessions", "", nil)
	for _, field := range []string{`"pullRequests":[{"repo":"Acme/Widgets","number":7,"title":"Add a thing","url":"https://example.com/7","author":"sam","mine":false,"draft":false,`,
		`"approvals":2,"changesRequested":0,"awaiting":1,"checks":"passing","conflicts":false,"onYou":false,"urgency":"","needs":"Waiting on review.","source":"fake","effort":""}]`,
		`"id":"fake-1"`, `"source":"fake"`, `"url":"https://example.com/1"`, `"repo":"Acme/Widgets"`, `"effort":""`, `"replies":[]`, `"feeds":{"fake":{"at":"`, `"count":1`} {
		if !strings.Contains(body, field) {
			t.Errorf("state lacks %s, which the page reads: %s", field, body)
		}
	}
	if strings.Contains(body, `"sessionId"`) {
		t.Errorf("a feed item claims a session: %s", body)
	}

	before := f.feeds.rescans
	if code := f.post("/api/repos/effort", `{"repo":"acme/widgets","effort":"widgets"}`); code != 204 {
		t.Fatalf("file a repository = %d", code)
	}
	if it := f.state().Items[0]; it.Effort != "widgets" || it.EffortVia != kb.ViaUser {
		t.Errorf("item filed %q via %q, want the user's filing of its repository", it.Effort, it.EffortVia)
	}
	if f.feeds.rescans != before+1 {
		t.Errorf("%d rescans after a filing, want 1: an edit may change which directories matter", f.feeds.rescans-before)
	}
	if code := f.post("/api/repos/effort", `{"repo":"acme/widgets","effort":""}`); code != 204 || f.state().Items[0].EffortVia != kb.ViaUser || f.state().Items[0].Effort != "" {
		t.Errorf("leaving a repository unsorted = %d, item %+v", code, f.state().Items[0])
	}
	if code := f.post("/api/repos/effort", `{"repo":"acme/widgets","effort":"nope"}`); code != 400 {
		t.Errorf("filing under an unknown effort = %d, want 400", code)
	}
	if code := f.post("/api/repos/effort", `{"repo":"","effort":"widgets"}`); code != 400 {
		t.Errorf("filing no repository = %d, want 400", code)
	}
	if code := f.post("/api/repos/effort", `{`); code != 400 {
		t.Errorf("a malformed filing = %d, want 400", code)
	}
}

func TestFeedIsCollectedWhenAsked(t *testing.T) {
	f := newFixture(t)
	f.state()
	probes := *f.probes

	if code := f.post("/api/feed/check", `{}`); code != 204 || f.feeds.checks != 1 {
		t.Fatalf("check now = %d with %d collections asked for, want 204 and 1", code, f.feeds.checks)
	}
	f.state()
	if *f.probes != probes+1 {
		t.Errorf("availability probed %d more times, want 1: checking now should notice a tool that was just signed in", *f.probes-probes)
	}

	if code := f.post("/api/integrations/fake", `{"enabled":true}`); code != 204 || f.feeds.checks != 2 {
		t.Errorf("turning an integration on asked for %d collections, want its feed collected at once", f.feeds.checks-1)
	}
	if code := f.post("/api/integrations/fake", `{"enabled":false}`); code != 204 || f.feeds.checks != 3 {
		t.Errorf("turning an integration off asked for %d collections, want one so its items go at once", f.feeds.checks-2)
	}
	if code := f.post("/api/integrations/nope", `{"enabled":true}`); code != 404 || f.feeds.checks != 3 {
		t.Errorf("an unknown integration = %d and asked for a collection", code)
	}

	rescans := f.feeds.rescans
	f.post("/api/efforts", `{"name":"A"}`)
	f.post("/api/efforts/a/dirs", `{"dir":"/work/a"}`)
	if f.feeds.rescans != rescans+2 {
		t.Errorf("%d rescans after two edits, want 2", f.feeds.rescans-rescans)
	}
	f.post("/api/efforts/nope/dirs", `{"dir":"/work/a"}`)
	if f.feeds.rescans != rescans+2 {
		t.Error("a refused edit caused a rescan")
	}
}

func TestRolesOverHTTP(t *testing.T) {
	f := newFixture(t)
	f.connect("s1")
	if code := f.post("/api/kb/roles", `{"name":"Reviewer","text":"Reviews pull requests","type":"stopped"}`); code != 204 {
		t.Fatalf("add a role = %d", code)
	}
	roles := f.state().Knowledge.Roles
	if len(roles) != 1 || roles[0].Name != "Reviewer" || roles[0].About != "Reviews pull requests" || roles[0].Attention != kb.AttentionStopped {
		t.Fatalf("roles = %+v", roles)
	}
	id := roles[0].ID
	_, body := f.do("GET", "/api/sessions", "", nil)
	for _, field := range []string{`"roles":[{"id":"` + id + `","name":"Reviewer","about":"Reviews pull requests","attention":"stopped"}]`, `"role":""`} {
		if !strings.Contains(body, field) {
			t.Errorf("state lacks %s, which the page reads: %s", field, body)
		}
	}

	if code := f.post("/api/sessions/s1/role", `{"role":"`+id+`"}`); code != 204 {
		t.Fatalf("give a session a role = %d", code)
	}
	if s := f.state().Sessions[0]; s.Role != id || s.RoleVia != kb.ViaUser {
		t.Errorf("session role %q via %q, want the user's choice", s.Role, s.RoleVia)
	}
	if code := f.post("/api/roles/"+id, `{"attention":"none"}`); code != 204 || f.state().Knowledge.Roles[0].Attention != kb.AttentionNone {
		t.Errorf("change a role's attention = %d, roles %+v", code, f.state().Knowledge.Roles)
	}
	for what, req := range map[string][2]string{
		"a role that does not exist":       {"/api/sessions/s1/role", `{"role":"nope"}`},
		"a malformed choice of role":       {"/api/sessions/s1/role", `{`},
		"an attention that does not exist": {"/api/roles/" + id, `{"attention":"loud"}`},
		"attention for an unknown role":    {"/api/roles/nope", `{"attention":"none"}`},
		"a malformed change of attention":  {"/api/roles/" + id, `{`},
	} {
		if code := f.post(req[0], req[1]); code != 400 {
			t.Errorf("%s = %d, want 400", what, code)
		}
	}
	if code := f.post("/api/sessions/s1/role", `{"role":""}`); code != 204 || f.state().Sessions[0].Role != "" || f.state().Sessions[0].RoleVia != kb.ViaUser {
		t.Errorf("give a session no role = %d, session %+v", code, f.state().Sessions[0])
	}
	if code, _ := f.do("DELETE", "/api/kb/roles/"+id, "", nil); code != 204 || len(f.state().Knowledge.Roles) != 0 {
		t.Errorf("remove a role = %d, roles %+v", code, f.state().Knowledge.Roles)
	}
}
