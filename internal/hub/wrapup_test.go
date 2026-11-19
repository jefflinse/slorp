package hub

import (
	"reflect"
	"strconv"
	"testing"
	"time"
)

func TestWrapup(t *testing.T) {
	h, k, st := newHub(t)
	e, err := k.Create("Widgets", "the widget service")
	if err != nil {
		t.Fatal(err)
	}
	if err := k.AddDir(e.ID, "/work/widgets"); err != nil {
		t.Fatal(err)
	}
	y, m, d := time.Now().Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, time.Local)
	day := today.Format(dayLayout)
	said := func(id, cwd, title, prompt string, at time.Time, others ...Session) {
		h.SetRoster(agent, append([]Session{{ID: id, Agent: agent, Status: Idle, Cwd: cwd, Title: title, LastPrompt: prompt, PromptAt: at}}, others...))
	}

	entry := func(h *Hub) Wrapup {
		w, _ := h.Wrapup(day)
		return w
	}

	if h.AskWrapup("yesterday") || h.AskWrapup("2026-13-40") {
		t.Fatal("AskWrapup took something that names no day")
	}
	if _, ok := h.Wrapup(day); ok {
		t.Fatal("a day nobody asked for has an entry")
	}
	if !h.AskWrapup(day) || entry(h).State != WrapupEmpty || len(h.Unwrapped()) != 0 {
		t.Fatalf("a day with nothing said: %+v, waiting %d; want it empty and not waiting", entry(h), len(h.Unwrapped()))
	}

	// One session that has gone by the time the day is wrapped up, one in an
	// effort and one filed nowhere.
	said("gone", "/work/widgets", "Gone by evening", "fix the flange", today.Add(9*time.Hour))
	said("open", "/work/widgets", "Still open", "said the day before", today.Add(-time.Hour))
	said("open", "/work/widgets", "Still open", "add a test", today.Add(8*time.Hour))
	h.SetInsight("open", h.sessions["open"].key, Insight{Title: "Test added"})
	said("open", "/work/widgets", "Still open", "ship it", today.Add(10*time.Hour))
	loose := Session{ID: "loose", Agent: agent, Status: Idle, Cwd: "/elsewhere", Title: "Loose end", LastPrompt: "tidy up", PromptAt: today.Add(11 * time.Hour),
		Compacted: &Compaction{At: today.Add(11*time.Hour + time.Minute)}}
	said("open", "/work/widgets", "Still open", "ship it", today.Add(10*time.Hour), loose)
	busy := Session{ID: "busy", Agent: agent, Status: Idle, Cwd: "/elsewhere", Title: "Busy"}
	for i := range wrapSessionMoments + 3 {
		busy.LastPrompt, busy.PromptAt = "step "+strconv.Itoa(i), today.Add(12*time.Hour+time.Duration(i)*time.Minute)
		said("open", "/work/widgets", "Still open", "ship it", today.Add(10*time.Hour), loose, busy)
	}

	if !h.AskWrapup(day) {
		t.Fatal("AskWrapup refused today")
	}
	if w := entry(h); w.State != WrapupPending || w.Moments != 1+2+1+wrapSessionMoments {
		t.Fatalf("asked for: %+v; want it pending, counting the day's moments and no other day's", w)
	}
	logs := h.Unwrapped()
	if len(logs) != 1 || !logs[0].Day.Equal(today) || len(logs[0].Efforts) != 2 {
		t.Fatalf("waiting = %+v, want today's log with the effort and the unsorted", logs)
	}
	widgets, unsorted := logs[0].Efforts[0], logs[0].Efforts[1]
	if widgets.ID != e.ID || widgets.Name != "Widgets" || widgets.About != "the widget service" || len(widgets.Sessions) != 2 {
		t.Fatalf("effort = %+v, want Widgets with its two sessions", widgets)
	}
	if s := widgets.Sessions[0]; s.Label != "Still open" || len(s.Moments) != 2 || s.Moments[0].Prompt != "add a test" || s.Moments[0].Line != "Test added" || s.Moments[1].Prompt != "ship it" {
		t.Errorf("first session = %+v; want the open one, which spoke first, with today's two moments and the model's line", s)
	}
	if s := widgets.Sessions[1]; s.Label != "Gone by evening" || len(s.Moments) != 1 || s.Moments[0].Prompt != "fix the flange" {
		t.Errorf("second session = %+v; want the one that has gone, under the name and effort it had", s)
	}
	if unsorted.ID != "unsorted" || unsorted.Name != "Unsorted" || len(unsorted.Sessions) != 2 || unsorted.Sessions[0].Label != "Loose end" {
		t.Fatalf("unsorted = %+v, want the two sessions filed nowhere, in the order they first spoke", unsorted)
	}
	if got := unsorted.Sessions[1].Moments; len(got) != wrapSessionMoments || got[0].Prompt != "step 3" {
		t.Errorf("a busy session gave %d moments from %q, want its latest %d", len(got), got[0].Prompt, wrapSessionMoments)
	}

	stale := logs[0]
	if !h.AskWrapup(day) {
		t.Fatal("AskWrapup refused a second asking")
	}
	h.SetWrapup(stale, "Written for the first asking", nil, nil, "")
	if w := entry(h); w.State != WrapupPending {
		t.Fatalf("an answer to an earlier asking was taken: %+v", w)
	}
	wrote := []WrapupEffort{{ID: e.ID, Name: "Widgets", Summary: "Moved.", Points: []WrapupPoint{{Text: "Added a test", Sessions: []string{"open"}}}, Open: []string{}}}
	h.SetWrapup(h.Unwrapped()[0], "A good day", wrote, nil, "")
	check := func(h *Hub, when string) {
		t.Helper()
		w := entry(h)
		if w.State != WrapupReady || w.Headline != "A good day" || len(w.Efforts) != 1 || !reflect.DeepEqual(w.Efforts[0].Points, wrote[0].Points) || time.Since(w.At) > time.Minute {
			t.Errorf("%s: %+v, want what the model wrote", when, w)
		}
		if len(h.Unwrapped()) != 0 {
			t.Errorf("%s: a day already written is still waiting", when)
		}
	}
	check(h, "once written")
	check(New(k, st), "after a restart")

	// A session filed after it last spoke keeps that filing when it goes,
	// and one whose effort has gone comes under the unsorted.
	other, err := k.Create("Gadgets", "")
	if err != nil {
		t.Fatal(err)
	}
	moved := Session{ID: "moved", Agent: agent, Status: Idle, Cwd: "/elsewhere", Title: "Moved late", LastPrompt: "one more", PromptAt: today.Add(13 * time.Hour)}
	h.SetRoster(agent, []Session{moved})
	if err := k.Assign("moved", other.ID); err != nil {
		t.Fatal(err)
	}
	h.SetRoster(agent, nil)
	h.AskWrapup(day)
	if got := h.Unwrapped()[0].Efforts; len(got) != 3 || got[1].ID != other.ID || len(got[1].Sessions) != 1 || got[1].Sessions[0].Label != "Moved late" {
		t.Fatalf("efforts = %+v; want the session that was filed late under Gadgets", got)
	}
	if err := k.Delete(other.ID); err != nil {
		t.Fatal(err)
	}
	if got := h.Unwrapped()[0].Efforts; len(got) != 2 || got[1].ID != "unsorted" || len(got[1].Sessions) != 3 {
		t.Fatalf("efforts = %+v; want the session whose effort has gone among the unsorted", got)
	}

	if w := entry(h); w.State != WrapupPending || w.Headline != "A good day" || len(w.Efforts) != 1 {
		t.Errorf("while being written again: %+v; want what was written before to stand", w)
	}
	h.SetWrapup(h.Unwrapped()[0], "", nil, nil, "The model fell over.")
	if w := entry(h); w.State != WrapupFailed || w.Error != "The model fell over." || w.Headline != "A good day" || len(h.Unwrapped()) != 0 {
		t.Errorf("after a failure: %+v; want it failed, with why, no longer waiting, and what was written before still there", w)
	}
	h.AskWrapup(day)
	if w := entry(h); w.State != WrapupPending || w.Error != "" {
		t.Errorf("asked again after a failure: %+v; want it waiting, with the failure forgotten", w)
	}
	h.SetWrapup(h.Unwrapped()[0], "Second time lucky", wrote, nil, "")
	if w := entry(h); w.State != WrapupReady || w.Headline != "Second time lucky" {
		t.Errorf("written again: %+v; want the new account in place of the old", w)
	}
}

func TestAPointStoredAsItsTextAlone(t *testing.T) {
	h, k, st := newHub(t)
	must(t, st.Put("wrapup", "2026-11-16", map[string]any{"day": "2026-11-16", "state": WrapupReady,
		"efforts": []map[string]any{{"id": "unsorted", "name": "Unsorted", "points": []string{"Added the retry"}}}}))
	h = New(k, st)
	w, ok := h.Wrapup("2026-11-16")
	if !ok || len(w.Efforts) != 1 || !reflect.DeepEqual(w.Efforts[0].Points, []WrapupPoint{{Text: "Added the retry"}}) {
		t.Errorf("an entry stored with its points as text = %+v, %v; want each read as a point with that text", w, ok)
	}
}
