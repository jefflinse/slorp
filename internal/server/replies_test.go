package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jefflinse/slorp/internal/hub"
	"github.com/jefflinse/slorp/internal/integrations"
	"github.com/jefflinse/slorp/internal/store"
	"github.com/jefflinse/slorp/internal/testdb"
)

// unsavedReplies keeps the hub in a store, failing each write of a reply, or
// of a batch with one, for which fail, given its number counted from when fail
// was set, says so.
type unsavedReplies struct {
	*store.Store
	fail func(n int) bool
	n    int
}

func (c *unsavedReplies) Put(kind, id string, v any) error {
	if kind == "reply" && c.fail != nil {
		c.n++
		if c.fail(c.n) {
			return errors.New("disk full")
		}
	}
	return c.Store.Put(kind, id, v)
}

func (c *unsavedReplies) PutAll(docs ...store.Doc) error {
	for _, d := range docs {
		if d.Kind == "reply" && c.fail != nil {
			c.n++
			if c.fail(c.n) {
				return errors.New("disk full")
			}
			break
		}
	}
	return c.Store.PutAll(docs...)
}

func (c *unsavedReplies) failing(fail func(n int) bool) { c.n, c.fail = 0, fail }

func TestAReplyIsAnsweredForOnlyOnceItIsSaved(t *testing.T) {
	st := testdb.Open(t)
	k := testdb.KB(t, st)
	c := &unsavedReplies{Store: st}
	h := hub.New(k, c)
	srv := httptest.NewServer(New(h, k, integrations.NewManager(func() map[string]bool { return nil }), &asked{}, nil, nil, nil, nil, nil, nil, nil, Page(nil, "")))
	t.Cleanup(srv.Close)
	do := func(path, body string) (int, string) {
		t.Helper()
		res, err := srv.Client().Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		return res.StatusCode, string(b)
	}
	poll := func() []hub.Reply {
		t.Helper()
		code, body := do("/api/bridge/poll", `{"sessionId":"s1","cwd":"/work/a"}`)
		var got struct{ Replies []hub.Reply }
		if code != http.StatusOK || json.Unmarshal([]byte(body), &got) != nil {
			t.Fatalf("poll = %d %s", code, body)
		}
		return got.Replies
	}
	saved := func() map[string]hub.ReplyState {
		t.Helper()
		out := map[string]hub.ReplyState{}
		if err := st.Each("reply", func(_ string, body []byte) {
			var r hub.Reply
			if json.Unmarshal(body, &r) == nil {
				out[r.Text] = r.State
			}
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	poll()

	c.failing(func(int) bool { return true })
	if code, _ := do("/api/sessions/s1/reply", `{"text":"lost"}`); code != http.StatusInternalServerError {
		t.Errorf("a reply that cannot be saved = %d, want 500", code)
	}
	if code, _ := do("/api/sessions/nobody/reply", `{"text":"lost"}`); code != http.StatusNotFound {
		t.Errorf("a reply to no session = %d, want 404", code)
	}
	if got := h.State().Sessions[0].Replies; len(got) != 0 {
		t.Errorf("a reply that could not be saved shows as %+v", got)
	}
	if got := saved(); len(got) != 0 {
		t.Errorf("saved replies %v after one could not be saved, want none", got)
	}

	c.failing(nil)
	var first hub.Reply
	for _, text := range []string{"first", "second"} {
		code, body := do("/api/sessions/s1/reply", `{"text":"`+text+`"}`)
		if code != http.StatusAccepted {
			t.Fatalf("a reply = %d %s, want 202", code, body)
		}
		if text == "first" {
			if err := json.Unmarshal([]byte(body), &first); err != nil {
				t.Fatal(err)
			}
		}
	}

	c.failing(func(n int) bool { return n == 2 })
	if got := poll(); len(got) != 1 || got[0].Text != "first" {
		t.Errorf("a poll whose second claim cannot be saved handed out %+v, want only the first", got)
	}
	c.failing(nil)
	if got := poll(); len(got) != 1 || got[0].Text != "second" {
		t.Errorf("the next poll handed out %+v, want only the second", got)
	}

	c.failing(func(int) bool { return true })
	if code, _ := do("/api/bridge/ack", `{"id":"`+first.ID+`","state":"delivered"}`); code != http.StatusInternalServerError {
		t.Errorf("an acknowledgement that cannot be saved = %d, want 500", code)
	}
	c.failing(nil)
	if code, _ := do("/api/bridge/ack", `{"id":"`+first.ID+`","state":"delivered"}`); code != http.StatusNoContent {
		t.Errorf("the acknowledgement made again = %d, want 204", code)
	}
	if got := saved(); len(got) != 2 || got["first"] != hub.Delivered || got["second"] != hub.Claimed {
		t.Errorf("saved replies %v, want the first delivered and the second claimed", got)
	}
}
