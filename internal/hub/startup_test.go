package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	bad := map[string]any{"not a record": []int{1}, "null": json.RawMessage("null")}
	for _, kind := range []string{"session", "origin", "reply", "subagent"} {
		cases := map[string]any{"listed in part": nil}
		for name, v := range bad {
			cases[name] = v
		}
		if kind == "reply" {
			cases["empty"] = map[string]any{}
			cases["misfiled"] = Reply{ID: "elsewhere", SessionID: "old", Text: "and washers", State: Queued}
			cases["stateless"] = Reply{ID: "bad", SessionID: "old", Text: "and washers", Owner: OwnerBridge}
			cases["in an unknown state"] = Reply{ID: "bad", SessionID: "old", Text: "and washers", State: "lost", Owner: OwnerBridge}
			cases["owned by no known runner"] = Reply{ID: "bad", SessionID: "old", Text: "and washers", State: Queued, Owner: "not-an-owner"}
			for _, field := range []string{"id", "sessionId", "text", "state", "createdAt", "owner"} {
				reply := map[string]any{"id": "bad", "sessionId": "old", "text": "and washers", "state": "queued", "createdAt": "2026-11-24T00:00:00Z", "owner": "bridge"}
				reply[field] = nil
				cases["with "+field+" null"] = reply
			}
			cases["without text"] = map[string]any{"id": "bad", "sessionId": "old", "state": "queued", "createdAt": "2026-11-24T00:00:00Z", "owner": "bridge"}
			cases["of empty text"] = Reply{ID: "bad", SessionID: "old", State: Queued, Owner: OwnerBridge}
			cases["of blank text"] = Reply{ID: "bad", SessionID: "old", Text: " \n\t", State: Queued, Owner: OwnerBridge}
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
				_, err = Open(k, c)
				if err == nil {
					t.Fatal("Slorp started from part of what was kept")
				}
				if row != nil && !strings.Contains(err.Error(), "bad") {
					t.Errorf("Slorp would not start, saying %q; want the row that stopped it named", err)
				}
				if field, null := strings.CutPrefix(name, "with "); null && !strings.Contains(err.Error(), strings.TrimSuffix(field, " null")+" is null") {
					t.Errorf("Slorp would not start, saying %q; want the null field named", err)
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

func TestRepliesKeptAreSettledByWhoOwnedThem(t *testing.T) {
	h, k, st := newHub(t)
	must(t, h.OpenManaged(agent, "run", "job", "token", nil))
	_, err := h.Enqueue("run", "for the run")
	must(t, err)
	h.SetRoster(agent, []Session{idle("bridged", "/w", "")})
	_, err = h.Enqueue("bridged", "for the bridge")
	must(t, err)
	h.SetRoster(agent, []Session{idle("legacy", "/w", "")})
	legacy, err := h.Enqueue("legacy", "from before owners")
	must(t, err)
	legacy.Owner = ""
	must(t, st.Put("reply", legacy.ID, legacy))
	h.SetRoster(agent, []Session{idle("unowned", "/w", "")})
	unowned, err := h.Enqueue("unowned", "from before owners, said so")
	must(t, err)
	must(t, st.Put("reply", unowned.ID, json.RawMessage(`{"id":"`+unowned.ID+`","sessionId":"unowned","text":"from before owners, said so","state":"queued","createdAt":"2026-11-24T00:00:00Z","owner":""}`)))

	h = New(k, st)
	h.SetRoster(agent, []Session{idle("run", "/w", ""), idle("bridged", "/w", ""), idle("legacy", "/w", ""), idle("unowned", "/w", "")})
	for id, want := range map[string]int{"run": 0, "bridged": 1, "legacy": 1, "unowned": 1} {
		if got, err := h.Claim(id); err != nil || len(got) != want {
			t.Errorf("%s claimed %+v, %v after a restart; want %d", id, got, err, want)
		}
	}
}

func TestAClosedSessionFiledAgainIsPublished(t *testing.T) {
	h, k, _ := newHub(t)
	one, two := effort(t, k, "One"), effort(t, k, "Two")
	at := time.Now().Add(-time.Hour)
	for _, id := range []string{"a", "b", "c"} {
		h.SetRoster(agent, []Session{said(id, "work on "+id, at)})
		h.SetRoster(agent, nil)
	}
	must(t, k.Assign("b", one))
	must(t, k.Assign("c", two))
	must(t, h.File("a", func() error { return k.Assign("a", one) }))
	h.Touch()
	ch, cancel := h.Subscribe()
	defer cancel()
	must(t, h.File("a", func() error { return k.Assign("a", two) }))
	h.Touch()
	select {
	case <-ch:
	default:
		t.Error("filing a closed session again was not published")
	}
	if a, _ := h.Archive("a"); a.Effort != two {
		t.Errorf("a is under %q, want %q", a.Effort, two)
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
