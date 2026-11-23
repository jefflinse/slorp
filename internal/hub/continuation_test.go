package hub

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/kb"
	"github.com/jefflinse/slorp/internal/store"
	"github.com/jefflinse/slorp/internal/testdb"
)

// stopping keeps the hub and its knowledge base in a store until, once
// armed, left writes have been made; every write after fails, as if Slorp
// had stopped there, or with once only the next does. stopped reports
// whether one failed.
type stopping struct {
	*store.Store
	armed, once, stopped bool
	left                 int
}

func (c *stopping) write() error {
	if !c.armed {
		return nil
	}
	if c.left == 0 {
		c.stopped = true
		c.armed = !c.once
		return errors.New("stopped")
	}
	c.left--
	return nil
}

func (c *stopping) Put(kind, id string, v any) error {
	if err := c.write(); err != nil {
		return err
	}
	return c.Store.Put(kind, id, v)
}

func (c *stopping) PutAll(docs ...store.Doc) error {
	if err := c.write(); err != nil {
		return err
	}
	return c.Store.PutAll(docs...)
}

func (c *stopping) Delete(kind, id string) error {
	if err := c.write(); err != nil {
		return err
	}
	return c.Store.Delete(kind, id)
}

func TestAContinuationStoppedPartWayIsTakenUpAgain(t *testing.T) {
	began := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	next := said("new", "now add washers", began.Add(time.Hour))
	check := func(t *testing.T, st *store.Store, h *Hub, k *kb.Store, effort, role string) {
		t.Helper()
		got := h.State()
		if len(got.Sessions) != 1 || len(got.Archive) != 0 {
			t.Fatalf("%d open and %d closed sessions, want the one conversation open and nothing closed", len(got.Sessions), len(got.Archive))
		}
		s := got.Sessions[0]
		if s.ID != "new" || s.Label != "Flange work" || s.Effort != effort || s.Role != role || prompts(s) != "add flanges | add bolts | now add washers" {
			t.Errorf("session %q is %q under %q as %q with %q; want the new id with the old one's name, filing and turns", s.ID, s.Label, s.Effort, s.Role, prompts(s))
		}
		if len(s.Subagents) != 1 || s.Subagents[0].ID != "child" {
			t.Errorf("subagents = %+v, want the one reported under the old id", s.Subagents)
		}
		if report, ok := h.RetainedSubagentReport(agent, "new", "child"); !ok || report.Report != "flanges fitted" {
			t.Errorf("report = %+v, %v; want the one reported under the old id", report, ok)
		}
		if got, err := h.Claim("new"); err != nil || len(got) != 2 {
			t.Errorf("claimed %+v, %v; want both replies queued under the old id", got, err)
		}
		if found, err := st.Get("session", "old", &sessionDoc{}); err != nil || found {
			t.Errorf("the old id's record is still kept (%v), want it gone once taken up", err)
		}
	}

	whole := false
	for n := 0; !whole && n <= 50; n++ {
		for _, how := range []struct{ once, restart bool }{{false, true}, {false, false}, {true, true}, {true, false}} {
			t.Run(fmt.Sprintf("failed after %d writes, once %v, restarted %v", n, how.once, how.restart), func(t *testing.T) {
				st := testdb.Open(t)
				c := &stopping{Store: st}
				k, err := kb.Open(c)
				must(t, err)
				h := New(k, c)
				e, r := effort(t, k, "Widgets"), role(t, k, "Reviewer", "")
				h.SetRoster(agent, []Session{said("old", "add flanges", began)})
				// A second turn said at the same moment as the first.
				h.mu.Lock()
				h.sessions["old"].Timeline = append(h.sessions["old"].Timeline, Moment{At: began, Prompt: "add bolts"})
				h.mu.Unlock()
				h.Rename("old", "Flange work")
				must(t, k.Assign("old", e))
				must(t, k.SetRole("old", r))
				for _, text := range []string{"and washers", "and bolts"} {
					_, err := h.Enqueue("old", text)
					must(t, err)
				}
				must(t, h.UpsertSubagent(agent, "old", 1, Subagent{ID: "child", Status: "completed"}, &SubagentReport{Report: "flanges fitted"}))
				h.SetRoster(agent, nil)
				h.Link("new", "old")

				c.armed, c.once, c.left = true, how.once, n
				h.SetRoster(agent, []Session{next})
				if !c.stopped {
					whole, c.armed = true, false
					check(t, st, h, k, e, r)
					return
				}
				c.armed = false
				if how.restart {
					k, err = kb.Open(st)
					must(t, err)
					h = New(k, st)
				}
				h.SetRoster(agent, []Session{next})
				check(t, st, h, k, e, r)
			})
		}
	}
}
