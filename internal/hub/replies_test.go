package hub

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/store"
	"github.com/jefflinse/slorp/internal/testdb"
)

// faultyReplies keeps the hub in a store, failing each write of a reply for
// which fail, given its number counted from when fail was set, says so, every
// write of a session while sessions is set, and every read of a session while
// unreadable is set or of the one unreadableID names. It counts the
// sessions put and the batches written.
type faultyReplies struct {
	*store.Store
	fail                 func(n int) bool
	n                    int
	sessions, unreadable bool
	unreadableID         string
	sessionPuts, batches int
}

func (c *faultyReplies) Get(kind, id string, v any) (bool, error) {
	if kind == "session" && (c.unreadable || id == c.unreadableID) {
		return false, errors.New("disk unreadable")
	}
	return c.Store.Get(kind, id, v)
}

func (c *faultyReplies) Put(kind, id string, v any) error {
	if kind == "session" && c.sessions {
		return errors.New("disk full")
	}
	if kind == "session" {
		c.sessionPuts++
	}
	if kind == "reply" && c.fail != nil {
		c.n++
		if c.fail(c.n) {
			return errors.New("disk full")
		}
	}
	return c.Store.Put(kind, id, v)
}

func (c *faultyReplies) PutAll(docs ...store.Doc) error {
	replies := false
	for _, d := range docs {
		if d.Kind == "session" && c.sessions {
			return errors.New("disk full")
		}
		replies = replies || d.Kind == "reply"
	}
	if replies && c.fail != nil {
		c.n++
		if c.fail(c.n) {
			return errors.New("disk full")
		}
	}
	c.batches++
	return c.Store.PutAll(docs...)
}

func (c *faultyReplies) failing(fail func(n int) bool) { c.n, c.fail = 0, fail }

