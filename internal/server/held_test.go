package server

import (
	"errors"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/hub"
	"github.com/jefflinse/slorp/internal/integrations"
	"github.com/jefflinse/slorp/internal/store"
	"github.com/jefflinse/slorp/internal/testdb"
)

// unreadSession keeps the hub in a store, failing each read of the session
// unreadable names.
type unreadSession struct {
	*store.Store
	unreadable string
}

func (c *unreadSession) Get(kind, id string, v any) (bool, error) {
	if kind == "session" && id == c.unreadable {
		return false, errors.New("disk unreadable")
	}
	return c.Store.Get(kind, id, v)
}

func TestASubagentIsReadUnderTheSessionThatTookItsSessionUp(t *testing.T) {
	f := newFixture(t)
	f.record.subagents = map[string][]hub.Subagent{"old": {{ID: "child", Status: "completed"}}}
	f.record.reports = map[string]hub.SubagentReport{"old/child": {Report: "flanges fitted"}}
	f.hub.SetRoster("claude", []hub.Session{{ID: "old", Agent: "claude", Cwd: "/w", LastPrompt: "add flanges", PromptAt: time.Now().Add(-time.Hour)}})
	f.hub.SetRoster("claude", nil)
	f.hub.Link("new", "old")
	f.hub.SetRoster("claude", []hub.Session{{ID: "new", Agent: "claude", Cwd: "/w"}})
	if code, body := f.do("GET", "/api/sessions/new/subagents/child", "", nil); code != 200 || !strings.Contains(body, "flanges fitted") {
		t.Errorf("the report under the session that took its session up = %d %s, want it", code, body)
	}
	f.hub.SetRoster("claude", nil)
	if code, body := f.do("GET", "/api/archive/new", "", nil); code != 200 || !strings.Contains(body, `"id":"child"`) {
		t.Errorf("closed, the session that took it up = %d %s, want its subagent listed", code, body)
	}
}

func TestASessionReportedOpenIsNotOpenedInAnotherTerminal(t *testing.T) {
	st := testdb.Open(t)
	k := testdb.KB(t, st)
	c := &unreadSession{Store: st}
	h := hub.New(k, c)
	terms := &terminals{}
	launchers := map[string]Launcher{"claude": &launcher{}, "opencode": &managedLauncher{h: h}}
	srv := httptest.NewServer(New(h, k, integrations.NewManager(func() map[string]bool { return nil }), &asked{}, nil, launchers, nil, nil, nil, terms, nil, Page(nil, "")))
	t.Cleanup(srv.Close)
	open := func(id string) int {
		t.Helper()
		res, err := srv.Client().Post(srv.URL+"/api/sessions/"+id+"/terminal", "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if err := res.Body.Close(); err != nil {
			t.Fatal(err)
		}
		return res.StatusCode
	}
	dir := t.TempDir()
	bg := hub.Session{ID: "bg", Agent: "claude", Kind: "background", Job: "3e14f875", Cwd: dir}
	h.SetRoster("claude", []hub.Session{{ID: "cl", Agent: "claude", Cwd: dir, LastPrompt: "in a terminal", PromptAt: time.Now()}, bg})
	h.SetRoster("claude", []hub.Session{bg})
	h.Apply("opencode", "oc", func(s *hub.Session) { s.Cwd, s.LastPrompt, s.PromptAt = dir, "in a terminal", time.Now() })
	h.Retire("opencode", -time.Second)
	c.unreadable = "cl"
	h.SetRoster("claude", []hub.Session{{ID: "cl", Agent: "claude", Cwd: dir}, bg})
	c.unreadable = "oc"
	h.Apply("opencode", "oc", nil)
	c.unreadable = ""

	for _, id := range []string{"cl", "oc"} {
		if code := open(id); code != 409 {
			t.Errorf("opening %s, reported open elsewhere, in a terminal = %d, want 409", id, code)
		}
	}
	if len(terms.opened) != 0 {
		t.Errorf("opened %q for sessions reported open elsewhere, want nothing", terms.opened)
	}
	if code := open("bg"); code != 204 || len(terms.opened) != 1 || !strings.Contains(terms.opened[0][1], "attach '3e14f875'") {
		t.Errorf("attaching to a background session = %d having opened %q, want it attached", code, terms.opened)
	}

	h.SetRoster("claude", []hub.Session{bg})
	h.Retire("opencode", -time.Second)
	for _, id := range []string{"cl", "oc"} {
		if code := open(id); code != 204 {
			t.Errorf("opening %s once it closed = %d, want 204", id, code)
		}
	}
	if len(terms.opened) != 3 || !strings.Contains(terms.opened[1][1], "--resume 'cl'") || !strings.Contains(terms.opened[2][1], "--session 'oc'") {
		t.Errorf("opened %q, want each resumed once it closed", terms.opened)
	}
}

