package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

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
	// record is what a Claude session's own record is read as holding.
	record *record
	// launcher is what the server asked of Claude Code's launcher.
	launcher *launcher
	// prober is what the server asked to have tried of the local model.
	prober *prober
}

// launcher records what it was asked to start, reopen, stop and install.
type launcher struct {
	calls []string
	// id is the session id a start reports, and err what every call fails with.
	id  string
	err error
}

func (l *launcher) Start(_ context.Context, dir, name, prompt string) (string, error) {
	l.calls = append(l.calls, "start "+dir+"|"+name+"|"+prompt)
	return l.id, l.err
}

func (l *launcher) Resume(_ context.Context, sessionID, dir, prompt string) error {
	l.calls = append(l.calls, "resume "+sessionID+"|"+dir+"|"+prompt)
	return l.err
}

func (l *launcher) Stop(_ context.Context, job string) error {
	l.calls = append(l.calls, "stop "+job)
	return l.err
}

func (l *launcher) Install(context.Context) error {
	l.calls = append(l.calls, "install")
	return l.err
}

func (l *launcher) Uninstall(context.Context) error {
	l.calls = append(l.calls, "uninstall")
	return l.err
}

// prober stands in for the local model, recording what it was asked to try.
type prober struct {
	tried []*hub.ModelSettings
	says  hub.ModelCheck
}

func (p *prober) Probe(_ context.Context, m *hub.ModelSettings) hub.ModelCheck {
	p.tried = append(p.tried, m)
	return p.says
}

type asked struct{ checks, rescans int }

func (a *asked) Check()  { a.checks++ }
func (a *asked) Rescan() { a.rescans++ }

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st := testdb.Open(t)
	k := testdb.KB(t, st)
	h := hub.New(k, st)
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
	rec, launch, probe := &record{}, &launcher{}, &prober{}
	srv := httptest.NewServer(New(h, k, tools, feeds, map[string]History{"claude": rec.read}, map[string]Launcher{"claude": launch}, map[string]Delegates{"claude": rec}, probe, Page([]byte("<title>slorp</title>"), "")))
	t.Cleanup(srv.Close)
	return &fixture{t, srv, h, k, feeds, probes, rec, launch, probe}
}

type record struct {
	exchanges []hub.Exchange
	err       error
	asked     []string
	// subagents are what each session is read as having started, by
	// session, and reports what each reported, by session and subagent.
	subagents map[string][]hub.Subagent
	reports   map[string]hub.SubagentReport
}

func (r *record) Subagents(sessionID string) []hub.Subagent { return r.subagents[sessionID] }

func (r *record) Report(sessionID, id string) (hub.SubagentReport, bool) {
	report, ok := r.reports[sessionID+"/"+id]
	return report, ok
}