func TestWhatWaitsForASessionGoesWithTheRunThatTakesItOver(t *testing.T) {
	st := testdb.Open(t)
	k := testdb.KB(t, st)
	c := &faultyReplies{Store: st}
	h := New(k, c)
	queuedElsewhere := func(id string) Reply {
		t.Helper()
		h.Apply("opencode", id, func(s *Session) { s.LastPrompt = "in a terminal" })
		r, err := h.Enqueue(id, "queued in "+id)
		must(t, err)
		h.Retire("opencode", -time.Second)
		return r
	}
	saved := func(id string) Reply {
		t.Helper()
		var r Reply
		if found, err := st.Get("reply", id, &r); err != nil || !found {
			t.Fatalf("reply %s is not saved: %v", id, err)
		}
		return r
	}
	afterRestart := func(id string) []Reply {
		t.Helper()
		restarted := New(k, st)
		restarted.Apply("opencode", id, func(s *Session) { s.LastPrompt = "in a terminal again" })
		got, err := restarted.Claim(id)
		must(t, err)
		return got
	}

	// Stopped before its first poll, where the drop could not be saved.
	stopped := queuedElsewhere("ses_stopped")
	must(t, h.OpenManaged("opencode", "ses_stopped", "job", "token-stopped", nil))
	if r := saved(stopped.ID); r.Owner != OwnerRun || r.State != Queued {
		t.Errorf("a waiting reply when a run took its session over is saved as %+v, want queued for the run", r)
	}
	c.failing(always)
	h.CloseManaged("token-stopped")
	c.failing(nil)
	if got := afterRestart("ses_stopped"); len(got) != 0 {
		t.Errorf("a reply cancelled with the run that took it over came back after a restart: %+v", got)
	}

	// Claimed by the run, which then crashed.
	crashed := queuedElsewhere("ses_crashed")
	must(t, h.OpenManaged("opencode", "ses_crashed", "job", "token-crashed", nil))
	if got, err := h.Claim("ses_crashed"); err != nil || len(got) != 1 || got[0].ID != crashed.ID {
		t.Fatalf("the run claimed %+v, %v; want the reply that was waiting", got, err)
	}
	afterRestart("ses_crashed")
	if r := saved(crashed.ID); r.State != Dropped {
		t.Errorf("a reply claimed by a run that crashed is %s after a restart, want dropped", r.State)
	}

	// Not taken over, when what waits could not be given to the run.
	kept := queuedElsewhere("ses_kept")
	c.failing(always)
	if err := h.OpenManaged("opencode", "ses_kept", "job", "token-kept", nil); !errors.Is(err, ErrNotSaved) {
		t.Fatalf("taking over a session whose replies cannot be given to the run = %v, want ErrNotSaved", err)
	}
	c.failing(nil)
	if _, release, err := h.BridgeLease(context.Background(), "token-kept", "opencode", "ses_kept", "", "/api/bridge/poll"); err == nil {
		release()
		t.Error("a run was opened though what waited for its session could not be given to it")
	}
	if r := saved(kept.ID); r.Owner != OwnerBridge || r.State != Queued {
		t.Errorf("a reply after a failed takeover is saved as %+v, want still queued for the terminal", r)
	}
	if got := afterRestart("ses_kept"); len(got) != 1 || got[0].ID != kept.ID {
		t.Errorf("after a failed takeover and a restart the terminal was handed %+v, want its reply", got)
	}

	// Not taken over, when the session could not be kept as the run's.
	unkept := queuedElsewhere("ses_unkept")
	c.sessions = true
	if err := h.OpenManaged("opencode", "ses_unkept", "job", "token-unkept", nil); !errors.Is(err, ErrNotSaved) {
		t.Fatalf("taking over a session that cannot be saved = %v, want ErrNotSaved", err)
	}
	c.sessions = false
	if _, release, err := h.BridgeLease(context.Background(), "token-unkept", "opencode", "ses_unkept", "", "/api/bridge/poll"); err == nil {
		release()
		t.Error("a run was opened though its session could not be saved")
	}
	h.CloseManaged("token-unkept")
	if r := saved(unkept.ID); r.Owner != OwnerBridge || r.State != Queued {
		t.Errorf("a reply after a takeover whose session could not be saved is saved as %+v, want still queued for the terminal", r)
	}
	if got := afterRestart("ses_unkept"); len(got) != 1 || got[0].ID != unkept.ID {
		t.Errorf("after a takeover whose session could not be saved and a restart the terminal was handed %+v, want its reply", got)
	}

	// Not taken over, when what was kept of the session could not be read.
	h.Apply("opencode", "ses_unread", func(s *Session) { s.LastPrompt = "in a terminal" })
	h.Rename("ses_unread", "Named by hand")
	unread, err := h.Enqueue("ses_unread", "queued in ses_unread")
	must(t, err)
	h.Retire("opencode", -time.Second)
	stored := func() sessionDoc {
		t.Helper()
		var d sessionDoc
		if found, err := st.Get("session", "ses_unread", &d); err != nil || !found {
			t.Fatalf("ses_unread is not saved: %v", err)
		}
		return d
	}
	before := stored()
	if _, archived := h.archive["ses_unread"]; !archived || before.Given != "Named by hand" || len(before.Timeline) != 1 {
		t.Fatalf("ses_unread is archived %v as %+v, want named with one moment", archived, before)
	}
	c.unreadable, c.batches = true, 0
	if err := h.OpenManaged("opencode", "ses_unread", "job", "token-unread", nil); err == nil {
		t.Fatal("a session whose record could not be read was taken over")
	}
	c.unreadable = false
	if c.batches != 0 {
		t.Errorf("taking over a session whose record could not be read wrote %d batches, want none", c.batches)
	}
	if _, release, err := h.BridgeLease(context.Background(), "token-unread", "opencode", "ses_unread", "", "/api/bridge/poll"); err == nil {
		release()
		t.Error("a run was opened though its session could not be read")
	}
	if _, archived := h.archive["ses_unread"]; !archived {
		t.Error("a session whose record could not be read left the archive")
	}
	if after := stored(); !reflect.DeepEqual(after, before) {
		t.Errorf("a session whose record could not be read is saved as %+v, want %+v", after, before)
	}
	if r := saved(unread.ID); r.Owner != OwnerBridge || r.State != Queued {
		t.Errorf("a reply after a takeover whose session could not be read is saved as %+v, want still queued for the terminal", r)
	}
	must(t, h.OpenManaged("opencode", "ses_unread", "job", "token-unread", nil))
	if s := h.sessions["ses_unread"]; s.given != "Named by hand" || len(s.Timeline) != 1 {
		t.Errorf("taken over once it could be read, ses_unread is named %q with %d moments, want its name and moment", s.given, len(s.Timeline))
	}
	if after := stored(); after.Given != "Named by hand" || len(after.Timeline) != 1 {
		t.Errorf("taken over once it could be read, ses_unread is saved as %+v, want its name and moment", after)
	}
}

