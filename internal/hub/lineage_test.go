package hub

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/kb"
)

// said is a session in which the user last said prompt, at the given time.
func said(id, prompt string, at time.Time) Session {
	return Session{ID: id, Agent: agent, Status: Idle, Cwd: "/w", StartedAt: at, LastPrompt: prompt, PromptAt: at}
}

// prompts lists what a session's timeline holds, oldest first.
func prompts(s Session) string {
	var out []string
	for _, m := range s.Timeline {
		out = append(out, m.Prompt)
	}
	return strings.Join(out, " | ")
}

func closedIDs(h *Hub) string {
	var out []string
	for _, a := range h.State().Archive {
		out = append(out, a.ID)
	}
	slices.Sort(out)
	return strings.Join(out, ",")
}

func TestAnOpenSessionTakesUpTheClosedOneItContinues(t *testing.T) {
	h, k, st := newHub(t)
	e, r := effort(t, k, "Widgets"), role(t, k, "Reviewer", "")
	began := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	h.SetRoster(agent, []Session{said("old", "add flanges", began)})
	h.Rename("old", "Flange work")
	must(t, k.Assign("old", e))
	must(t, k.SetRole("old", r))
	h.SetInsight("old", h.sessions["old"].key, Insight{Title: "Flanges added", Summary: "It added flanges."})
	queued, _ := h.Enqueue("old", "and washers")

	h.Link("new", "old")
	next := said("new", "now add washers", began.Add(time.Hour))
	h.SetRoster(agent, []Session{next})

	check := func(h *Hub, when string) {
		t.Helper()
		got := h.State()
		if len(got.Sessions) != 1 || len(got.Archive) != 0 {
			t.Fatalf("%s: %d open and %d closed sessions, want the one conversation open and nothing closed", when, len(got.Sessions), len(got.Archive))
		}
		s := got.Sessions[0]
		if s.ID != "new" || s.Label != "Flange work" || s.Effort != e || s.EffortVia != kb.ViaUser || s.Role != r {
			t.Errorf("%s: session %q is %q under %q (%s) as %q; want the new id with the name, effort and role given to the old", when, s.ID, s.Label, s.Effort, s.EffortVia, s.Role)
		}
		if got := prompts(s); got != "add flanges | now add washers" || s.Timeline[0].Line != "Flanges added" {
			t.Errorf("%s: timeline = %q, want what was said under both ids, oldest first, with the model's lines", when, got)
		}
		if s.Insight == nil || s.Insight.Title != "Flanges added" {
			t.Errorf("%s: insight = %+v, want the last written of the conversation until a new one is", when, s.Insight)
		}
		if len(s.Replies) != 1 || s.Replies[0].ID != queued.ID {
			t.Errorf("%s: replies = %+v, want the one queued under the old id", when, s.Replies)
		}
	}
	check(h, "as the id changes")

	h = New(k, st)
	h.SetRoster(agent, []Session{next})
	check(h, "after a restart")
	if got := h.Claim("new"); len(got) != 1 || got[0].Text != "and washers" {
		t.Errorf("claimed %+v, want the reply queued under the old id delivered under the new", got)
	}

	h = New(k, st)
	h.SetRoster(agent, nil)
	if got := closedIDs(h); got != "new" {
		t.Errorf("closed = %q once the conversation ends, want it once, under its last id", got)
	}
}

func TestClosedSessionsOfOneLineageGoToTheOneStillOpen(t *testing.T) {
	h, _, _ := newHub(t)
	at := time.Now().Add(-5 * time.Hour).Truncate(time.Second)
	h.SetRoster(agent, []Session{said("first", "one", at), said("second", "two", at.Add(time.Hour)), said("apart", "elsewhere", at)})
	h.SetRoster(agent, nil)
	if got := closedIDs(h); got != "apart,first,second" {
		t.Fatalf("closed = %s", got)
	}

	// The second and third both continue the first; the fourth continues a
	// session the hub never saw, which itself continues the first.
	h.Link("second", "first")
	h.Link("third", "first")
	h.Link("fourth", "unseen")
	h.Link("unseen", "first")
	h.SetRoster(agent, []Session{said("third", "three", at.Add(2*time.Hour))})
	if got := prompts(session(t, h, "third")); got != "one | two | three" || closedIDs(h) != "apart" {
		t.Errorf("timeline %q with %q closed; want the whole lineage in the open session and the unrelated one left closed", got, closedIDs(h))
	}

	h.SetRoster(agent, []Session{said("fourth", "four", at.Add(3*time.Hour))})
	if got := prompts(session(t, h, "fourth")); got != "one | two | three | four" || closedIDs(h) != "apart" {
		t.Errorf("timeline %q with %q closed; want a session reached through one never seen to take up the lineage", got, closedIDs(h))
	}
}

