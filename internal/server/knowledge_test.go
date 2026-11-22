package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jefflinse/slorp/internal/hub"
	"github.com/jefflinse/slorp/internal/integrations"
	"github.com/jefflinse/slorp/internal/kb"
	"github.com/jefflinse/slorp/internal/store"
	"github.com/jefflinse/slorp/internal/testdb"
)

func TestARejectedProfileEditLeavesTheProfileAsItWas(t *testing.T) {
	f := newFixture(t)
	code, body := f.do(http.MethodPost, "/api/profiles", `{"name":"Local","engine":"local","temperature":0.5}`, nil)
	if code != http.StatusOK {
		t.Fatalf("adding a profile = %d %s", code, body)
	}
	before := f.state().Knowledge.Profiles

	if code := f.post("/api/profiles/"+before[0].ID, `{"temperature":7}`); code != http.StatusBadRequest {
		t.Fatalf("a temperature of 7 = %d, want 400", code)
	}
	for name, profiles := range map[string][]kb.Profile{"kept": f.kb.Knowledge().Profiles, "reopened": testdb.KB(t, f.store).Knowledge().Profiles} {
		if got := profiles[0].Temperature; got == nil || *got != 0.5 {
			t.Errorf("%s temperature after a rejected edit = %v, want 0.5", name, deref(got))
		}
	}
}

func deref(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}

// unsaved keeps the knowledge base in a store, but fails every write while
// fail is set.
// puts counts the writes asked for.
type unsaved struct {
	*store.Store
	fail bool
	puts int
}

func (p *unsaved) Put(kind, id string, v any) error {
	p.puts++
	if p.fail {
		return errors.New("disk full")
	}
	return p.Store.Put(kind, id, v)
}

func TestAKnowledgeEditThatCannotBeSavedIsTheServersFault(t *testing.T) {
	st := testdb.Open(t)
	p := &unsaved{Store: st}
	k, err := kb.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(hub.New(k, st), k, integrations.NewManager(func() map[string]bool { return nil }), &asked{}, nil, nil, nil, nil, nil, nil, nil, Page(nil, "")))
	t.Cleanup(srv.Close)
	post := func(path, body string) int {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}

	p.fail = true
	for path, body := range map[string]string{
		"/api/efforts":  `{"name":"Widgets"}`,
		"/api/profiles": `{"name":"Local"}`,
		"/api/handoffs": `{"name":"Pass","text":"{{response}}"}`,
	} {
		if code := post(path, body); code != http.StatusInternalServerError {
			t.Errorf("POST %s that cannot be saved = %d, want 500", path, code)
		}
	}
	for path, body := range map[string]string{
		"/api/efforts":  `{"name":""}`,
		"/api/profiles": `{"name":""}`,
		"/api/handoffs": `{"name":"Pass"}`,
	} {
		if code := post(path, body); code != http.StatusBadRequest {
			t.Errorf("POST %s that is invalid = %d, want 400", path, code)
		}
	}
	if len(k.Efforts()) != 0 || len(k.Knowledge().Profiles) != 0 || len(k.Knowledge().Handoffs) != 0 {
		t.Errorf("the knowledge base kept what could not be saved: %+v", k.Knowledge())
	}
}

func TestAStartedSessionIsFiledWholeOrNotAtAll(t *testing.T) {
	st := testdb.Open(t)
	p := &unsaved{Store: st}
	k, err := kb.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	e, err := k.Create("Widgets", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := k.Add(kb.SectionRoles, kb.Entry{Name: "Reviewer"}); err != nil {
		t.Fatal(err)
	}
	role := k.Knowledge().Roles[0].ID
	launch := &launcher{}
	srv := httptest.NewServer(New(hub.New(k, st), k, integrations.NewManager(func() map[string]bool { return nil }), &asked{}, nil, map[string]Launcher{"claude": launch}, map[string]PluginManager{"claude": launch}, nil, nil, nil, nil, Page(nil, "")))
	t.Cleanup(srv.Close)
	start := func(id string) int {
		t.Helper()
		launch.id = id
		body := `{"dir":` + strconv.Quote(t.TempDir()) + `,"prompt":"review it","effort":"` + e.ID + `","role":"` + role + `"}`
		res, err := srv.Client().Post(srv.URL+"/api/sessions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	filed := func(k *kb.Store, id string) (effort, role bool) {
		_, via, ok := k.Resolve(id, "/")
		_, given := k.RoleOf(id)
		return ok && via == kb.ViaUser, given
	}

	p.fail, p.puts = true, 0
	if code := start("ses_unsaved"); code != http.StatusInternalServerError || p.puts != 1 {
		t.Errorf("a start whose filing cannot be saved = %d after %d writes, want 500 after one", code, p.puts)
	}
	for name, k := range map[string]*kb.Store{"kept": k, "reopened": testdb.KB(t, st)} {
		if effort, role := filed(k, "ses_unsaved"); effort || role {
			t.Errorf("%s: a session whose filing was not saved has effort %v and role %v, want neither", name, effort, role)
		}
	}

	p.fail, p.puts = false, 0
	if code := start("ses_saved"); code != http.StatusAccepted || p.puts != 1 {
		t.Errorf("a start = %d after %d writes, want 202 after one", code, p.puts)
	}
	for name, k := range map[string]*kb.Store{"kept": k, "reopened": testdb.KB(t, st)} {
		if effort, role := filed(k, "ses_saved"); !effort || !role {
			t.Errorf("%s: a started session has effort %v and role %v, want both", name, effort, role)
		}
	}
}

func TestAnEffortEditIsMadeWholeOrNotAtAll(t *testing.T) {
	st := testdb.Open(t)
	p := &unsaved{Store: st}
	k, err := kb.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	e, err := k.Create("Widgets", "")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(hub.New(k, st), k, integrations.NewManager(func() map[string]bool { return nil }), &asked{}, nil, nil, nil, nil, nil, nil, nil, Page(nil, "")))
	t.Cleanup(srv.Close)
	edit := func() int {
		t.Helper()
		res, err := srv.Client().Post(srv.URL+"/api/efforts/"+e.ID, "application/json", strings.NewReader(`{"name":"Gadgets","shelved":true}`))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	stands := func(k *kb.Store) kb.Effort {
		t.Helper()
		for _, got := range k.Efforts() {
			if got.ID == e.ID {
				return got
			}
		}
		t.Fatalf("effort %s is gone", e.ID)
		return kb.Effort{}
	}
	revision := k.Revision()

	p.fail, p.puts = true, 0
	if code := edit(); code != http.StatusInternalServerError || p.puts != 1 {
		t.Errorf("renaming and shelving that cannot be saved = %d after %d writes, want 500 after one", code, p.puts)
	}
	for name, k := range map[string]*kb.Store{"kept": k, "reopened": testdb.KB(t, st)} {
		if got := stands(k); got.Name != "Widgets" || got.Shelved || k.Revision() != revision {
			t.Errorf("%s effort after a failed edit: %+v at revision %d; want it as it was at %d", name, got, k.Revision(), revision)
		}
	}

	p.fail, p.puts = false, 0
	if code := edit(); code != http.StatusNoContent || p.puts != 1 {
		t.Errorf("renaming and shelving = %d after %d writes, want 204 after one", code, p.puts)
	}
	for name, k := range map[string]*kb.Store{"kept": k, "reopened": testdb.KB(t, st)} {
		if got := stands(k); got.Name != "Gadgets" || !got.Shelved {
			t.Errorf("%s effort after renaming and shelving: %+v; want both", name, got)
		}
	}
}
