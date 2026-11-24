package hub

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/store"
	"github.com/jefflinse/slorp/internal/testdb"
)

// unlisted keeps the hub in a store, failing to list the documents of kind
// while it is set.
type unlisted struct {
	*store.Store
	kind string
}

func (c *unlisted) Each(kind string, fn func(id string, body []byte)) error {
	if kind == c.kind {
		return errors.New("disk unreadable")
	}
	return c.Store.Each(kind, fn)
}

func TestWhatCouldNotBeReadAtStartIsHandedOverOnceItCan(t *testing.T) {
	for _, kind := range []string{"reply", "subagent"} {
		for _, restart := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s unread, restarted %v", kind, restart), func(t *testing.T) {
				st := testdb.Open(t)
				k := testdb.KB(t, st)
				h := New(k, st)
				at := time.Now().Add(-time.Hour)
				h.SetRoster(agent, []Session{{ID: "old", Agent: agent, Status: Idle, Cwd: "/w", StartedAt: at}})
				var queued Reply
				if kind == "reply" {
					var err error
					queued, err = h.Enqueue("old", "and washers")
					must(t, err)
				} else {
					must(t, h.UpsertSubagent(agent, "old", 1, Subagent{ID: "child", Status: "completed"}, &SubagentReport{Report: "flanges fitted"}))
				}
				h.SetRoster(agent, nil)
				h.Link("new", "old")
				next := []Session{said("new", "now washers", at.Add(time.Minute))}

				c := &unlisted{Store: st, kind: kind}
				h = New(k, c)
				if restart {
					h.SetRoster(agent, next)
					if closedIDs(h) != "old" {
						t.Fatalf("closed = %q while what old holds could not all be read, want old kept", closedIDs(h))
					}
					h = New(k, st)
				} else {
					c.kind = ""
				}
				h.SetRoster(agent, next)
				if kind == "reply" {
					if got, err := h.Claim("new"); err != nil || len(got) != 1 || got[0].ID != queued.ID {
						t.Errorf("new claimed %+v, %v; want the reply queued for old, once", got, err)
					}
				} else if report, ok := h.RetainedSubagentReport(agent, "new", "child"); !ok || report.Report != "flanges fitted" {
					t.Errorf("new's report = %+v, %v; want old's", report, ok)
				}
				if closedIDs(h) != "" {
					t.Errorf("closed = %q, want old taken up", closedIDs(h))
				}
			})
		}
	}
}

func TestRepliesReadAgainAreHandedOutInTheOrderTheyWereQueued(t *testing.T) {
	for _, heir := range []bool{false, true} {
		t.Run(fmt.Sprintf("claimed by an heir %v", heir), func(t *testing.T) {
			st := testdb.Open(t)
			k := testdb.KB(t, st)
			h := New(k, st)
			at := time.Now().Add(-time.Hour)
			a := said("a", "add flanges", at)
			h.SetRoster(agent, []Session{a})
			_, err := h.Enqueue("a", "one")
			must(t, err)

			c := &unlisted{Store: st, kind: "reply"}
			h = New(k, c)
			h.SetRoster(agent, []Session{a})
			_, err = h.Enqueue("a", "two")
			must(t, err)
			if got, err := h.Claim("a"); err == nil || len(got) != 0 {
				t.Errorf("a claimed %+v, %v while the replies kept could not all be read, want none and an error", got, err)
			}
			claimant := "a"
			if heir {
				h.SetRoster(agent, nil)
				h.Link("b", "a")
				c.kind = ""
				h.SetRoster(agent, []Session{said("b", "now washers", at.Add(time.Minute))})
				claimant = "b"
			} else {
				c.kind = ""
			}
			claim := func() string {
				t.Helper()
				got, err := h.Claim(claimant)
				must(t, err)
				var texts []string
				for _, r := range got {
					texts = append(texts, r.Text)
				}
				return strings.Join(texts, ",")
			}
			if got := claim(); got != "one,two" {
				t.Errorf("%s claimed %q, want one,two", claimant, got)
			}
			if got := claim(); got != "" {
				t.Errorf("%s claimed %q again, want nothing", claimant, got)
			}
			h = New(k, st)
			if heir {
				h.SetRoster(agent, []Session{said("b", "now washers", at.Add(time.Minute))})
			} else {
				h.SetRoster(agent, []Session{a})
			}
			if got := claim(); got != "" {
				t.Errorf("%s claimed %q after a restart, want nothing", claimant, got)
			}
		})
	}
}

func TestAReplyKeptUnreadablyKeepsTheSessionItWaitsFor(t *testing.T) {
	for name, bad := range map[string]any{
		"not a reply": "and washers",
		"empty":       map[string]any{},
		"misfiled":    Reply{ID: "elsewhere", SessionID: "old", Text: "and washers", State: Queued},
	} {
		t.Run(name, func(t *testing.T) {
			h, k, st := newHub(t)
			at := time.Now().Add(-time.Hour)
			h.SetRoster(agent, []Session{said("old", "add flanges", at)})
			queued, err := h.Enqueue("old", "and washers")
			must(t, err)
			h.SetRoster(agent, nil)
			h.Link("new", "old")
			next := []Session{said("new", "now washers", at.Add(time.Minute))}
			must(t, st.Put("reply", queued.ID, bad))

			h = New(k, st)
			h.SetRoster(agent, next)
			if closedIDs(h) != "old" {
				t.Fatalf("closed = %q while a reply kept could not be read, want old kept", closedIDs(h))
			}
			must(t, st.Put("reply", queued.ID, queued))
			h.SetRoster(agent, next)
			if got, err := h.Claim("new"); err != nil || len(got) != 1 || got[0].ID != queued.ID {
				t.Errorf("new claimed %+v, %v once the reply was repaired; want it, once", got, err)
			}
			if closedIDs(h) != "" {
				t.Errorf("closed = %q, want old taken up", closedIDs(h))
			}
		})
	}
}

