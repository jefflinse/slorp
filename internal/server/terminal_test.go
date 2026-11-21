package server

import (
	"errors"
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
