package hub

import (
	"strconv"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/integrations"
	"github.com/jefflinse/slorp/internal/kb"
)

func calls(list []Named) string {
	out := ""
	for _, n := range list {
		out += n.ID + "=" + n.Name + " "
	}
	return out
}

// widgets is a hub with an effort and a workspace over /w/widgets, a session
// working there on the branch of acme/widgets#7, and one working elsewhere.
func widgets(t *testing.T) (h *Hub, effortID, workspaceID string) {
	t.Helper()
	h, k, _ := newHub(t)
	effortID = effort(t, k, "Widgets")
	if err := k.AddDir(effortID, "/w/widgets"); err != nil {
		t.Fatal(err)
	}
	if err := k.Add(kb.SectionWorkspaces, kb.Entry{Name: "Widgets checkout", Dir: "/w/widgets"}); err != nil {
		t.Fatal(err)
	}
	if err := k.Add(kb.SectionWorkspaces, kb.Entry{Name: "Everything", Dir: "/w"}); err != nil {
		t.Fatal(err)
	}
	for _, w := range k.Knowledge().Workspaces {
		if w.Dir == "/w/widgets" {
			workspaceID = w.ID
		}
	}
	h.SetRoster("claude", []Session{
		{ID: "on-pull", Agent: "claude", Title: "Retry the upload", Cwd: "/w/widgets/api", Branch: "retry"},
		{ID: "elsewhere", Agent: "claude", Title: "Tidy the notes", Cwd: "/notes"}})
	h.SetCheckouts(map[string][]string{"/w/widgets/api": {"acme/widgets"}})
	p := pull("acme/widgets", 7, false, time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC))
	p.Title, p.URL, p.Branch, p.Base = "Retry uploads", "https://example.com/7", "retry", "main"
	h.SetFeed("github", nil, []integrations.PullRequest{p}, nil)
	return h, effortID, workspaceID
}

func TestAnActionOnASessionBringsWhatTheSessionGoesWith(t *testing.T) {
	h, effortID, workspaceID := widgets(t)
	if got := h.State().Actions; got == nil || len(got) != 0 {
		t.Fatalf("Actions = %v before the model has done anything, want an empty list", got)
	}
	h.Acted(Action{Kind: ActDescribe, Outcome: "Wants a decision", Sessions: []Named{{ID: "on-pull"}}})
	h.Acted(Action{Kind: ActDescribe, Sessions: []Named{{ID: "elsewhere"}, {ID: "long-gone-session", Name: "Called this"}, {ID: "never-seen-session"}}})

	got := h.State().Actions
	if len(got) != 2 {
		t.Fatalf("%d actions, want two", len(got))
	}
	a := got[1]
	if a.ID == "" || a.Outcome != "Wants a decision" {
		t.Errorf("action = %+v; want an ID of its own and the outcome it was given", a)
	}
	if calls(a.Sessions) != "on-pull=Retry the upload " {
		t.Errorf("sessions = %q; want the session by the name it goes by", calls(a.Sessions))
	}
	if calls(a.Efforts) != effortID+"=Widgets " {
		t.Errorf("efforts = %q; want the effort the session is filed under", calls(a.Efforts))
	}
	if len(a.Pulls) != 1 || a.Pulls[0] != (WrapupPull{Key: "acme/widgets#7", Title: "Retry uploads", URL: "https://example.com/7"}) {
		t.Errorf("pulls = %+v; want the pull request made from the session's branch", a.Pulls)
	}
	if calls(a.Workspaces) != workspaceID+"=Widgets checkout " {
		t.Errorf("workspaces = %q; want only the deepest workspace the session is in", calls(a.Workspaces))
	}
	b := got[0]
	if calls(b.Sessions) != "elsewhere=Tidy the notes long-gone-session=Called this never-seen-session=never-se " {
		t.Errorf("sessions = %q; want a session that is not known kept by the name given, or else by the start of its ID", calls(b.Sessions))
	}
	if len(b.Efforts)+len(b.Pulls)+len(b.Workspaces) != 0 {
		t.Errorf("action = %+v; want nothing more for sessions that go with nothing", b)
	}
}

