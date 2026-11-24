package hub

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/kb"
	"github.com/jefflinse/slorp/internal/store"
	"github.com/jefflinse/slorp/internal/testdb"
)

// addressed lists, by session, the texts of the replies kept in st.
func addressed(t *testing.T, st *store.Store) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	must(t, st.Each("reply", func(_ string, body []byte) {
		var r Reply
		must(t, json.Unmarshal(body, &r))
		out[r.SessionID] = append(out[r.SessionID], r.Text)
	}))
	for _, texts := range out {
		slices.Sort(texts)
	}
	return out
}

func TestATakeUpStoppedPartWayIsWholeWithoutTheSessionSeenAgain(t *testing.T) {
	began := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	whole := false
	for n := 0; !whole && n <= 50; n++ {
		t.Run(fmt.Sprintf("stopped after %d writes", n), func(t *testing.T) {
			st := testdb.Open(t)
			c := &stopping{Store: st}
			k, err := kb.Open(c)
			must(t, err)
			h := New(k, c)
			e := effort(t, k, "Widgets")
			h.SetRoster(agent, []Session{said("old", "add flanges", began)})
			h.Rename("old", "Flange work")
			must(t, k.Assign("old", e))
			for _, text := range []string{"and bolts", "and washers"} {
				_, err := h.Enqueue("old", text)
				must(t, err)
			}
			h.SetRoster(agent, nil)
			h.Link("new", "old")

			c.armed, c.left = true, n
			h.SetRoster(agent, []Session{said("new", "now add washers", began.Add(time.Hour))})
			whole = !c.stopped
			c.armed = false
			k, err = kb.Open(st)
			must(t, err)
			h = New(k, st)
			h.SetRoster(agent, nil)

			both := []string{"and bolts", "and washers"}
			if found, err := st.Get("session", "old", &sessionDoc{}); err != nil || found {
				o, ok := h.Archive("old")
				if got, _, _ := k.Resolve("old", ""); !ok || o.Label != "Flange work" || got != e || !reflect.DeepEqual(addressed(t, st)["old"], both) {
					t.Errorf("not taken up, old is kept %v as %q under %q with replies %v; want it as it was", ok, o.Label, got, addressed(t, st))
				}
				if a, ok := h.Archive("new"); ok && slices.ContainsFunc(a.Timeline, func(m Moment) bool { return m.Prompt == "add flanges" }) {
					t.Errorf("not taken up, new holds old's turn: %+v", a.Timeline)
				}
				return
			}
			a, ok := h.Archive("new")
			if !ok || a.Label != "Flange work" || a.Effort != e || prompts(Session{Timeline: a.Timeline}) != "add flanges | now add washers" {
				t.Errorf("taken up, new is kept %v as %q under %q with %q; want old's name and filing, and both turns", ok, a.Label, a.Effort, prompts(Session{Timeline: a.Timeline}))
			}
			if got, _, _ := k.Resolve("new", ""); got != e {
				t.Errorf("taken up, new is filed under %q, want %q", got, e)
			}
			if p, ok := h.Past(e); !ok || !slices.ContainsFunc(p.Closed, func(a Archived) bool { return a.ID == "new" }) {
				t.Errorf("the effort's past holds %+v, want new", p.Closed)
			}
			if got := addressed(t, st); !reflect.DeepEqual(got["new"], both) || len(got["old"]) != 0 {
				t.Errorf("replies are kept for %v, want both for new", got)
			}
		})
	}
}

func TestAReopenedSessionKeepsWhatItGainsAfterBeingTakenUp(t *testing.T) {
	for _, shape := range []string{"siblings", "continued"} {
		t.Run(shape, func(t *testing.T) { reopened(t, shape == "continued") })
	}
}

