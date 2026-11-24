package hub

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/store"
)

// partial keeps the hub in a store, listing only the first document of kind
// before failing, and counting what is written to it.
type partial struct {
	*store.Store
	kind  string
	wrote int
}

func (c *partial) Each(kind string, fn func(id string, body []byte)) error {
	if kind != c.kind {
		return c.Store.Each(kind, fn)
	}
	listed := false
	if err := c.Store.Each(kind, func(id string, body []byte) {
		if !listed {
			listed = true
			fn(id, body)
		}
	}); err != nil {
		return err
	}
	return errors.New("disk unreadable")
}

func (c *partial) Put(kind, id string, v any) error {
	c.wrote++
	return c.Store.Put(kind, id, v)
}

func (c *partial) PutAll(docs ...store.Doc) error {
	c.wrote++
	return c.Store.PutAll(docs...)
}

func (c *partial) Delete(kind, id string) error {
	c.wrote++
	return c.Store.Delete(kind, id)
}

func TestSlorpStartsOnlyFromAllThatIsKept(t *testing.T) {
	bad := map[string]any{"not a record": []int{1}}
	for _, kind := range []string{"session", "origin", "reply", "subagent"} {
		cases := map[string]any{"listed in part": nil}
		for name, v := range bad {
			cases[name] = v
		}
		if kind == "reply" {
			cases["empty"] = map[string]any{}
			cases["misfiled"] = Reply{ID: "elsewhere", SessionID: "old", Text: "and washers", State: Queued}
		}
		for name, row := range cases {
			t.Run(fmt.Sprintf("%s %s", kind, name), func(t *testing.T) {
				h, k, st := newHub(t)
				at := time.Now().Add(-time.Hour)
				h.Link("old", "root")
				h.Link("other", "")
				h.SetRoster(agent, []Session{said("old", "add flanges", at), said("other", "add bolts", at)})
				queued, err := h.Enqueue("old", "and washers")
				must(t, err)
				_, err = h.Enqueue("other", "and nuts")
				must(t, err)
				must(t, h.UpsertSubagent(agent, "old", 1, Subagent{ID: "child", Status: "completed"}, &SubagentReport{Report: "flanges fitted"}))
				must(t, h.UpsertSubagent(agent, "other", 1, Subagent{ID: "child", Status: "completed"}, nil))
				h.SetRoster(agent, []Session{said("other", "add bolts", at)})
				h.Link("new", "old")

				c := &partial{Store: st}
				if row == nil {
					c.kind = kind
				} else {
					must(t, st.Put(kind, "bad", row))
				}
				if _, err := Open(k, c); err == nil {
					t.Fatal("Slorp started from part of what was kept")
				}
				if c.wrote != 0 {
					t.Errorf("Slorp wrote %d times while starting from part of what was kept, want none", c.wrote)
				}

				if row != nil {
					must(t, st.Delete(kind, "bad"))
				}
				h, err = Open(k, st)
				must(t, err)
				h.SetRoster(agent, []Session{said("new", "now washers", at.Add(time.Minute)), said("other", "add bolts", at)})
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

func TestAFilingIsMadeWithTheHubHeld(t *testing.T) {
	h, _, _ := newHub(t)
	h.SetRoster(agent, []Session{idle("a", "/w", "")})
	must(t, h.File("a", func() error {
		if h.mu.TryLock() {
			h.mu.Unlock()
			t.Error("a filing was made with the hub free to take the session up")
		}
		return nil
	}))
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