func TestAnActionOnAPullRequestBringsItsEffortAndSessions(t *testing.T) {
	h, effortID, _ := widgets(t)
	h.Acted(Action{Kind: ActBrief, Pulls: []WrapupPull{{Key: "acme/widgets#7"}}})
	h.Acted(Action{Kind: ActWrapup, Day: "2026-11-20", Pulls: []WrapupPull{{Key: "acme/widgets#7"}}, Sessions: []Named{{ID: "elsewhere"}},
		Efforts: []Named{{ID: effortID}, {ID: "unsorted"}, {ID: "gone", Name: "Since deleted"}, {ID: effortID}}})

	got := h.State().Actions
	brief := got[1]
	if len(brief.Pulls) != 1 || brief.Pulls[0].Title != "Retry uploads" || brief.Pulls[0].URL != "https://example.com/7" {
		t.Errorf("pulls = %+v; want the pull request with its title and address filled in", brief.Pulls)
	}
	if calls(brief.Sessions) != "on-pull=Retry the upload " || calls(brief.Efforts) != effortID+"=Widgets " || len(brief.Workspaces) != 1 {
		t.Errorf("action = %+v; want the session working on the pull request, with its effort and workspace", brief)
	}
	wrapup := got[0]
	if calls(wrapup.Sessions) != "elsewhere=Tidy the notes " {
		t.Errorf("sessions = %q; want only the sessions named, when any are", calls(wrapup.Sessions))
	}
	if calls(wrapup.Efforts) != effortID+"=Widgets gone=Since deleted " {
		t.Errorf("efforts = %q; want each effort once by its name, and none that is neither known nor named", calls(wrapup.Efforts))
	}
	if wrapup.Day != "2026-11-20" {
		t.Errorf("day = %q", wrapup.Day)
	}
}

func TestWorkThatKeepsFailingIsListedOnce(t *testing.T) {
	h, k, st := newHub(t)
	on := func(id string) []Named { return []Named{{ID: id, Name: id}} }
	h.Acted(Action{Kind: ActDescribe, Sessions: on("a"), Error: "no answer"})
	h.Acted(Action{Kind: ActDescribe, Sessions: on("b"), Error: "no answer"})
	h.Acted(Action{Kind: ActFile, Sessions: on("a"), Error: "no answer"})
	h.Acted(Action{Kind: ActDescribe, Sessions: on("a"), Error: "no answer"})
	h.Acted(Action{Kind: ActDescribe, Sessions: on("a"), Error: "still no answer"})

	for _, h := range []*Hub{h, New(k, st)} {
		got := h.Actions()
		if len(got) != 3 || got[0].Kind != ActDescribe || got[0].Tries != 3 || got[0].Error != "still no answer" || got[1].Kind != ActFile || got[2].Sessions[0].ID != "b" || got[1].Tries+got[2].Tries != 0 {
			t.Fatalf("actions = %+v; want the session's description listed once, as failed three times, and the other work left alone", got)
		}
	}
	h.Acted(Action{Kind: ActDescribe, Sessions: on("a"), Outcome: "Described at last"})
	h.Acted(Action{Kind: ActDescribe, Sessions: on("a"), Error: "no answer"})
	if got := h.Actions(); len(got) != 5 || got[0].Tries != 0 || got[1].Outcome != "Described at last" || got[2].Tries != 3 {
		t.Errorf("actions = %+v; want a failure after the work went well to stand on its own", got)
	}
}

func TestActionsOutliveARestartUpToALimit(t *testing.T) {
	h, k, st := newHub(t)
	began := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	for i := range keptActions + 3 {
		h.Acted(Action{Kind: ActNote, At: began.Add(time.Duration(i) * time.Minute), Outcome: strconv.Itoa(i)})
	}
	for _, h := range []*Hub{h, New(k, st)} {
		all := h.Actions()
		if len(all) != keptActions || all[0].Outcome != strconv.Itoa(keptActions+2) || all[len(all)-1].Outcome != "3" {
			t.Fatalf("kept %d actions, from %q back to %q; want the latest %d, the latest first", len(all), all[0].Outcome, all[len(all)-1].Outcome, keptActions)
		}
		recent := h.State().Actions
		if len(recent) != recentActions || recent[0].ID != all[0].ID || recent[recentActions-1].ID != all[recentActions-1].ID {
			t.Errorf("the state carries %d actions; want the latest %d, the latest first", len(recent), recentActions)
		}
	}
	stored := 0
	st.Each("action", func(string, []byte) { stored++ })
	if stored != keptActions {
		t.Errorf("%d actions are stored, want no more than the %d kept", stored, keptActions)
	}
}
