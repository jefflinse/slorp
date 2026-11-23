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
