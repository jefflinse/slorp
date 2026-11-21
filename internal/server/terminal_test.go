package server

import (
	"errors"
	"io"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/hub"
	"github.com/jefflinse/slorp/internal/integrations"
	"github.com/jefflinse/slorp/internal/testdb"
)

func TestOpeningASessionInATerminal(t *testing.T) {
	f := newFixture(t)
	f.hub.SetRoster("claude", []hub.Session{
		{ID: "bg", Agent: "claude", Kind: "background", Job: "3e14f875", Cwd: "/work/it's"},
		{ID: "mine", Agent: "claude", Kind: "interactive", Cwd: "/work/a"},
		{ID: "done", Agent: "claude", Kind: "interactive", Cwd: "/work/b", LastPrompt: "do it", PromptAt: time.Now()},
	})
	open := func(id string) (int, string) { return f.do("POST", "/api/sessions/"+id+"/terminal", `{}`, nil) }

	_, body := f.do("GET", "/api/sessions", "", nil)
	for _, field := range []string{`"platform":"` + runtime.GOOS + `"`, `"terminal":{"apps":[{"id":"one","name":"One"},{"id":"two","name":"Two"}],"default":"two"}`} {
		if !strings.Contains(body, field) {
			t.Errorf("state lacks %s, which the page reads: %s", field, body)
		}
	}

	if code, body := open("bg"); code != 204 {
		t.Fatalf("opening a background session = %d %s", code, body)
	}
	attach := `cd -- '/work/it'"'"'s' && 'claude' attach '3e14f875'`
	if want := [][2]string{{"two", attach}}; !reflect.DeepEqual(f.terminals.opened, want) {
		t.Errorf("opened %q, want %q: the terminal Slorp was started from, attached to the session's job in its directory", f.terminals.opened, want)
	}

	if code := f.post("/api/settings", `{"terminal":"one"}`); code != 204 || f.hub.Settings().Terminal != "one" {
		t.Fatalf("choosing a terminal = %d, settings %+v", code, f.hub.Settings())
	}
	if code := f.post("/api/settings", `{"terminal":"three"}`); code != 400 || f.hub.Settings().Terminal != "one" {
		t.Errorf("choosing a terminal that is not installed = %d, settings %+v; want it refused", code, f.hub.Settings())
	}
	f.hub.SetRoster("claude", []hub.Session{{ID: "mine", Agent: "claude", Kind: "interactive", Cwd: "/work/a"}})
	f.terminals.opened = nil
	if code, body := open("done"); code != 204 {
		t.Fatalf("opening a closed session = %d %s", code, body)
	}
	if want := [][2]string{{"one", `cd -- '/work/b' && 'claude' --resume 'done'`}}; !reflect.DeepEqual(f.terminals.opened, want) {
		t.Errorf("opened %q, want %q: the terminal chosen, resuming the session", f.terminals.opened, want)
	}

	f.terminals.opened = nil
	if code, _ := open("mine"); code != 409 {
		t.Errorf("opening a session that is in a terminal of its own = %d, want 409", code)
	}
	if code, _ := open("nope"); code != 404 {
		t.Errorf("opening a session there is none of = %d, want 404", code)
	}
	if code, _ := f.do("POST", "/api/sessions/done/terminal", `{`, nil); code != 400 {
		t.Errorf("a malformed request = %d, want 400", code)
	}
	if len(f.terminals.opened) != 0 {
		t.Errorf("opened %q for requests that were refused", f.terminals.opened)
	}
	f.terminals.err = errors.New("not allowed")
	if code, body := open("done"); code != 502 || !strings.Contains(body, "not allowed") {
		t.Errorf("a terminal that would not open = %d %s, want 502 saying why", code, body)
	}
}

// attaching is a launcher whose sessions running in the background can be
// opened in a terminal, with the command it is told to give.
type attaching struct {
	launcher
	command string
	ok      bool
	err     error
	asked   []string
}

func (a *attaching) AttachCommand(job string) (string, bool, error) {
	a.asked = append(a.asked, job)
	return a.command, a.ok, a.err
}

func TestOpeningAnAgentsOwnBackgroundSessionInATerminal(t *testing.T) {
	st := testdb.Open(t)
	k := testdb.KB(t, st)
	h := hub.New(k, st)
	h.SetRoster("opencode", []hub.Session{{ID: "run", Agent: "opencode", Job: "j9", Cwd: "/work/a"}, {ID: "mine", Agent: "opencode", Cwd: "/work/a"}})
	l, terms := &attaching{command: "sh '/private/attach'", ok: true}, &terminals{}
	srv := httptest.NewServer(New(h, k, integrations.NewManager(func() map[string]bool { return nil }), &asked{}, nil, map[string]Launcher{"opencode": l}, nil, nil, nil, terms, Page(nil, "")))
	t.Cleanup(srv.Close)
	open := func(id string) int {
		res, err := srv.Client().Post(srv.URL+"/api/sessions/"+id+"/terminal", "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}

	res, err := srv.Client().Get(srv.URL + "/api/sessions")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(body), `"resumeCommand":false,"attach":true`) {
		t.Errorf("state does not say this agent's background sessions can be attached to: %s", body)
	}

	if code := open("run"); code != 204 {
		t.Fatalf("opening a session the launcher runs = %d", code)
	}
	if want := [][2]string{{"two", "sh '/private/attach'"}}; !reflect.DeepEqual(terms.opened, want) || !reflect.DeepEqual(l.asked, []string{"j9"}) {
		t.Errorf("opened %q having asked for %q, want %q for job j9", terms.opened, l.asked, want)
	}
	if code := open("mine"); code != 409 || len(l.asked) != 1 {
		t.Errorf("opening a session in a terminal of its own = %d having asked for %q, want 409 without asking", code, l.asked)
	}
	l.ok = false
	if code := open("run"); code != 409 {
		t.Errorf("opening a session the launcher no longer runs = %d, want 409", code)
	}
	l.err = errors.New("disk full")
	if code := open("run"); code != 500 {
		t.Errorf("a command that could not be prepared = %d, want 500", code)
	}
	if len(terms.opened) != 1 {
		t.Errorf("opened %q, want nothing more after the first", terms.opened)
	}
}

func TestNoTerminalToOpenASessionIn(t *testing.T) {
	st := testdb.Open(t)
	k := testdb.KB(t, st)
	h := hub.New(k, st)
	h.SetRoster("claude", []hub.Session{{ID: "bg", Agent: "claude", Kind: "background", Job: "3e14f875", Cwd: "/work/a"}})
	srv := httptest.NewServer(New(h, k, integrations.NewManager(func() map[string]bool { return nil }), &asked{}, nil, map[string]Launcher{"claude": &launcher{}}, nil, nil, nil, nil, Page(nil, "")))
	t.Cleanup(srv.Close)
	res, err := srv.Client().Post(srv.URL+"/api/sessions/bg/terminal", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 400 {
		t.Errorf("opening a session where Slorp knows no terminal = %d, want 400", res.StatusCode)
	}
}
