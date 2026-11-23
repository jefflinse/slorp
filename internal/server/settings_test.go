package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/hub"
	"github.com/jefflinse/slorp/internal/integrations"
	"github.com/jefflinse/slorp/internal/store"
	"github.com/jefflinse/slorp/internal/testdb"
)

// unsavedSettings keeps the hub in a store, but fails every write of the
// settings while fail is set.
type unsavedSettings struct {
	*store.Store
	fail bool
}

func (c *unsavedSettings) Put(kind, id string, v any) error {
	if c.fail && kind == "setting" {
		return errors.New("disk full")
	}
	return c.Store.Put(kind, id, v)
}

func TestASettingsChangeThatCannotBeSavedIsNotMade(t *testing.T) {
	st := testdb.Open(t)
	k := testdb.KB(t, st)
	c := &unsavedSettings{Store: st}
	h := hub.New(k, c)
	tools := integrations.NewManager(func() map[string]bool { return h.Settings().Integrations }, integrations.Integration{
		ID: "fake", Name: "Fake", About: "A stand-in.",
		Check: func(context.Context) (bool, string) { return true, "Ready." },
		Scope: func(string) (string, bool) { return "", true },
	})
	srv := httptest.NewServer(New(h, k, tools, &asked{}, nil, nil, nil, nil, nil, nil, nil, Page(nil, "")))
	t.Cleanup(srv.Close)
	post := func(path, body string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
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
	if code := post("/api/settings", `{"prompts":{"rewrite":"always"}}`); code/100 != 2 {
		t.Fatalf("save = %d, want it kept", code)
	}

	c.fail = true
	for path, body := range map[string]string{
		"/api/settings":          `{"answerPermissions":false,"prompts":{"rewrite":"always","tasks":{"note":"Say little."}}}`,
		"/api/model/rewrite":     `{"tasks":["describe"]}`,
		"/api/integrations/fake": `{"enabled":true}`,
	} {
		if code := post(path, body); code != http.StatusInternalServerError {
			t.Errorf("POST %s that cannot be saved = %d, want 500", path, code)
		}
	}
	if code := post("/api/settings", `{"model":{"url":"ftp://nowhere"}}`); code != http.StatusBadRequest {
		t.Errorf("an invalid save while saving fails = %d, want 400", code)
	}
	check := func(name string, s hub.Settings) {
		t.Helper()
		if !s.AnswerPermissions || s.Prompts.Tasks[hub.ActNote] != "" || s.Rewritten[hub.ActNote] != 0 || s.Rewritten[hub.ActDescribe] != 0 || s.Integrations["fake"] || s.IntegrationsRevision != 0 {
			t.Errorf("%s settings hold what could not be saved: %+v", name, s)
		}
	}
	check("after the failed saves", h.Settings())

	// The next save that succeeds keeps only what it changes.
	c.fail = false
	if code := post("/api/settings", `{"model":{"name":"other-model"}}`); code/100 != 2 {
		t.Fatalf("a later save = %d, want it kept", code)
	}
	for name, s := range map[string]hub.Settings{"after a later save": h.Settings(), "reopened": hub.New(k, st).Settings()} {
		check(name, s)
		if s.Model.Name != "other-model" {
			t.Errorf("%s settings have model %q, want the later save's", name, s.Model.Name)
		}
	}
}

// heldBody holds the first read of a request's body until release is
// closed, closing reading once it is held.
type heldBody struct {
	io.ReadCloser
	once             sync.Once
	reading, release chan struct{}
}

func (b *heldBody) Read(p []byte) (int, error) {
	b.once.Do(func() {
		close(b.reading)
		<-b.release
	})
	return b.ReadCloser.Read(p)
}

func TestPartialSettingsSavedTogetherBothStand(t *testing.T) {
	f := newFixture(t)
	reading, release := make(chan struct{}), make(chan struct{})
	var released, held sync.Once
	next := f.srv.Config.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/settings" {
			held.Do(func() { r.Body = &heldBody{ReadCloser: r.Body, reading: reading, release: release} })
		}
		next.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { released.Do(func() { close(release) }) })
	save := func(body string) <-chan int {
		done := make(chan int, 1)
		go func() {
			req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/settings", strings.NewReader(body))
			if err != nil {
				done <- 0
				return
			}
			req.Header.Set("Content-Type", "application/json")
			res, err := srv.Client().Do(req)
			if err != nil {
				done <- 0
				return
			}
			res.Body.Close()
			done <- res.StatusCode
		}()
		return done
	}
	answered := func(what string, done <-chan int) {
		t.Helper()
		select {
		case code := <-done:
			if code != http.StatusNoContent {
				t.Fatalf("%s = %d, want 204", what, code)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s never finished", what)
		}
	}

	// The second save is made, and answered, while the first is being read.
	first := save(`{"model":{"name":"other-model"}}`)
	select {
	case <-reading:
	case <-time.After(5 * time.Second):
		t.Fatal("the first save was never read")
	}
	answered("the second save", save(`{"answerPermissions":false}`))
	released.Do(func() { close(release) })
	answered("the first save", first)

	for name, s := range map[string]hub.Settings{"kept": f.hub.Settings(), "reopened": hub.New(f.kb, f.store).Settings()} {
		if s.AnswerPermissions || s.Model.Name != "other-model" {
			t.Errorf("%s settings answer permissions %v with model %q; want both saves to stand", name, s.AnswerPermissions, s.Model.Name)
		}
	}
}

func TestARejectedSettingsSaveChangesNothing(t *testing.T) {
	f := newFixture(t)
	if code := f.post("/api/settings", `{"model":{"name":"kept"},"prompts":{"global":"Be brief."}}`); code/100 != 2 {
		t.Fatalf("save = %d, want it kept", code)
	}
	if code := f.post("/api/settings", `{"model":{"name":"lost","url":"ftp://nowhere"}}`); code != http.StatusBadRequest {
		t.Fatalf("save with a bad model address = %d, want 400", code)
	}
	if s := f.hub.Settings(); s.Model.Name != "kept" || s.Prompts.Global != "Be brief." {
		t.Errorf("after a rejected save, model %q and global prompt %q; want what was saved before", s.Model.Name, s.Prompts.Global)
	}
}
