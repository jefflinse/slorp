package hub

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/kb"
	"github.com/jefflinse/slorp/internal/testdb"
)

func TestAContinuationThatFailedGoesWhollyToOneSession(t *testing.T) {
	for n := range 5 {
		for _, restart := range []bool{false, true} {
			t.Run(fmt.Sprintf("failed after %d writes, restarted %v", n, restart), func(t *testing.T) {
				st := testdb.Open(t)
				c := &stopping{Store: st}
				k := testdb.KB(t, st)
				h := New(k, c)
				at := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
				h.SetRoster(agent, []Session{said("old", "add flanges", at)})
				for _, text := range []string{"and washers", "and bolts"} {
					_, err := h.Enqueue("old", text)
					must(t, err)
				}
				h.SetRoster(agent, nil)
				h.Link("a", "old")
				a := said("a", "now washers", at.Add(time.Hour))
				c.armed, c.once, c.left = true, true, n
				h.SetRoster(agent, []Session{a})
				c.armed = false
				if restart {
					h = New(k, st)
				}
				h.Link("b", "old")
				h.SetRoster(agent, []Session{a, said("b", "now bolts", at.Add(2*time.Hour))})
				took := map[string]int{}
				for _, id := range []string{"a", "b"} {
					got, err := h.Claim(id)
					must(t, err)
					took[id] = len(got)
					if strings.Contains(prompts(session(t, h, id)), "add flanges") != (len(got) > 0) {
						t.Errorf("%s has %q and claimed %d replies; want the old turn with the replies or neither", id, prompts(session(t, h, id)), len(got))
					}
				}
				if took["a"]+took["b"] != 2 || took["a"] != 0 && took["b"] != 0 {
					t.Errorf("claimed %v, want both replies claimed by one session", took)
				}
			})
		}
	}
}

func TestATakenUpSessionsSubagentsGoWhereItWent(t *testing.T) {
	st := testdb.Open(t)
	k := testdb.KB(t, st)
	h := New(k, st)
	at := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	h.Link("branch-a", "root")
	h.Link("branch-b", "root")
	h.SetRoster(agent, []Session{said("branch-a", "one", at)})
	must(t, h.UpsertSubagent(agent, "branch-a", 1, Subagent{ID: "child", Status: "completed"}, &SubagentReport{Report: "flanges fitted"}))
	h.SetRoster(agent, nil)
	h.SetRoster(agent, []Session{said("branch-b", "two", at.Add(time.Hour))})
	// Another branch, open beside it, that takes nothing up.
	h.Link("branch-c", "root")
	both := []Session{said("branch-c", "three", at), said("branch-b", "two", at.Add(time.Hour))}
	h.SetRoster(agent, both)

	check := func(h *Hub, when string) {
		t.Helper()
		if prompts(session(t, h, "branch-b")) != "one | two" {
			t.Fatalf("%s: branch-b has %q, want what branch-a said taken up", when, prompts(session(t, h, "branch-b")))
		}
		if s := session(t, h, "branch-b"); len(s.Subagents) != 1 || s.Subagents[0].ID != "child" {
			t.Errorf("%s: branch-b's subagents are %+v, want branch-a's", when, s.Subagents)
		}
		if report, ok := h.RetainedSubagentReport(agent, "branch-b", "child"); !ok || report.Report != "flanges fitted" {
			t.Errorf("%s: branch-b's report = %+v, %v; want branch-a's", when, report, ok)
		}
		if s := session(t, h, "branch-c"); len(s.Subagents) != 0 {
			t.Errorf("%s: branch-c, which took nothing up, has subagents %+v", when, s.Subagents)
		}
		if _, ok := h.RetainedSubagentReport(agent, "branch-c", "child"); ok {
			t.Errorf("%s: branch-c, which took nothing up, has branch-a's report", when)
		}
	}
	check(h, "as it is taken up")
	h = New(k, st)
	h.SetRoster(agent, both)
	check(h, "after a restart")
}