func TestWhatWaitsOnWhatCouldNotBeReadGoesOnOnceItCan(t *testing.T) {
	t.Run("a subagent reported", func(t *testing.T) {
		st := testdb.Open(t)
		k := testdb.KB(t, st)
		c := &unlisted{Store: st, kind: "subagent"}
		h := New(k, c)
		h.SetRoster(agent, []Session{idle("a", "/w", "")})
		if err := h.UpsertSubagent(agent, "a", 1, Subagent{ID: "child"}, nil); err == nil {
			t.Error("a subagent was recorded while those kept could not all be read")
		}
		c.kind = ""
		must(t, h.UpsertSubagent(agent, "a", 1, Subagent{ID: "child"}, nil))
	})
	t.Run("a reply acknowledged", func(t *testing.T) {
		h, k, st := newHub(t)
		h.SetRoster(agent, []Session{idle("a", "/w", "")})
		r, err := h.Enqueue("a", "one")
		must(t, err)
		_, err = h.Claim("a")
		must(t, err)
		c := &unlisted{Store: st, kind: "reply"}
		h = New(k, c)
		if err := h.Ack(r.ID, Delivered); err == nil {
			t.Error("a reply was acknowledged while those kept could not all be read")
		}
		c.kind = ""
		must(t, h.Ack(r.ID, Delivered))
		var kept Reply
		if _, err := st.Get("reply", r.ID, &kept); err != nil || kept.State != Delivered {
			t.Errorf("the reply is kept as %q, %v; want delivered", kept.State, err)
		}
	})
	t.Run("a session closed", func(t *testing.T) {
		h, k, st := newHub(t)
		at := time.Now().Add(-time.Hour)
		a := Session{ID: "a", Agent: agent, Status: Idle, Cwd: "/w", StartedAt: at}
		h.SetRoster(agent, []Session{a})
		queued, err := h.Enqueue("a", "one")
		must(t, err)
		c := &unlisted{Store: st, kind: "reply"}
		h = New(k, c)
		h.SetRoster(agent, []Session{a})
		h.SetRoster(agent, nil)
		if closedIDs(h) != "a" {
			t.Fatalf("closed = %q while what a holds could not all be read, want a kept", closedIDs(h))
		}
		c.kind = ""
		h.Link("b", "a")
		h.SetRoster(agent, []Session{said("b", "now washers", at.Add(time.Minute))})
		if got, err := h.Claim("b"); err != nil || len(got) != 1 || got[0].ID != queued.ID {
			t.Errorf("b claimed %+v, %v; want the reply queued for a, once", got, err)
		}
	})
	t.Run("a session run by Slorp", func(t *testing.T) {
		h, k, st := newHub(t)
		h.SetRoster(agent, []Session{idle("a", "/w", "")})
		_, err := h.Enqueue("a", "one")
		must(t, err)
		h.SetRoster(agent, nil)
		c := &unlisted{Store: st, kind: "reply"}
		h = New(k, c)
		if err := h.OpenManaged(agent, "a", "job", "token", nil); err == nil {
			t.Error("a session was run by Slorp while the replies kept could not all be read")
		}
		c.kind = ""
		must(t, h.OpenManaged(agent, "a", "job", "token", nil))
		if got, err := h.Claim("a"); err != nil || len(got) != 1 || got[0].Owner != OwnerRun {
			t.Errorf("the run claimed %+v, %v; want the reply waiting for a, as the run's", got, err)
		}
	})
	t.Run("a session first run by Slorp", func(t *testing.T) {
		st := testdb.Open(t)
		h := New(testdb.KB(t, st), &unlisted{Store: st, kind: "reply"})
		must(t, h.OpenManaged(agent, "fresh", "job", "token", nil))
	})
}

func TestASubagentHandedOverAgainIsAsLastSeen(t *testing.T) {
	h, k, st := newHub(t)
	at := time.Now().Add(-time.Hour)
	a, b := said("a", "one", at), said("b", "two", at.Add(time.Hour))
	h.Link("a", "root")
	h.Link("b", "root")
	a.Subagents = []Subagent{{ID: "listed", Type: "explore"}}
	h.SetRoster(agent, []Session{a})
	h.SetRoster(agent, nil)
	h.SetRoster(agent, []Session{b})
	a.Subagents = []Subagent{{ID: "listed", Type: "explore", Done: true}}
	h.SetRoster(agent, []Session{a, b})
	h.SetRoster(agent, []Session{b})
	for _, h := range []*Hub{h, New(k, st)} {
		h.SetRoster(agent, []Session{b})
		s := session(t, h, "b")
		if len(s.Subagents) != 1 || !s.Subagents[0].Done {
			t.Errorf("b lists %+v, want the subagent as a was last seen with it, done", s.Subagents)
		}
	}
}