func (r *record) read(sessionID string, limit int) ([]hub.Exchange, error) {
	r.asked = append(r.asked, sessionID+" "+strconv.Itoa(limit))
	return r.exchanges, r.err
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

func TestATurnOpensAMomentInTheTimeline(t *testing.T) {
	f := newFixture(t)
	f.connect("s1")
	f.post("/api/bridge/event", `{"sessionId":"s1","type":"turn.start","text":"fix the rounding"}`)
	f.post("/api/bridge/event", `{"sessionId":"s1","type":"turn.complete","text":"Done."}`)
	f.post("/api/bridge/event", `{"sessionId":"s1","type":"turn.start","text":"now ship it"}`)
	f.post("/api/bridge/event", `{"sessionId":"s1","type":"turn.start","text":"fix the rounding"}`)

	got := f.state().Sessions[0].Timeline
	if len(got) != 3 || got[0].Prompt != "fix the rounding" || got[1].Prompt != "now ship it" || got[2].Prompt != "fix the rounding" ||
		!got[1].At.After(got[0].At) || time.Since(got[0].At) > time.Minute {
		t.Errorf("timeline = %+v; want every prompt in order, timed as it arrived, a repeated one included", got)
	}
}

func TestBridgeReportsUsageWindows(t *testing.T) {
	f := newFixture(t)
	at := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	resets := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	body, _ := json.Marshal(map[string]any{"sessionId": "s1", "measuredAt": at.UnixMilli(), "rateLimits": []map[string]any{
		{"kind": "five_hour", "percentUsed": 23.5, "resetsAt": resets.Format(time.RFC3339)},
		{"kind": strings.Repeat("k", 200), "percentUsed": -4, "resetsAt": "soon"},
	}})
	f.post("/api/bridge/poll", string(body))

	got := f.state().Limits
	if len(got) != 2 || got[0].Agent != "claude" || got[0].Kind != "five_hour" || got[0].PercentUsed != 23.5 || !got[0].ResetsAt.Equal(resets) || !got[0].At.Equal(at) {
		t.Fatalf("limits = %+v; want the window as the bridge reported it", got)
	}
	if len(got[1].Kind) != 80 || got[1].PercentUsed != 0 || !got[1].ResetsAt.IsZero() {
		t.Errorf("second window = %+v; want a long name cut, a negative share floored and an unreadable time dropped", got[1])
	}

	many := make([]map[string]any, 20)
	for i := range many {
		many[i] = map[string]any{"kind": "w", "percentUsed": 1}
	}
	body, _ = json.Marshal(map[string]any{"sessionId": "s1", "measuredAt": at.UnixMilli() + 1000, "rateLimits": many})
	f.post("/api/bridge/poll", string(body))
	if got := f.state().Limits; len(got) != 8 {
		t.Errorf("%d windows from one bridge, want no more than 8", len(got))
	}
	f.post("/api/bridge/poll", `{"sessionId":"s1"}`)
	if got := f.state().Limits; len(got) != 8 {
		t.Errorf("a poll with no reading left %d windows, want the last reading kept", len(got))
	}
}

func TestBackfillingASession(t *testing.T) {
	f := newFixture(t)
	f.connect("s1")
	f.post("/api/bridge/poll", `{"agent":"opencode","sessionId":"o1"}`)
	at := time.Now().Add(-time.Hour)
	f.record.exchanges = []hub.Exchange{{At: at, Prompt: "fix the rounding", Reply: "Fixed."}, {At: at.Add(time.Minute), Prompt: "now ship it"}}

	if code := f.post("/api/sessions/nobody/backfill", ""); code != http.StatusNotFound {
		t.Errorf("backfilling a session nobody knows = %d, want 404", code)
	}
	if code := f.post("/api/sessions/o1/backfill", ""); code != http.StatusBadRequest || len(f.record.asked) != 0 {
		t.Errorf("backfilling a session whose agent keeps no record = %d, having read %v; want 400 and nothing read", code, f.record.asked)
	}
	code, body := f.do("POST", "/api/sessions/s1/backfill", "", nil)
	if code != http.StatusAccepted || !strings.Contains(body, `"exchanges":2`) || !strings.Contains(body, `"queued":1`) {
		t.Fatalf("backfilling = %d %s; want it taken, counting what was read and what waits for the model", code, body)
	}
	if len(f.record.asked) != 1 || f.record.asked[0] != "s1 "+strconv.Itoa(hub.BackfillLimit) {
		t.Errorf("read %v, want that session's record once, up to the limit", f.record.asked)
	}
	var got hub.Session
	for _, s := range f.state().Sessions {
		if s.ID == "s1" {
			got = s
		}
	}
	if len(got.Timeline) != 2 || got.Timeline[0].Prompt != "fix the rounding" || got.Backfilling != 1 {
		t.Errorf("session = %+v; want both exchanges in its timeline and one waiting", got)
	}

	f.record.err = errors.New("no transcript was found for the session")
	if code, body := f.do("POST", "/api/sessions/s1/backfill", "", nil); code != http.StatusBadGateway || !strings.Contains(body, "no transcript") {
		t.Errorf("a record that cannot be read = %d %s, want 502 saying why", code, body)
	}
}

func TestAClosedSessionCanBeReadBack(t *testing.T) {
	f := newFixture(t)
	f.connect("s1")
	f.post("/api/bridge/event", `{"sessionId":"s1","type":"turn.start","text":"fix the rounding"}`)
	if code, _ := f.do("GET", "/api/archive/s1", "", nil); code != http.StatusNotFound {
		t.Errorf("reading back an open session = %d, want 404", code)
	}
	f.hub.SetRoster("claude", nil)

	code, body := f.do("GET", "/api/archive/s1", "", nil)
	var got hub.Archived
	if err := json.Unmarshal([]byte(body), &got); code != http.StatusOK || err != nil || got.ID != "s1" || len(got.Timeline) != 1 || got.Timeline[0].Prompt != "fix the rounding" {
		t.Errorf("reading it back = %d %s (%v); want the session with its timeline", code, body, err)
	}
	if listed := f.state().Archive; len(listed) != 1 || listed[0].Moments != 1 || listed[0].Timeline != nil {
		t.Errorf("listed = %+v, want it without its timeline", listed)
	}
}

func TestAskingForAWrapup(t *testing.T) {
	f := newFixture(t)
	if code := f.post("/api/wrapup", `{"day":"tomorrow"}`); code != http.StatusBadRequest {
		t.Errorf("asking for no day at all = %d, want 400", code)
	}
	if code := f.post("/api/wrapup", `{"day":"2026-11-17"}`); code != http.StatusAccepted {
		t.Fatalf("asking for a day = %d, want 202", code)
	}
	if got := f.state().Wrapups; len(got) != 1 || got["2026-11-17"].State != hub.WrapupEmpty {
		t.Errorf("wrap-ups = %+v, want that day, empty for want of anything said", got)
	}
}

func TestNamingASession(t *testing.T) {
	f := newFixture(t)
	f.connect("s1")
	label := func() string { return f.state().Sessions[0].Label }

	if code := f.post("/api/sessions/s1/name", `{"name":"  Loop reviewer \n"}`); code != http.StatusNoContent {
		t.Fatalf("naming = %d", code)
	}
	if got := label(); got != "Loop reviewer" {
		t.Errorf("label = %q, want the name without the space around it", got)
	}
	f.post("/api/sessions/s1/name", `{"name":"`+strings.Repeat("é", 200)+`"}`)
	if got := label(); got != strings.Repeat("é", 80) {
		t.Errorf("label is %d runes, want a long name cut to 80", len([]rune(got)))
	}
	f.post("/api/sessions/s1/name", `{"name":"   "}`)
	if got := label(); got != "s1" {
		t.Errorf("label = %q; a blank name should hand naming back", got)
	}
	if code := f.post("/api/sessions/nobody/name", `{"name":"x"}`); code != http.StatusNotFound {
		t.Errorf("naming a session nobody knows = %d, want 404", code)
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
	for _, field := range []string{`"sessions":[]`, `"items":[]`, `"efforts":[]`, `"watch":[]`, `"questions":[]`, `"context":[]`, `"routing":[]`, `"answerPermissions":true`, `"lookups":[]`, `"integrations":[{`, `"feeds":{}`, `"watches":[]`, `"plugins":{}`} {
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

func TestABridgeIsNotHeardAboutASessionTheRosterDoesNotList(t *testing.T) {
	f := newFixture(t)
	f.hub.Rostered("claude")
	f.hub.SetRoster("claude", []hub.Session{{ID: "listed", Agent: "claude", Status: hub.Idle}})

	code, res := f.do("POST", "/api/bridge/poll", `{"sessionId":"stray","cwd":"/w","title":"Left behind"}`, nil)
	if code != 200 || !strings.Contains(res, `"fresh":false`) {
		t.Errorf("poll = %d %s, want it answered without a session being opened", code, res)
	}
	if code := f.post("/api/bridge/event", `{"sessionId":"stray","type":"turn.start","text":"hello"}`); code != 204 {
		t.Errorf("event = %d", code)
	}
	code, res = f.do("POST", "/api/bridge/permission", `{"sessionId":"stray","tool":"Bash","summary":"ls","input":"ls","canAnswer":true}`, nil)
	if code != 200 || !strings.Contains(res, `"answerable":false`) {
		t.Errorf("permission = %d %s, want the prompt left to the terminal", code, res)
	}
	if got := f.state().Sessions; len(got) != 1 || got[0].ID != "listed" {
		t.Errorf("sessions = %+v, want only the one the roster lists", got)
	}
}

func TestInstallingAnAgentsPlugin(t *testing.T) {
	f := newFixture(t)
	if code := f.post("/api/plugins/claude", `{}`); code != 204 || len(f.launcher.calls) != 1 || f.launcher.calls[0] != "install" {
		t.Errorf("install = %d after %v, want it done by the agent's launcher", code, f.launcher.calls)
	}
	if code := f.post("/api/plugins/opencode", `{}`); code != 404 || len(f.launcher.calls) != 1 {
		t.Errorf("install for an agent with no launcher = %d after %v, want 404 and nothing run", code, f.launcher.calls)
	}
	f.launcher.err = errors.New("blocked by policy")
	if code, body := f.do("POST", "/api/plugins/claude", `{}`, nil); code != 502 || !strings.Contains(body, "blocked by policy") {
		t.Errorf("a failed install = %d %q, want 502 with what went wrong", code, body)
	}
}

func TestUninstallingAnAgentsPlugin(t *testing.T) {
	f := newFixture(t)
	if code, _ := f.do("DELETE", "/api/plugins/claude", ``, nil); code != 204 || len(f.launcher.calls) != 1 || f.launcher.calls[0] != "uninstall" {
		t.Errorf("uninstall = %d after %v, want it done by the agent's launcher", code, f.launcher.calls)
	}
	if code, _ := f.do("DELETE", "/api/plugins/opencode", ``, nil); code != 404 || len(f.launcher.calls) != 1 {
		t.Errorf("uninstall for an agent with no launcher = %d after %v, want 404 and nothing run", code, f.launcher.calls)
	}
	f.launcher.err = errors.New("blocked by policy")
	if code, body := f.do("DELETE", "/api/plugins/claude", ``, nil); code != 502 || !strings.Contains(body, "blocked by policy") {
		t.Errorf("a failed uninstall = %d %q, want 502 with what went wrong", code, body)
	}
}

func TestPage(t *testing.T) {
	get := func(h http.Handler) (string, string) {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
		if w.Code != 200 {
			t.Fatalf("status = %d", w.Code)
		}
		return w.Body.String(), w.Header().Get("Content-Type")
	}
	built := []byte("<title>built in</title>")
	if body, kind := get(Page(built, "")); body != "<title>built in</title>" || kind != "text/html; charset=utf-8" {
		t.Errorf("with no path, served %q as %q; want the built-in page as HTML", body, kind)
	}

	path := filepath.Join(t.TempDir(), "index.html")
	for _, want := range []string{"<title>on disk</title>", "<title>edited</title>"} {
		if err := os.WriteFile(path, []byte(want), 0o600); err != nil {
			t.Fatal(err)
		}
		if body, kind := get(Page(built, path)); body != want || kind != "text/html; charset=utf-8" {
			t.Errorf("with a path, served %q as %q; want the file as it now stands", body, kind)
		}
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

func TestStartingASession(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	f.post("/api/efforts", `{"name":"Snapshot store"}`)
	f.post("/api/kb/roles", `{"name":"Reviewer","text":"Reviews pull requests"}`)
	role := f.state().Knowledge.Roles[0].ID
	file := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	body := func(fields string) string {
		return `{"dir":` + strconv.Quote(dir) + `,"prompt":"review it"` + fields + `}`
	}

	for name, bad := range map[string]string{
		"no prompt":            `{"dir":` + strconv.Quote(dir) + `,"prompt":"  "}`,
		"a prompt too long":    `{"dir":` + strconv.Quote(dir) + `,"prompt":"` + strings.Repeat("x", startLimit+1) + `"}`,
		"no directory":         `{"prompt":"go"}`,
		"a relative directory": `{"dir":".","prompt":"go"}`,
		"a missing directory":  `{"dir":` + strconv.Quote(filepath.Join(dir, "gone")) + `,"prompt":"go"}`,
		"a file":               `{"dir":` + strconv.Quote(file) + `,"prompt":"go"}`,
		"an unknown effort":    body(`,"effort":"nope"`),
		"an unknown role":      body(`,"role":"nope"`),
		"an unknown agent":     body(`,"agent":"opencode"`),
	} {
		if code := f.post("/api/sessions", bad); code != 400 {
			t.Errorf("starting with %s = %d, want 400", name, code)
		}
	}
	if len(f.launcher.calls) != 0 {
		t.Fatalf("refused starts still reached the launcher: %q", f.launcher.calls)
	}

	f.launcher.id = "new-1"
	code, res := f.do("POST", "/api/sessions", body(`,"name":"  `+strings.Repeat("n", nameLimit+5)+`  ","effort":"snapshot-store","role":"`+role+`"`), nil)
	if code != 202 || !strings.Contains(res, `"sessionId":"new-1"`) {
		t.Fatalf("start = %d %s", code, res)
	}
	if want := "start " + dir + "|" + strings.Repeat("n", nameLimit) + "|review it"; f.launcher.calls[0] != want {
		t.Errorf("launcher was asked %q, want %q", f.launcher.calls[0], want)
	}
	if effort, via, _ := f.kb.Resolve("new-1", dir); effort != "snapshot-store" || via != kb.ViaUser {
		t.Errorf("started session filed under %q via %q, want the effort it was started for", effort, via)
	}
	if r, ok := f.kb.RoleOf("new-1"); !ok || r.ID != role {
		t.Errorf("started session has role %+v, want the one it was started with", r)
	}

	// A start the agent had not listed yet has no id to file under.
	f.launcher.id = ""
	if code := f.post("/api/sessions", body(`,"effort":"snapshot-store"`)); code != 202 {
		t.Errorf("start with no id yet = %d, want 202", code)
	}
	if _, _, ok := f.kb.Resolve("", dir); ok {
		t.Error("a start with no id filed a session with no id")
	}

	f.launcher.err = errors.New("not logged in")
	if code, res := f.do("POST", "/api/sessions", body(""), nil); code != 502 || !strings.Contains(res, "not logged in") {
		t.Errorf("a start claude refused = %d %q, want 502 saying why", code, res)
	}
}

func TestStartingInTheHomeDirectory(t *testing.T) {
	f := newFixture(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.Mkdir(filepath.Join(home, "proj"), 0o700); err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string]string{"~/proj/": filepath.Join(home, "proj"), " ~ ": home} {
		f.launcher.calls = nil
		if code := f.post("/api/sessions", `{"dir":"`+dir+`","prompt":"go"}`); code != 202 {
			t.Fatalf("start in %q = %d", dir, code)
		}
		if f.launcher.calls[0] != "start "+want+"||go" {
			t.Errorf("start in %q asked the launcher %q, want it in %s", dir, f.launcher.calls[0], want)
		}
	}
	if code := f.post("/api/sessions", `{"dir":"~proj","prompt":"go"}`); code != 400 {
		t.Errorf("start in ~proj = %d, want 400: only a bare ~ is home", code)
	}
}

func TestStoppingAndReopeningASession(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	f.hub.SetRoster("claude", []hub.Session{
		{ID: "bg", Agent: "claude", Kind: "background", Job: "3e14f875", Cwd: dir, LastPrompt: "do it", PromptAt: time.Now()},
		{ID: "term", Agent: "claude", Kind: "interactive", Cwd: dir, LastPrompt: "do it", PromptAt: time.Now()},
	})

	if code := f.post("/api/sessions/nope/stop", `{}`); code != 404 {
		t.Errorf("stopping an unknown session = %d, want 404", code)
	}
	if code := f.post("/api/sessions/term/stop", `{}`); code != 400 {
		t.Errorf("stopping a session in a terminal = %d, want 400", code)
	}
	if code := f.post("/api/archive/bg/resume", `{}`); code != 404 {
		t.Errorf("reopening an open session = %d, want 404", code)
	}
	if code := f.post("/api/sessions/bg/stop", `{}`); code != 204 {
		t.Fatalf("stop = %d", code)
	}
	if len(f.launcher.calls) != 1 || f.launcher.calls[0] != "stop 3e14f875" {
		t.Fatalf("launcher was asked %q, want one stop by the session's job", f.launcher.calls)
	}
	f.launcher.err = errors.New("No session matching")
	if code, res := f.do("POST", "/api/sessions/bg/stop", `{}`, nil); code != 502 || !strings.Contains(res, "No session matching") {
		t.Errorf("a stop claude refused = %d %q, want 502 saying why", code, res)
	}
	f.launcher.err, f.launcher.calls = nil, nil

	f.hub.SetRoster("claude", nil)
	if code := f.post("/api/archive/bg/resume", `{"prompt":"`+strings.Repeat("x", startLimit+1)+`"}`); code != 400 {
		t.Errorf("reopening with a prompt too long = %d, want 400", code)
	}
	if code := f.post("/api/archive/bg/resume", `{"prompt":"  carry on  "}`); code != 202 {
		t.Fatalf("reopen = %d", code)
	}
	if code := f.post("/api/archive/term/resume", `{}`); code != 202 {
		t.Fatalf("reopen with nothing to say = %d", code)
	}
	if want := []string{"resume bg|" + dir + "|carry on", "resume term|" + dir + "|"}; !slices.Equal(f.launcher.calls, want) {
		t.Errorf("launcher was asked %q, want %q", f.launcher.calls, want)
	}
	f.launcher.err = errors.New("no such conversation")
	if code, res := f.do("POST", "/api/archive/bg/resume", `{}`, nil); code != 502 || !strings.Contains(res, "no such conversation") {
		t.Errorf("a reopen claude refused = %d %q, want 502 saying why", code, res)
	}

	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	f.launcher.err, f.launcher.calls = nil, nil
	if code := f.post("/api/archive/bg/resume", `{}`); code != 400 || len(f.launcher.calls) != 0 {
		t.Errorf("reopening where the directory is gone = %d after %q, want 400 and no launch", code, f.launcher.calls)
	}
}

func TestModelURL(t *testing.T) {
	tests := []struct {
		in, want string
		ok       bool
	}{
		{"", "", true},
		{"  http://localhost:1234  ", "http://localhost:1234", true},
		{"http://localhost:11434/v1", "http://localhost:11434", true},
		{"http://localhost:11434/v1/", "http://localhost:11434", true},
		{"localhost:8080", "http://localhost:8080", true},
		{"https://models.example/openai/v1?key=1#top", "https://models.example/openai", true},
		{"http://127.0.0.1:1234/", "http://127.0.0.1:1234", true},
		{"ftp://localhost:21", "", false},
		{"http://", "", false},
		{"http://user:secret@localhost:1234", "", false},
		{"http://local host", "", false},
	}
	for _, tt := range tests {
		if got, ok := modelURL(tt.in); got != tt.want || ok != tt.ok {
			t.Errorf("modelURL(%q) = %q, %v; want %q, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestChoosingTheLocalModel(t *testing.T) {
	f := newFixture(t)
	if code := f.post("/api/settings", `{"model":{"url":" localhost:11434/v1/ ","name":" qwen "}}`); code != 204 {
		t.Fatalf("settings = %d, want 204", code)
	}
	if got, want := f.state().Settings.Model, (hub.ModelSettings{URL: "http://localhost:11434", Name: "qwen"}); got != want {
		t.Errorf("model settings = %+v, want %+v", got, want)
	}
	if !f.state().Settings.AnswerPermissions {
		t.Error("choosing the model changed where permission prompts are answered")
	}
	if code, body := f.do("POST", "/api/settings", `{"model":{"url":"file:///etc/passwd"}}`, nil); code != 400 || !strings.Contains(body, "http") {
		t.Errorf("an address that is not a server's = %d %q, want 400 saying what it must be", code, body)
	}
	if got := f.state().Settings.Model.URL; got != "http://localhost:11434" {
		t.Errorf("address = %q after one that was refused, want it left alone", got)
	}
	if code := f.post("/api/settings", `{"model":{"off":true}}`); code != 204 || !f.state().Settings.Model.Off || f.state().Settings.Model.URL == "" {
		t.Errorf("turning the model off = %d, leaving %+v; want it off and its address kept", code, f.state().Settings.Model)
	}
}

func TestCheckingTheLocalModel(t *testing.T) {
	f := newFixture(t)
	f.prober.says = hub.ModelCheck{URL: "http://localhost:1234", Reached: true, Models: []string{"warm"}, Model: "warm", Answered: true, Millis: 12}
	code, body := f.do("POST", "/api/model/check", `{}`, nil)
	var got hub.ModelCheck
	if err := json.Unmarshal([]byte(body), &got); code != 200 || err != nil || !got.Answered || got.Model != "warm" || got.Millis != 12 {
		t.Errorf("check = %d %s, want how the try went", code, body)
	}
	if len(f.prober.tried) != 1 || f.prober.tried[0] != nil {
		t.Errorf("tried %v, want what is in use", f.prober.tried)
	}

	if code, _ := f.do("POST", "/api/model/check", `{"model":{"url":"localhost:9/v1","name":" other "}}`, nil); code != 200 {
		t.Fatalf("check of another server = %d, want 200", code)
	}
	if m := f.prober.tried[len(f.prober.tried)-1]; m == nil || *m != (hub.ModelSettings{URL: "http://localhost:9", Name: "other"}) {
		t.Errorf("tried %+v, want the server and model asked about", m)
	}
	if got := f.state().Settings.Model; got != (hub.ModelSettings{}) {
		t.Errorf("model settings = %+v after a check, want them untouched", got)
	}

	if code, _ := f.do("POST", "/api/model/check", `{"model":{"url":"ftp://x"}}`, nil); code != 400 || len(f.prober.tried) != 2 {
		t.Errorf("check of an address that is not a server's = %d after %d tries, want 400 and nothing tried", code, len(f.prober.tried))
	}
}

func TestASessionsSubagentsCanBeRead(t *testing.T) {
	f := newFixture(t)
	f.record.subagents = map[string][]hub.Subagent{"s1": {{ID: "a1", Type: "Explore", Description: "Find the callers", Done: true, Tools: 3}}}
	f.record.reports = map[string]hub.SubagentReport{"s1/a1": {Prompt: "find them", Report: "there are two", Handed: true}}
	f.connect("s1")
	f.post("/api/bridge/event", `{"sessionId":"s1","type":"turn.start","text":"who calls this"}`)

	report := func(path string) (int, hub.SubagentReport) {
		code, body := f.do("GET", path, "", nil)
		var got hub.SubagentReport
		json.Unmarshal([]byte(body), &got)
		return code, got
	}
	want := hub.SubagentReport{Prompt: "find them", Report: "there are two", Handed: true}
	if code, got := report("/api/sessions/s1/subagents/a1"); code != 200 || got != want {
		t.Errorf("a subagent of an open session = %d %+v, want %+v", code, got, want)
	}
	if code, _ := report("/api/sessions/s1/subagents/a2"); code != 404 {
		t.Errorf("a subagent the session did not start = %d, want 404", code)
	}
	if code, _ := report("/api/sessions/s9/subagents/a1"); code != 404 {
		t.Errorf("a subagent of a session Slorp has not heard of = %d, want 404", code)
	}

	f.hub.SetRoster("claude", nil)
	if code, got := report("/api/sessions/s1/subagents/a1"); code != 200 || got != want {
		t.Errorf("a subagent of a closed session = %d %+v, want %+v", code, got, want)
	}
	code, body := f.do("GET", "/api/archive/s1", "", nil)
	var closed hub.Archived
	if err := json.Unmarshal([]byte(body), &closed); code != 200 || err != nil || len(closed.Subagents) != 1 || closed.Subagents[0].ID != "a1" || closed.Subagents[0].Tools != 3 {
		t.Errorf("reading the closed session back = %d %s, want its subagent with it", code, body)
	}
	if listed := f.state().Archive; len(listed) != 1 || listed[0].Subagents != nil {
		t.Errorf("listed = %+v, want it without its subagents", listed)
	}
}

func TestABridgeReportsItsSubagents(t *testing.T) {
	f := newFixture(t)
	f.connect("s1")
	subagents := func() []hub.Subagent { return f.state().Sessions[0].Subagents }
	long := strings.Repeat("é", doingLimit+50)
	samples := strings.TrimSuffix(strings.Repeat("7,", samplesKept+4), ",")
	f.post("/api/bridge/poll", `{"sessionId":"s1","agents":[
		{"id":"a1","type":"Explore","description":"Find the callers","status":"running","doing":"  `+long+`  ","parentId":"a0","model":"claude-haiku-5-5","startTime":1700000000000,"tokenSamples":[1,2,`+samples+`],"costUsd":0.25},
		{"id":"","status":"running"}]}`)
	got := subagents()
	if len(got) != 1 {
		t.Fatalf("subagents = %+v, want the one that has an id", got)
	}
	a := got[0]
	if a.ID != "a1" || a.Type != "Explore" || a.Description != "Find the callers" || a.Status != "running" || a.ParentID != "a0" || a.Model != "claude-haiku-5-5" || a.CostUSD != 0.25 || a.StartedAt.UnixMilli() != 1700000000000 {
		t.Errorf("subagent = %+v, want what the bridge said of it", a)
	}
	if len([]rune(a.Doing)) != doingLimit || !strings.HasPrefix(a.Doing, "é") || len(a.Samples) != samplesKept || a.Samples[0] != 7 {
		t.Errorf("kept %d runes of what it is doing and %d readings from %d; want %d and the latest %d", len([]rune(a.Doing)), len(a.Samples), a.Samples[0], doingLimit, samplesKept)
	}

	f.post("/api/bridge/poll", `{"sessionId":"s1"}`)
	if got := subagents(); len(got) != 1 {
		t.Errorf("subagents = %+v after a bridge that says nothing of them, want them kept", got)
	}
	f.post("/api/bridge/poll", `{"sessionId":"s1","agents":[]}`)
	if got := subagents(); len(got) != 0 {
		t.Errorf("subagents = %+v after a bridge that says it has none, want none", got)
	}

	many := strings.TrimSuffix(strings.Repeat(`{"id":"x","status":"running"},`, subagentsHeard+5), ",")
	f.post("/api/bridge/poll", `{"sessionId":"s1","agents":[`+many+`]}`)
	if got := subagents(); len(got) != subagentsHeard {
		t.Errorf("%d subagents kept, want no more than %d from one bridge", len(got), subagentsHeard)
	}

	f.post("/api/bridge/permission", `{"sessionId":"s1","tool":"Bash","summary":"rm -rf build","input":"rm -rf build","canAnswer":true,"agentId":"a1"}`)
	if p := f.state().Sessions[0].Pending; p == nil || p.AgentID != "a1" {
		t.Errorf("pending = %+v, want it to name the subagent that asked", p)
	}
}