func TestOpenSessionsOfOneLineageStayApart(t *testing.T) {
	h, _, _ := newHub(t)
	at := time.Now().Add(-5 * time.Hour).Truncate(time.Second)
	h.Link("fork", "trunk")
	h.Link("left", "trunk")
	trunk, fork := said("trunk", "one", at), said("fork", "two", at.Add(time.Hour))
	h.SetRoster(agent, []Session{trunk, fork, said("left", "three", at.Add(2*time.Hour))})
	if a, b := prompts(session(t, h, "trunk")), prompts(session(t, h, "fork")); a != "one" || b != "two" || closedIDs(h) != "" {
		t.Fatalf("timelines %q and %q, closed %q; want sessions that are both open left as they are", a, b, closedIDs(h))
	}

	h.SetRoster(agent, []Session{trunk, fork})
	if a, b := prompts(session(t, h, "trunk")), prompts(session(t, h, "fork")); a != "one" || b != "two | three" || closedIDs(h) != "" {
		t.Errorf("timelines %q and %q, closed %q; want the closed session taken up by the later of the two to start", a, b, closedIDs(h))
	}
}

func TestTheLatestToCloseIsHeededFirst(t *testing.T) {
	h, k, _ := newHub(t)
	older, newer := effort(t, k, "Older"), effort(t, k, "Newer")
	at := time.Now().Add(-5 * time.Hour).Truncate(time.Second)
	// The ids sort against the order the sessions closed in.
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
	h.SetRoster(agent, []Session{said("open", "three", at.Add(2*time.Hour))})
	if s := session(t, h, "open"); s.Label != "Late name" || s.Effort != newer {
		t.Errorf("session is %q under %q, want the name and effort of the latest to close", s.Label, s.Effort)
	}

	// A name given to the open session itself outranks any it takes up.
	h.Rename("open", "Its own name")
	h.Link("stray", "root")
	h.SetRoster(agent, []Session{said("open", "three", at.Add(2*time.Hour)), said("stray", "four", at)})
	h.Rename("stray", "Stray name")
	h.SetRoster(agent, []Session{said("open", "three", at.Add(2*time.Hour))})
	if s := session(t, h, "open"); s.Label != "Its own name" || closedIDs(h) != "" {
		t.Errorf("session is %q with %q closed, want its own name kept as it takes up another", s.Label, closedIDs(h))
	}
}

func TestFilingOutlastsASessionThatLeftNothingElse(t *testing.T) {
	h, k, _ := newHub(t)
	e, r := effort(t, k, "Widgets"), role(t, k, "Reviewer", "")
	h.SetRoster(agent, []Session{idle("silent", "/w", "")})
	must(t, k.Assign("silent", e))
	must(t, k.SetRole("silent", r))
	h.Link("between", "silent")
	h.Link("next", "between")
	h.SetRoster(agent, []Session{idle("next", "/w", "")})
	if s := session(t, h, "next"); s.Effort != e || s.EffortVia != kb.ViaUser || s.Role != r || closedIDs(h) != "" {
		t.Errorf("session under %q (%s) as %q, want the effort and role the user gave the session it continues", s.Effort, s.EffortVia, s.Role)
	}
}

func TestWhatTheModelWroteOfAClosedSessionStandsInForTheOpenOne(t *testing.T) {
	h, k, st := newHub(t)
	e, r := effort(t, k, "Widgets"), role(t, k, "Reviewer", "")
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	h.SetRoster(agent, []Session{said("old", "add flanges", at)})
	key := h.sessions["old"].key
	h.SetInsight("old", key, Insight{Title: "Flanges added", Summary: "It added flanges."})
	h.SetGuess("old", key, e)
	h.SetRoleGuess("old", key, r)
	h.Link("new", "old")
	// The new session has said nothing yet, so nothing else stores it.
	next := []Session{idle("new", "/elsewhere", "")}
	h.SetRoster(agent, next)

	for _, when := range []string{"as the id changes", "after a restart"} {
		s := session(t, h, "new")
		if s.Label != "Flanges added" || s.LabelVia != ViaGuess || s.Effort != e || s.EffortVia != ViaGuess || s.Role != r || s.RoleVia != ViaGuess || prompts(s) != "add flanges" {
			t.Errorf("%s: session %q (%s) under %q (%s) as %q (%s) with timeline %q; want the model's name, filing and role for the old one", when, s.Label, s.LabelVia, s.Effort, s.EffortVia, s.Role, s.RoleVia, prompts(s))
		}
		h = New(k, st)
		h.SetRoster(agent, next)
	}
}