func TestASessionWithNothingSaidIsTakenUpForWhatItHolds(t *testing.T) {
	st := testdb.Open(t)
	k := testdb.KB(t, st)
	h := New(k, st)
	h.SetRoster(agent, []Session{idle("old", "/w", "")})
	h.Rename("old", "Flange work")
	queued, err := h.Enqueue("old", "add flanges")
	must(t, err)
	h.SetRoster(agent, nil)
	if closedIDs(h) != "old" {
		t.Fatalf("closed = %q, want a named session with a reply waiting kept", closedIDs(h))
	}
	h = New(k, st)
	if closedIDs(h) != "old" {
		t.Fatalf("closed after a restart = %q, want it kept", closedIDs(h))
	}
	h.Link("new", "old")
	h.SetRoster(agent, []Session{idle("new", "/w", "")})
	if s := session(t, h, "new"); s.Label != "Flange work" {
		t.Errorf("new is named %q, want the name given to the one it continues", s.Label)
	}
	if got, err := h.Claim("new"); err != nil || len(got) != 1 || got[0].ID != queued.ID {
		t.Errorf("new claimed %+v, %v; want the reply waiting for the one it continues", got, err)
	}

	// Nothing said or given, but a reply waiting.
	h.SetRoster(agent, []Session{idle("waiting", "/w", "")})
	queued, err = h.Enqueue("waiting", "add bolts")
	must(t, err)
	h.SetRoster(agent, nil)
	h = New(k, st)
	h.Link("next", "waiting")
	h.SetRoster(agent, []Session{idle("next", "/w", "")})
	if got, err := h.Claim("next"); err != nil || len(got) != 1 || got[0].ID != queued.ID {
		t.Errorf("next claimed %+v, %v; want the reply waiting for the one it continues", got, err)
	}

	// Nothing said, but a subagent's report taken up.
	h.SetRoster(agent, []Session{idle("holder", "/w", "")})
	must(t, h.UpsertSubagent(agent, "holder", 1, Subagent{ID: "child", Status: "completed"}, &SubagentReport{Report: "flanges fitted"}))
	h.SetRoster(agent, nil)
	h.Link("heir", "holder")
	h.SetRoster(agent, []Session{idle("heir", "/w", "")})
	h.SetRoster(agent, nil)
	for _, h := range []*Hub{h, New(k, st)} {
		if _, ok := h.Archive("heir"); !ok {
			t.Fatal("a closed session holding only a report it took up was not kept")
		}
		if report, ok := h.RetainedSubagentReport(agent, "heir", "child"); !ok || report.Report != "flanges fitted" {
			t.Errorf("its report = %+v, %v; want the one it took up", report, ok)
		}
	}
}

func TestAFailedTakeUpLeavesTheLatestToCloseHeededFirst(t *testing.T) {
	st := testdb.Open(t)
	c := &stopping{Store: st}
	k := testdb.KB(t, st)
	h := New(k, c)
	older, newer := effort(t, k, "Older"), effort(t, k, "Newer")
	at := time.Now().Add(-5 * time.Hour).Truncate(time.Second)
	h.SetRoster(agent, []Session{said("a-early", "one", at), said("b-late", "two", at.Add(time.Hour))})
	h.Rename("a-early", "Early name")
	h.Rename("b-late", "Late name")
	must(t, k.Assign("a-early", older))
	must(t, k.Assign("b-late", newer))
	h.SetRoster(agent, []Session{said("b-late", "two", at.Add(time.Hour))})
	h.SetRoster(agent, nil)
	h.Link("a-early", "root")
	h.Link("b-late", "root")
	h.Link("open", "root")

	open := []Session{said("open", "three", at.Add(2*time.Hour))}
	c.armed, c.once = true, true
	h.SetRoster(agent, open)
	h.SetRoster(agent, open)
	if s := session(t, h, "open"); !c.stopped || s.Label != "Late name" || s.Effort != newer || prompts(s) != "one | two | three" {
		t.Errorf("session is %q under %q with %q after a failed take-up (failed %v), want the latest to close heeded first", s.Label, s.Effort, prompts(s), c.stopped)
	}
}

func TestAFilingThatCouldNotFollowIsCarriedLater(t *testing.T) {
	st := testdb.Open(t)
	c := &stopping{Store: st}
	k, err := kb.Open(c)
	must(t, err)
	h := New(k, c)
	e := effort(t, k, "Widgets")
	at := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	h.Link("branch-a", "root")
	h.Link("branch-b", "root")
	h.SetRoster(agent, []Session{said("branch-a", "one", at)})
	must(t, k.Assign("branch-a", e))
	h.SetRoster(agent, nil)

	b := []Session{said("branch-b", "two", at.Add(time.Hour))}
	c.armed, c.once, c.left = true, true, 1
	h.SetRoster(agent, b)
	if s := session(t, h, "branch-b"); !c.stopped || prompts(s) != "one | two" || s.Effort != e {
		t.Errorf("branch-b has %q under %q (failed %v), want branch-a taken up with its filing", prompts(s), s.Effort, c.stopped)
	}
}

func TestATakenUpFilingIsKeptAsTheSessionCloses(t *testing.T) {
	h, k, st := newHub(t)
	e := effort(t, k, "Widgets")
	at := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	h.SetRoster(agent, []Session{said("old", "add flanges", at)})
	must(t, k.Assign("old", e))
	h.SetRoster(agent, nil)
	h.Link("new", "old")
	h.SetRoster(agent, []Session{idle("new", "/w", "")})

	h = New(k, st)
	h.SetRoster(agent, nil)
	if a, ok := h.Archive("new"); !ok || a.Effort != e {
		t.Errorf("closed, new is kept %v under %q; want it under the effort it took up", ok, a.Effort)
	}
	if p, ok := h.Past(e); !ok || !slices.ContainsFunc(p.Closed, func(a Archived) bool { return a.ID == "new" }) {
		t.Errorf("the effort's past holds %+v, want new among what closed", p.Closed)
	}
}

func TestSessionsThatContinueEachOtherAreListed(t *testing.T) {
	h, _, _ := newHub(t)
	h.Link("x", "y")
	h.Link("y", "x")
	at := time.Now().Add(-time.Hour)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.SetRoster(agent, []Session{said("x", "one", at), said("y", "two", at)})
		h.Sessions()
		h.RetainedSubagentReport(agent, "x", "child")
		h.SubagentSessions("y")
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("listing two sessions that continue each other did not finish")
	}
}