func TestASessionHeldOpenAfterARestartIsNotReopened(t *testing.T) {
	st := testdb.Open(t)
	k := testdb.KB(t, st)
	hub.New(k, st).Apply("opencode", "ses_open", func(s *hub.Session) { s.Cwd, s.LastPrompt, s.PromptAt = t.TempDir(), "in a terminal", time.Now() })

	c := &unreadSession{Store: st, unreadable: "ses_open"}
	h := hub.New(k, c)
	h.Apply("opencode", "ses_open", nil)
	c.unreadable = ""
	if _, archived := h.Archive("ses_open"); archived || !h.HeldOpen("ses_open") {
		t.Fatalf("ses_open is archived %v and held %v, want held and not archived", archived, h.HeldOpen("ses_open"))
	}
	l := &launcher{}
	srv := httptest.NewServer(New(h, k, integrations.NewManager(func() map[string]bool { return nil }), &asked{}, nil, map[string]Launcher{"opencode": l}, nil, nil, nil, nil, nil, Page(nil, "")))
	t.Cleanup(srv.Close)
	post := func(id string) int {
		t.Helper()
		res, err := srv.Client().Post(srv.URL+"/api/archive/"+id+"/resume", "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if err := res.Body.Close(); err != nil {
			t.Fatal(err)
		}
		return res.StatusCode
	}
	if code := post("ses_open"); code != 409 {
		t.Errorf("reopening a session held open after a restart = %d, want 409", code)
	}
	if code := post("ses_unknown"); code != 404 {
		t.Errorf("reopening a session never known = %d, want 404", code)
	}
	if len(l.calls) != 0 {
		t.Errorf("the launcher was asked %q, want nothing", l.calls)
	}
}

func TestASessionReportedOpenIsNotReopenedFromTheArchive(t *testing.T) {
	st := testdb.Open(t)
	k := testdb.KB(t, st)
	c := &unreadSession{Store: st}
	h := hub.New(k, c)
	l := &launcher{}
	srv := httptest.NewServer(New(h, k, integrations.NewManager(func() map[string]bool { return nil }), &asked{}, nil, map[string]Launcher{"opencode": l}, nil, nil, nil, nil, nil, Page(nil, "")))
	t.Cleanup(srv.Close)
	post := func(path, body string) int {
		t.Helper()
		res, err := srv.Client().Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if err := res.Body.Close(); err != nil {
			t.Fatal(err)
		}
		return res.StatusCode
	}
	dir := t.TempDir()
	h.Apply("opencode", "ses_x", func(s *hub.Session) { s.Cwd, s.LastPrompt, s.PromptAt = dir, "in a terminal", time.Now() })
	h.Retire("opencode", -time.Second)
	if _, ok := h.Archive("ses_x"); !ok {
		t.Fatal("ses_x is not archived")
	}

	c.unreadable = "ses_x"
	if code := post("/api/bridge/event", `{"agent":"opencode","sessionId":"ses_x","type":"turn.start","text":"again"}`); code != 204 {
		t.Fatalf("the terminal's report = %d, want 204", code)
	}
	c.unreadable = ""
	for _, body := range []string{`{}`, `{"requestId":"again"}`} {
		if code := post("/api/archive/ses_x/resume", body); code != 409 {
			t.Errorf("reopening a session reported open elsewhere with %s = %d, want 409", body, code)
		}
	}
	if len(l.calls) != 0 {
		t.Errorf("reopening a session reported open elsewhere asked the launcher %q, want nothing", l.calls)
	}

	h.Retire("opencode", -time.Second)
	if code := post("/api/archive/ses_x/resume", `{"requestId":"again"}`); code != 202 {
		t.Errorf("reopening once its terminal fell silent = %d, want 202", code)
	}
	want := []string{"resume ses_x|" + dir + "|"}
	if !slices.Equal(l.calls, want) {
		t.Errorf("the launcher was asked %q, want %q", l.calls, want)
	}

	c.unreadable = "ses_x"
	post("/api/bridge/event", `{"agent":"opencode","sessionId":"ses_x","type":"turn.start","text":"once more"}`)
	c.unreadable = ""
	if code := post("/api/archive/ses_x/resume", `{"requestId":"again"}`); code != 202 || !slices.Equal(l.calls, want) {
		t.Errorf("repeating a reopen that was done, while held open elsewhere, = %d after %q; want it answered as before, without asking again", code, l.calls)
	}
}
