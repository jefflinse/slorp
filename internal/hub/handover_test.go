package hub

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

func TestAHandedReportOutlivesAnUpdateWithoutOne(t *testing.T) {
	h, k, st := newHub(t)
	at := time.Now().Add(-time.Hour)
	h.SetRoster(agent, []Session{said("old", "add flanges", at)})
	must(t, h.UpsertSubagent(agent, "old", 1, Subagent{ID: "child", Status: "running"}, &SubagentReport{Report: "before"}))
	h.SetRoster(agent, nil)
	h.Link("new", "old")
	next := said("new", "now washers", at.Add(time.Minute))
	h.SetRoster(agent, []Session{next})
	must(t, h.UpsertSubagent(agent, "new", 2, Subagent{ID: "child", Status: "completed"}, nil))
	for _, h := range []*Hub{h, New(k, st)} {
		if report, ok := h.RetainedSubagentReport(agent, "new", "child"); !ok || report.Report != "before" {
			t.Errorf("report = %+v, %v; want the one handed over, kept through an update without one", report, ok)
		}
	}
}

func TestWhatIsHandedOverLeavesRoomForASessionsOwn(t *testing.T) {
	h, k, st := newHub(t)
	at := time.Now().Add(-time.Hour)
	h.SetRoster(agent, []Session{said("old", "add flanges", at)})
	must(t, h.UpsertSubagent(agent, "old", 1, Subagent{ID: "handed", Status: "running"}, &SubagentReport{Report: "before"}))
	h.SetRoster(agent, nil)
	h.Link("new", "old")
	h.SetRoster(agent, []Session{said("new", "now washers", at.Add(time.Minute))})
	must(t, h.UpsertSubagent(agent, "new", 2, Subagent{ID: "handed", Status: "completed"}, nil))
	for i := range retainedSubagentLimit {
		must(t, h.UpsertSubagent(agent, "new", 1, Subagent{ID: fmt.Sprintf("own%03d", i), Status: "completed"}, nil))
	}
	h = New(k, st)
	h.SetRoster(agent, []Session{said("new", "now washers", at.Add(time.Minute))})
	if err := h.UpsertSubagent(agent, "new", 3, Subagent{ID: "handed", Status: "completed"}, nil); err != nil {
		t.Errorf("after a restart, updating what was handed over = %v, want its records read back", err)
	}
}

func TestAFilingOrRoleAloneIsTakenUpFromASessionNeverSeenToClose(t *testing.T) {
	h, k, st := newHub(t)
	e, r := effort(t, k, "Widgets"), role(t, k, "Reviewer", "")
	h.Link("a", "root")
	h.Link("b", "root")
	h.SetRoster(agent, []Session{idle("a", "/w", "")})
	must(t, k.Assign("a", e))
	must(t, k.SetRole("a", r))
	h.Touch()

	h = New(k, st)
	h.SetRoster(agent, []Session{idle("b", "/w", "")})
	if s := session(t, h, "b"); s.Effort != e || s.Role != r {
		t.Errorf("b is under %q as %q, want the filing and role of the sibling it took up", s.Effort, s.Role)
	}
}

func TestEverySubagentSeenIsHandedOver(t *testing.T) {
	t.Run("one seen after the last turn", func(t *testing.T) {
		h, k, st := newHub(t)
		at := time.Now().Add(-time.Hour)
		old := said("old", "add flanges", at)
		h.SetRoster(agent, []Session{old})
		old.Subagents = []Subagent{{ID: "late", Type: "explore"}}
		h.SetRoster(agent, []Session{old})

		h = New(k, st)
		h.Link("new", "old")
		h.SetRoster(agent, []Session{said("new", "now washers", at.Add(time.Minute))})
		if s := session(t, h, "new"); !slices.ContainsFunc(s.Subagents, func(a Subagent) bool { return a.ID == "late" }) {
			t.Errorf("new lists %+v, want the subagent old was last seen with", s.Subagents)
		}
		if got := h.SubagentSessions("new", "late"); !slices.Equal(got, []string{"new", "old"}) {
			t.Errorf("its report is looked for under %v, want new then old", got)
		}
	})
	t.Run("more than are retained", func(t *testing.T) {
		h, _, _ := newHub(t)
		at := time.Now().Add(-time.Hour)
		old := said("old", "add flanges", at)
		for i := range retainedSubagentLimit + 1 {
			old.Subagents = append(old.Subagents, Subagent{ID: fmt.Sprintf("c%03d", i), StartedAt: at.Add(time.Duration(i) * time.Second)})
		}
		h.SetRoster(agent, []Session{old})
		h.SetRoster(agent, nil)
		h.Link("new", "old")
		h.SetRoster(agent, []Session{said("new", "now washers", at.Add(time.Hour))})
		if s := session(t, h, "new"); len(s.Subagents) != retainedSubagentLimit+1 {
			t.Errorf("new lists %d subagents, want all %d old had", len(s.Subagents), retainedSubagentLimit+1)
		}
		last := fmt.Sprintf("c%03d", retainedSubagentLimit)
		if got := h.SubagentSessions("new", last); !slices.Equal(got, []string{"new", "old"}) {
			t.Errorf("the last one's report is looked for under %v, want new then old", got)
		}
	})
}
