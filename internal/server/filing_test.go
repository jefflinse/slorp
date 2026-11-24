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
	"github.com/jefflinse/slorp/internal/kb"
	"github.com/jefflinse/slorp/internal/store"
	"github.com/jefflinse/slorp/internal/testdb"
)

// unsavedSessions keeps the hub in a store, failing every write of a
// session while fail is set.
type unsavedSessions struct {
	*store.Store
	fail bool
}

func (c *unsavedSessions) Put(kind, id string, v any) error {
	if kind == "session" && c.fail {
		return errors.New("disk full")
	}
	return c.Store.Put(kind, id, v)
}

func TestAFilingIsTakenOnlyOnceItsSessionIsKept(t *testing.T) {
	st := testdb.Open(t)
	k := testdb.KB(t, st)
	e, err := k.Create("Widgets", "")
	if err != nil {
		t.Fatal(err)
	}
	id := e.ID
	c := &unsavedSessions{Store: st}
	h := hub.New(k, c)
	srv := httptest.NewServer(New(h, k, integrations.NewManager(func() map[string]bool { return nil }), &asked{}, nil, nil, nil, nil, nil, nil, nil, Page(nil, "")))
	t.Cleanup(srv.Close)
	file := func() int {
		t.Helper()
		res, err := srv.Client().Post(srv.URL+"/api/sessions/a/effort", "application/json", strings.NewReader(`{"effort":"`+id+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		if err := res.Body.Close(); err != nil {
			t.Fatal(err)
		}
		return res.StatusCode
	}
	h.Link("a", "root")
	h.Link("b", "root")
	h.SetRoster("claude", []hub.Session{{ID: "a", Agent: "claude", Cwd: "/w"}})

	c.fail = true
	if code := file(); code != 500 {
		t.Errorf("filing a session that cannot be kept = %d, want 500", code)
	}
	if got, _, _ := k.Resolve("a", ""); got != "" {
		t.Errorf("a refused filing left a under %q", got)
	}
	c.fail = false
	if code := file(); code != 204 {
		t.Fatalf("filing a session once it can be kept = %d, want 204", code)
	}

	h = hub.New(k, st)
	h.SetRoster("claude", []hub.Session{{ID: "b", Agent: "claude", Cwd: "/w"}})
	sessions := h.Sessions()
	if at := slices.IndexFunc(sessions, func(s hub.Session) bool { return s.ID == "b" }); at < 0 || sessions[at].Effort != id {
		t.Errorf("after a restart the sessions are %+v, want b under %q, taken up from a", sessions, id)
	}
}

func TestAFilingOfASessionTakenUpIsRefused(t *testing.T) {
	st := testdb.Open(t)
	k := testdb.KB(t, st)
	e, err := k.Create("Widgets", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := k.Add(kb.SectionRoles, kb.Entry{Name: "Reviewer"}); err != nil {
		t.Fatal(err)
	}
	role := k.Knowledge().Roles[0].ID
	h := hub.New(k, st)
	srv := httptest.NewServer(New(h, k, integrations.NewManager(func() map[string]bool { return nil }), &asked{}, nil, nil, nil, nil, nil, nil, nil, Page(nil, "")))
	t.Cleanup(srv.Close)
	at := time.Now().Add(-time.Hour)
	h.Link("a", "root")
	h.Link("b", "root")
	h.SetRoster("claude", []hub.Session{{ID: "a", Agent: "claude", Cwd: "/w", StartedAt: at, LastPrompt: "add flanges", PromptAt: at}})
	h.SetRoster("claude", []hub.Session{{ID: "b", Agent: "claude", Cwd: "/w", StartedAt: at.Add(time.Minute)}})
	if archived := h.State().Archive; len(archived) != 0 {
		t.Fatalf("archive = %+v, want a taken up by b", archived)
	}

	for path, body := range map[string]string{"effort": `{"effort":"` + e.ID + `"}`, "role": `{"role":"` + role + `"}`} {
		res, err := srv.Client().Post(srv.URL+"/api/sessions/a/"+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if err := res.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != 404 {
			t.Errorf("a %s for a session taken up = %d, want 404", path, res.StatusCode)
		}
	}
	if got, _, _ := k.Resolve("a", ""); got != "" {
		t.Errorf("a refused filing left a under %q", got)
	}
	if _, ok := k.RoleOf("a"); ok {
		t.Error("a refused role was given to a")
	}
}

// unreadSessions keeps the hub in a store, failing every read of a session
// while fail is set.
type unreadSessions struct {
	*store.Store
	fail bool
}

func (c *unreadSessions) Get(kind, id string, v any) (bool, error) {
	if kind == "session" && c.fail {
		return false, errors.New("disk unreadable")
	}
	return c.Store.Get(kind, id, v)
}

func TestAFilingOfASessionThatCannotBeReadIsRefused(t *testing.T) {
	st := testdb.Open(t)
	k := testdb.KB(t, st)
	e, err := k.Create("Widgets", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := k.Add(kb.SectionRoles, kb.Entry{Name: "Reviewer"}); err != nil {
		t.Fatal(err)
	}
	role := k.Knowledge().Roles[0].ID
	c := &unreadSessions{Store: st, fail: true}
	h := hub.New(k, c)
	srv := httptest.NewServer(New(h, k, integrations.NewManager(func() map[string]bool { return nil }), &asked{}, nil, nil, nil, nil, nil, nil, nil, Page(nil, "")))
	t.Cleanup(srv.Close)
	edits := map[string]string{"effort": `{"effort":"` + e.ID + `"}`, "role": `{"role":"` + role + `"}`}
	file := func(path string) int {
		t.Helper()
		res, err := srv.Client().Post(srv.URL+"/api/sessions/a/"+path, "application/json", strings.NewReader(edits[path]))
		if err != nil {
			t.Fatal(err)
		}
		if err := res.Body.Close(); err != nil {
			t.Fatal(err)
		}
		return res.StatusCode
	}
	a := []hub.Session{{ID: "a", Agent: "claude", Cwd: "/w"}}
	h.SetRoster("claude", a)
	if !h.HeldOpen("a") {
		t.Fatal("a is not held while what is kept of it cannot be read")
	}
	for path := range edits {
		if code := file(path); code != 500 {
			t.Errorf("a %s for a session that cannot be read = %d, want 500", path, code)
		}
	}
	if got, _, _ := k.Resolve("a", ""); got != "" {
		t.Errorf("a refused filing left a under %q", got)
	}
	if _, ok := k.RoleOf("a"); ok {
		t.Error("a refused role was given to a")
	}

	c.fail = false
	h.SetRoster("claude", a)
	for path := range edits {
		if code := file(path); code != 204 {
			t.Errorf("a %s for a session once it can be read = %d, want 204", path, code)
		}
	}
	var kept struct {
		Agent string `json:"agent"`
	}
	if found, err := st.Get("session", "a", &kept); err != nil || !found {
		t.Errorf("a filed session is kept: %v, %v; want it saved", found, err)
	}
}

func TestAFilingOfAClosedSessionIsMadeOnceItIsKept(t *testing.T) {
	st := testdb.Open(t)
	k := testdb.KB(t, st)
	e, err := k.Create("Widgets", "")
	if err != nil {
		t.Fatal(err)
	}
	c := &unsavedSessions{Store: st, fail: true}
	h := hub.New(k, c)
	srv := httptest.NewServer(New(h, k, integrations.NewManager(func() map[string]bool { return nil }), &asked{}, nil, nil, nil, nil, nil, nil, nil, Page(nil, "")))
	t.Cleanup(srv.Close)
	at := time.Now().Add(-time.Hour)
	h.SetRoster("claude", []hub.Session{{ID: "a", Agent: "claude", Cwd: "/w", StartedAt: at, LastPrompt: "add flanges", PromptAt: at}})
	h.SetRoster("claude", nil)
	if archived := h.State().Archive; len(archived) != 1 {
		t.Fatalf("archive = %+v, want a", archived)
	}
	for _, fail := range []bool{true, false} {
		c.fail = fail
		res, err := srv.Client().Post(srv.URL+"/api/sessions/a/effort", "application/json", strings.NewReader(`{"effort":"`+e.ID+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		if err := res.Body.Close(); err != nil {
			t.Fatal(err)
		}
		filed, _, _ := k.Resolve("a", "")
		if want := map[bool]int{true: 500, false: 204}[fail]; res.StatusCode != want || (filed != "") == fail {
			t.Errorf("filing a closed session with saving failing %v = %d, filed under %q; want %d", fail, res.StatusCode, filed, want)
		}
	}
	var kept struct {
		Agent string `json:"agent"`
	}
	if found, err := st.Get("session", "a", &kept); err != nil || !found {
		t.Errorf("a filed closed session is kept: %v, %v; want it saved", found, err)
	}
	for when, h := range map[string]*hub.Hub{"at once": h, "after a restart": hub.New(k, st)} {
		if a, ok := h.Archive("a"); !ok || a.Effort != e.ID {
			t.Errorf("%s the closed session is %+v, %v; want it under %q", when, a, ok, e.ID)
		}
		if past, ok := h.Past(e.ID); !ok || past.ClosedCount != 1 {
			t.Errorf("%s the effort's history holds %d closed sessions, %v; want a", when, past.ClosedCount, ok)
		}
	}
}
