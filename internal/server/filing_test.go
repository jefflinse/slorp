package server

import (
	"errors"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/jefflinse/slorp/internal/hub"
	"github.com/jefflinse/slorp/internal/integrations"
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