func TestASessionKeptUnreadablyIsNotTakenOver(t *testing.T) {
	st := testdb.Open(t)
	h := New(testdb.KB(t, st), st)
	must(t, st.Put("session", "ses_garbled", "not a session"))
	if err := h.OpenManaged("opencode", "ses_garbled", "job", "token", nil); err == nil {
		t.Fatal("a session kept as something that is not one was taken over")
	}
	var kept string
	if found, err := st.Get("session", "ses_garbled", &kept); err != nil || !found || kept != "not a session" {
		t.Errorf("a session that could not be decoded is saved as %q, %v, %v; want left as it was", kept, found, err)
	}
}

func TestATakeoverIsWrittenOnce(t *testing.T) {
	st := testdb.Open(t)
	c := &faultyReplies{Store: st}
	h := New(testdb.KB(t, st), c)
	must(t, h.OpenManaged("opencode", "ses_new", "job", "token", nil))
	if c.batches != 1 || c.sessionPuts != 0 {
		t.Errorf("taking a new session over wrote %d batches and put the session %d times, want one batch alone", c.batches, c.sessionPuts)
	}
	h.mu.Lock()
	h.sessions["ses_new"].Title = "Retitled"
	h.changed()
	h.mu.Unlock()
	var d sessionDoc
	if found, err := st.Get("session", "ses_new", &d); err != nil || !found || d.Title != "Retitled" || c.sessionPuts != 1 {
		t.Errorf("after a change the session is saved as %+v (put %d times), %v; want retitled once", d, c.sessionPuts, err)
	}
}

func always(int) bool { return true }

func TestRepliesAreSavedBeforeTheyMove(t *testing.T) {
	st := testdb.Open(t)
	k := testdb.KB(t, st)
	c := &faultyReplies{Store: st}
	open := func(c Cache) *Hub {
		h := New(k, c)
		h.SetRoster(agent, []Session{idle("s1", "/w", "")})
		return h
	}
	h := open(c)
	handed := map[string]int{}
	claim := func(h *Hub) ([]Reply, error) {
		got, err := h.Claim("s1")
		for _, r := range got {
			handed[r.ID]++
		}
		return got, err
	}
	states := func(h *Hub) map[string]ReplyState {
		out := map[string]ReplyState{}
		for _, r := range session(t, h, "s1").Replies {
			out[r.Text] = r.State
		}
		return out
	}
	stand := func(what string, want map[string]ReplyState) {
		t.Helper()
		for name, h := range map[string]*Hub{"kept": h, "reopened": open(st)} {
			got := states(h)
			if len(got) != len(want) {
				t.Errorf("%s, %s replies are %v, want %v", what, name, got, want)
				continue
			}
			for text, state := range want {
				if got[text] != state {
					t.Errorf("%s, %s replies are %v, want %v", what, name, got, want)
					break
				}
			}
		}
	}

	c.failing(always)
	if _, err := h.Enqueue("s1", "lost"); !errors.Is(err, ErrNotSaved) {
		t.Fatalf("queuing a reply that cannot be saved = %v, want ErrNotSaved", err)
	}
	stand("after a reply could not be queued", map[string]ReplyState{})

	c.failing(nil)
	first, err := h.Enqueue("s1", "first")
	must(t, err)
	_, err = h.Enqueue("s1", "second")
	must(t, err)

	c.failing(func(n int) bool { return n == 2 })
	if got, err := claim(h); !errors.Is(err, ErrNotSaved) || len(got) != 1 || got[0].ID != first.ID {
		t.Fatalf("claiming two replies when the second cannot be saved = %+v, %v; want the first and ErrNotSaved", got, err)
	}
	stand("after the second claim could not be saved", map[string]ReplyState{"first": Claimed, "second": Queued})

	c.failing(always)
	if got, err := claim(h); !errors.Is(err, ErrNotSaved) || len(got) != 0 {
		t.Fatalf("claiming a reply that cannot be saved = %+v, %v; want none and ErrNotSaved", got, err)
	}
	c.failing(nil)
	if got, err := claim(h); err != nil || len(got) != 1 || got[0].Text != "second" {
		t.Fatalf("claiming again = %+v, %v; want the second reply", got, err)
	}

	c.failing(always)
	if err := h.Ack(first.ID, Delivered); !errors.Is(err, ErrNotSaved) {
		t.Fatalf("acknowledging a reply that cannot be saved = %v, want ErrNotSaved", err)
	}
	stand("after an acknowledgement could not be saved", map[string]ReplyState{"first": Claimed, "second": Claimed})
	c.failing(nil)
	must(t, h.Ack(first.ID, Delivered))
	stand("after the acknowledgement was made again", map[string]ReplyState{"first": Delivered, "second": Claimed})

	if got, err := claim(open(st)); err != nil || len(got) != 0 {
		t.Errorf("a restart handed out %+v, %v; want nothing", got, err)
	}
	for id, n := range handed {
		if n != 1 {
			t.Errorf("reply %s was handed out %d times, want once", id, n)
		}
	}
	if len(handed) != 2 {
		t.Errorf("%d replies were handed out, want both", len(handed))
	}
}