func TestTakingUpAClosedSessionIsAnnounced(t *testing.T) {
	h, _, _ := newHub(t)
	old, open := idle("old", "/w", "done"), idle("open", "/w", "done")
	h.SetRoster(agent, []Session{old, open})
	for _, id := range []string{"old", "open"} {
		h.SetInsight(id, h.sessions[id].key, Insight{Title: "Written of " + id, Summary: "Its own summary."})
	}
	h.SetRoster(agent, []Session{open})
	h.Link("open", "old")
	ticks, stop := h.Subscribe()
	defer stop()

	// The open session has all it would take, so only the closed list changes.
	h.SetRoster(agent, []Session{open})
	select {
	case <-ticks:
	default:
		t.Error("subscribers were not told that a closed session had gone")
	}
	if s := session(t, h, "open"); closedIDs(h) != "" || s.Insight.Title != "Written of open" {
		t.Errorf("closed = %q and insight %q, want the session taken up and the open one's own insight kept", closedIDs(h), s.Insight.Title)
	}
}

func TestLink(t *testing.T) {
	h, k, st := newHub(t)
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	h.SetRoster(agent, []Session{said("a", "one", at), said("b", "two", at), idle("quiet", "/w", "")})
	h.Apply("opencode", "other", func(s *Session) { s.LastPrompt = "elsewhere" })
	h.SetRoster(agent, nil)
	h.Retire("opencode", -time.Second)
	if got := strings.Join(h.Untraced(agent), ","); got != "a,b" {
		t.Errorf("untraced = %q, want the agent's closed sessions that were worth keeping", got)
	}

	if !h.Link("a", "before") {
		t.Error("a link to a session nothing is known of did not ask for it to be traced")
	}
	if h.Link("before", "") || h.Link("b", "b") {
		t.Error("a session that continues none asked for more tracing")
	}
	if h.Link("c", "a") || h.Link("d", "before") {
		t.Error("a link to a session already traced asked for it to be traced again")
	}
	h.Link("a", "elsewhere")
	if got := h.root("c"); got != "before" || h.root("b") != "b" || h.root("unknown") != "unknown" {
		t.Errorf("root of c = %q, want it followed through a to where a was first said to come from", got)
	}
	if got := h.Untraced(agent); len(got) != 0 {
		t.Errorf("untraced = %q after every closed session was linked", got)
	}

	h = New(k, st)
	if got := h.root("c"); got != "before" || len(h.Untraced(agent)) != 0 {
		t.Errorf("after a restart, root of c = %q with %q untraced; want the links kept", got, h.Untraced(agent))
	}

	// Sessions that name each other in a circle are followed only so far.
	h.Link("x", "y")
	h.Link("y", "x")
	if got := h.root("x"); got != "x" && got != "y" {
		t.Errorf("root of a circle = %q", got)
	}
}

func TestJoin(t *testing.T) {
	at := time.Now().Add(-100 * time.Hour).Truncate(time.Second)
	var old, cur []Moment
	for i := range timelineLimit {
		old = append(old, Moment{At: at.Add(time.Duration(2*i) * time.Minute), Prompt: "old " + strconv.Itoa(i)})
	}
	// The last thing said before the id changed is in both timelines.
	carried := Moment{At: old[len(old)-1].At, Prompt: old[len(old)-1].Prompt, Line: "described later"}
	cur = append(cur, carried, Moment{At: at.Add(time.Minute), Prompt: "early"}, Moment{At: at.Add(time.Hour * 99), Prompt: "latest"})

	got := join(old, cur)
	if len(got) != timelineLimit || got[len(got)-1].Prompt != "latest" || got[len(got)-2] != carried {
		t.Errorf("%d moments ending %+v; want the latest %d, with the moment both held kept once", len(got), got[max(0, len(got)-2):], timelineLimit)
	}
	if !slices.IsSortedFunc(got, func(a, b Moment) int { return a.At.Compare(b.At) }) || got[0].Prompt != "old 1" {
		t.Errorf("timeline starts at %q, want it in order of time with the oldest dropped", got[0].Prompt)
	}
}
