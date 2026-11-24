package hub

import (
	"errors"
	"fmt"
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
				h.SetRoster(agent, []Session{said("old", "add flanges", at)})
				queued, err := h.Enqueue("old", "and washers")
				must(t, err)
				must(t, h.UpsertSubagent(agent, "old", 1, Subagent{ID: "child", Status: "completed"}, &SubagentReport{Report: "flanges fitted"}))
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
				if got, err := h.Claim("new"); err != nil || len(got) != 1 || got[0].ID != queued.ID {
					t.Errorf("new claimed %+v, %v; want the reply queued for old, once", got, err)
				}
				if report, ok := h.RetainedSubagentReport(agent, "new", "child"); !ok || report.Report != "flanges fitted" {
					t.Errorf("new's report = %+v, %v; want old's", report, ok)
				}
				if closedIDs(h) != "" {
					t.Errorf("closed = %q, want old taken up", closedIDs(h))
				}
			})
		}
	}
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