func TestARestartKeepsWhatWasQueuedForASessionRunElsewhere(t *testing.T) {
	h, k, cache := newHub(t)
	must(t, h.OpenManaged("opencode", "ses_root", "job", "token", nil))
	_, err := h.Enqueue("ses_root", "for the run")
	must(t, err)
	h.CloseManaged("token")

	// The same conversation, opened again in a terminal of the user's own.
	h.Apply("opencode", "ses_root", func(s *Session) { s.LastPrompt = "reopened" })
	kept, err := h.Enqueue("ses_root", "for the terminal")
	must(t, err)
	// A reply from before Slorp kept who it was queued for.
	must(t, cache.Put("reply", "legacy", Reply{ID: "legacy", SessionID: "ses_root", Text: "from before", State: Queued, CreatedAt: time.Now()}))

	restarted := New(k, cache)
	restarted.Apply("opencode", "ses_root", func(s *Session) { s.LastPrompt = "reopened again" })
	states := map[string]ReplyState{}
	for _, r := range session(t, restarted, "ses_root").Replies {
		states[r.Text] = r.State
	}
	if states["for the run"] != Dropped || states["from before"] != Dropped || states["for the terminal"] != Queued {
		t.Errorf("after a restart, replies are %v; want the run's and the one from before dropped, and the terminal's queued", states)
	}
	if got, err := restarted.Claim("ses_root"); err != nil || len(got) != 1 || got[0].ID != kept.ID {
		t.Errorf("the next poll after a restart handed out %+v, %v; want the reply queued for the terminal", got, err)
	}
}

func TestOnlyTheRunThatClaimedAReplyAcknowledgesItAgain(t *testing.T) {
	st := testdb.Open(t)
	c := &faultyReplies{Store: st}
	k := testdb.KB(t, st)
	h := New(k, c)
	must(t, h.OpenManaged("opencode", "one", "job-one", "token-one", nil))
	must(t, h.OpenManaged("opencode", "two", "job-two", "token-two", nil))
	r, err := h.Enqueue("one", "queued")
	must(t, err)
	_, err = h.Claim("one")
	must(t, err)

	c.failing(always)
	if err := h.Ack(r.ID, Delivered); !errors.Is(err, ErrNotSaved) {
		t.Fatalf("acknowledging when it cannot be saved = %v, want ErrNotSaved", err)
	}
	c.failing(nil)
	for _, token := range []string{"", "token-two"} {
		if _, release, err := h.BridgeLease(context.Background(), token, "", "", r.ID, "/api/bridge/ack"); err == nil {
			release()
			t.Errorf("a caller with token %q may acknowledge another run's reply", token)
		}
	}
	_, release, err := h.BridgeLease(context.Background(), "token-one", "", "", r.ID, "/api/bridge/ack")
	if err != nil {
		t.Fatalf("the run that claimed the reply may not acknowledge it again: %v", err)
	}
	must(t, h.Ack(r.ID, Delivered))
	release()
	var saved Reply
	if found, err := st.Get("reply", r.ID, &saved); err != nil || !found || saved.State != Delivered {
		t.Errorf("saved reply %+v, %v, %v; want it delivered", saved, found, err)
	}
	h.CloseManaged("token-one")
	h.CloseManaged("token-two")
}

func TestRepliesAreNotHandedOutPastOneThatCouldNotBeClaimed(t *testing.T) {
	st := testdb.Open(t)
	c := &faultyReplies{Store: st}
	h := New(testdb.KB(t, st), c)
	h.SetRoster(agent, []Session{idle("s1", "/w", "")})
	for _, text := range []string{"first", "second"} {
		_, err := h.Enqueue("s1", text)
		must(t, err)
	}
	c.failing(func(n int) bool { return n == 1 })
	if got, err := h.Claim("s1"); !errors.Is(err, ErrNotSaved) || len(got) != 0 {
		t.Fatalf("claiming when the first claim cannot be saved = %+v, %v; want none and ErrNotSaved", got, err)
	}
	c.failing(nil)
	if got, err := h.Claim("s1"); err != nil || len(got) != 2 || got[0].Text != "first" || got[1].Text != "second" {
		t.Errorf("claiming again = %+v, %v; want both, in order", got, err)
	}
}
