package hub

import (
	"errors"
	"testing"

	"github.com/jefflinse/slorp/internal/store"
	"github.com/jefflinse/slorp/internal/testdb"
)

// faultyReplies keeps the hub in a store, failing each write of a reply for
// which fail, given its number counted from when fail was set, says so.
type faultyReplies struct {
	*store.Store
	fail func(n int) bool
	n    int
}

func (c *faultyReplies) Put(kind, id string, v any) error {
	if kind == "reply" && c.fail != nil {
		c.n++
		if c.fail(c.n) {
			return errors.New("disk full")
		}
	}
	return c.Store.Put(kind, id, v)
}

func (c *faultyReplies) failing(fail func(n int) bool) { c.n, c.fail = 0, fail }

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