func reopened(t *testing.T, continued bool) {
	h, k, st := newHub(t)
	before, after := effort(t, k, "Before"), effort(t, k, "After")
	at := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	if continued {
		h.Link("branch-b", "branch-a")
	} else {
		h.Link("branch-a", "root")
		h.Link("branch-b", "root")
	}
	a, b := said("branch-a", "one", at), said("branch-b", "two", at.Add(time.Hour))
	h.SetRoster(agent, []Session{a})
	must(t, h.UpsertSubagent(agent, "branch-a", 1, Subagent{ID: "handed", Status: "completed"}, &SubagentReport{Report: "handed over"}))
	must(t, k.Assign("branch-a", before))
	h.SetRoster(agent, nil)
	h.SetRoster(agent, []Session{b})
	if s := session(t, h, "branch-b"); s.Effort != before {
		t.Fatalf("branch-b is under %q, want the filing it took up", s.Effort)
	}

	h.SetRoster(agent, []Session{a, b})
	must(t, k.Assign("branch-a", after))
	must(t, h.UpsertSubagent(agent, "branch-a", 1, Subagent{ID: "since", Status: "completed"}, &SubagentReport{Report: "made since"}))
	must(t, h.UpsertSubagent(agent, "branch-a", 2, Subagent{ID: "handed", Status: "completed"}, &SubagentReport{Report: "changed under branch-a"}))
	queued, err := h.Enqueue("branch-a", "for branch-a")
	must(t, err)
	h.SetRoster(agent, []Session{a, b})

	check := func(h *Hub, when string) {
		t.Helper()
		if got, _, _ := k.Resolve("branch-a", ""); got != after {
			t.Errorf("%s: branch-a is filed under %q, want what it was filed under once reopened", when, got)
		}
		if s := session(t, h, "branch-b"); s.Effort != before || len(s.Subagents) != 1 || s.Subagents[0].ID != "handed" {
			t.Errorf("%s: branch-b is under %q with subagents %+v; want only what was handed over", when, s.Effort, s.Subagents)
		}
		if _, ok := h.RetainedSubagentReport(agent, "branch-b", "since"); ok {
			t.Errorf("%s: branch-b has the report of a subagent branch-a started after it was taken up", when)
		}
		if report, ok := h.RetainedSubagentReport(agent, "branch-b", "handed"); !ok || report.Report != "handed over" {
			t.Errorf("%s: branch-b's report of what it was handed = %+v, %v", when, report, ok)
		}
		if got := h.SubagentSessions("branch-b", "since"); !slices.Equal(got, []string{"branch-b"}) {
			t.Errorf("%s: a subagent started since is looked for under %v, want branch-b alone", when, got)
		}
	}
	check(h, "while both are open")
	k, err = kb.Open(st)
	must(t, err)
	h = New(k, st)
	h.SetRoster(agent, []Session{a, b})
	check(h, "after a restart")

	// Closed again, it is handed over again: what it gained since with it.
	a.Subagents = []Subagent{{ID: "listed", Type: "explore"}}
	h.SetRoster(agent, []Session{a, b})
	h.SetRoster(agent, []Session{b})
	again := func(h *Hub, when string) {
		t.Helper()
		s := session(t, h, "branch-b")
		for _, child := range []string{"handed", "since", "listed"} {
			if !slices.ContainsFunc(s.Subagents, func(a Subagent) bool { return a.ID == child }) {
				t.Errorf("%s: branch-b lists %+v, want %s, handed over", when, s.Subagents, child)
			}
		}
		if got := h.SubagentSessions("branch-b", "listed"); !slices.Equal(got, []string{"branch-b", "branch-a"}) {
			t.Errorf("%s: the listed subagent is looked for under %v, want branch-b then branch-a", when, got)
		}
		for child, want := range map[string]string{"handed": "changed under branch-a", "since": "made since"} {
			if report, ok := h.RetainedSubagentReport(agent, "branch-b", child); !ok || report.Report != want {
				t.Errorf("%s: branch-b's report of %s = %+v, %v; want %q", when, child, report, ok, want)
			}
		}
	}
	again(h, "handed over again")
	if got, err := h.Claim("branch-b"); err != nil || len(got) != 1 || got[0].ID != queued.ID {
		t.Errorf("branch-b claimed %+v, %v; want the reply queued for branch-a once reopened", got, err)
	}
	h = New(k, st)
	h.SetRoster(agent, []Session{b})
	again(h, "handed over again, after a restart")
}

func TestAReplyWaitsForASessionNeverSeenToClose(t *testing.T) {
	h, k, st := newHub(t)
	h.SetRoster(agent, []Session{idle("old", "/w", "")})
	queued, err := h.Enqueue("old", "add flanges")
	must(t, err)

	h = New(k, st)
	h.Link("new", "old")
	h.SetRoster(agent, []Session{idle("new", "/w", "")})
	if got, err := h.Claim("new"); err != nil || len(got) != 1 || got[0].ID != queued.ID {
		t.Errorf("new claimed %+v, %v; want the reply queued for the session it continues", got, err)
	}
}

func TestAFilingOrRoleAloneIsTakenUp(t *testing.T) {
	h, k, st := newHub(t)
	e, r := effort(t, k, "Widgets"), role(t, k, "Reviewer", "")
	for _, id := range []string{"filed", "given", "filed-heir", "given-heir"} {
		h.Link(id, map[bool]string{true: "root-filed", false: "root-given"}[id == "filed" || id == "filed-heir"])
	}
	h.SetRoster(agent, []Session{idle("filed", "/w", ""), idle("given", "/w", "")})
	must(t, k.Assign("filed", e))
	must(t, k.SetRole("given", r))
	h.SetRoster(agent, nil)

	h = New(k, st)
	h.SetRoster(agent, []Session{idle("filed-heir", "/w", ""), idle("given-heir", "/w", "")})
	if s := session(t, h, "filed-heir"); s.Effort != e {
		t.Errorf("the sibling of a session filed alone is under %q, want %q", s.Effort, e)
	}
	if s := session(t, h, "given-heir"); s.Role != r {
		t.Errorf("the sibling of a session given a role alone has %q, want %q", s.Role, r)
	}
}

func TestASubagentIsListedAndReportedFromOneRecord(t *testing.T) {
	h, _, _ := newHub(t)
	at := time.Now().Add(-time.Hour)
	old := said("old", "add flanges", at)
	old.Subagents = []Subagent{{ID: "read", Type: "explore", Done: true}}
	h.SetRoster(agent, []Session{old})
	must(t, h.UpsertSubagent(agent, "old", 1, Subagent{ID: "child", Status: "completed"}, &SubagentReport{Report: "first"}))
	h.SetRoster(agent, nil)
	h.Link("new", "old")
	h.SetRoster(agent, []Session{said("new", "now washers", at.Add(time.Minute))})
	must(t, h.UpsertSubagent(agent, "new", 2, Subagent{ID: "child", Status: "failed"}, &SubagentReport{Report: "second"}))

	s := session(t, h, "new")
	at2 := slices.IndexFunc(s.Subagents, func(a Subagent) bool { return a.ID == "child" })
	if at2 < 0 || s.Subagents[at2].ReportRevision != 2 || s.Subagents[at2].Status != "failed" {
		t.Errorf("subagents are %+v, want child listed as its latest revision", s.Subagents)
	}
	if report, ok := h.RetainedSubagentReport(agent, "new", "child"); !ok || report.Report != "second" {
		t.Errorf("child's report = %+v, %v; want its latest", report, ok)
	}
	if !slices.ContainsFunc(s.Subagents, func(a Subagent) bool { return a.ID == "read" }) {
		t.Errorf("subagents are %+v, want the one the old session's agent listed", s.Subagents)
	}
}
