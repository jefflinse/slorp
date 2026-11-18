package hub

import (
	"strconv"
	"testing"
	"time"
)

func TestArchive(t *testing.T) {
	h, k, st := newHub(t)
	e, err := k.Create("Widgets", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := k.AddDir(e.ID, "/work/widgets"); err != nil {
		t.Fatal(err)
	}
	began, said := time.Now().Add(-3*time.Hour).Truncate(time.Second), time.Now().Add(-2*time.Hour).Truncate(time.Second)
	worked := Session{ID: "worked-0123", Agent: agent, Status: Idle, Cwd: "/work/widgets", Branch: "feat/flange", Title: "Flange support",
		StartedAt: began, LastPrompt: "add flange support", PromptAt: said}
	silent := Session{ID: "silent", Agent: agent, Status: Idle, Cwd: "/work/widgets", Title: "Never used"}
	listed := func(h *Hub) []Archived { return h.State().Archive }

	h.SetRoster(agent, []Session{worked, silent})
	h.SetInsight(worked.ID, h.sessions[worked.ID].key, Insight{Title: "Flanges added", Summary: "It added flanges."})
	if got := listed(h); got == nil || len(got) != 0 {
		t.Fatalf("archive = %#v while every session is open, want an empty list", got)
	}

	h.SetRoster(agent, nil)
	got := listed(h)
	if len(got) != 1 {
		t.Fatalf("archive = %+v, want the session that was used and not the one that never was", got)
	}
	a := got[0]
	if a.ID != worked.ID || a.Agent != agent || a.Label != "Flange support" || a.Effort != e.ID || a.Cwd != "/work/widgets" || a.Branch != "feat/flange" ||
		!a.StartedAt.Equal(began) || time.Since(a.EndedAt) > time.Minute || a.Title != "Flanges added" || a.Summary != "It added flanges." || a.Moments != 1 || a.Timeline != nil {
		t.Errorf("archived = %+v, want it as it stood when it closed, without its timeline", a)
	}
	full, ok := h.Archive(worked.ID)
	if _, missing := h.Archive("silent"); !ok || missing || len(full.Timeline) != 1 || full.Timeline[0].Prompt != "add flange support" || full.Timeline[0].Line != "Flanges added" {
		t.Errorf("Archive = %+v, %v; want the closed session with its timeline, and nothing for one that is not archived", full, ok)
	}

	if got := listed(New(k, st)); len(got) != 1 || got[0].ID != worked.ID || got[0].Label != "Flange support" {
		t.Errorf("after a restart, archive = %+v", got)
	}

	// A session that comes back is open again, with what it had.
	h.SetRoster(agent, []Session{worked})
	if back := session(t, h, worked.ID); len(listed(h)) != 0 || len(back.Timeline) != 1 || back.Insight == nil {
		t.Fatalf("a returning session: archive %+v, session %+v; want it out of the archive and whole", listed(h), back)
	}
	if got := listed(New(k, st)); len(got) != 0 {
		t.Errorf("after a restart, a session that had come back is archived again: %+v", got)
	}

	h.SetRoster(agent, nil)
	if got := listed(h); len(got) != 1 || got[0].Effort != e.ID {
		t.Fatalf("archive = %+v, want the session closed again under its effort", got)
	}
	if err := k.Delete(e.ID); err != nil {
		t.Fatal(err)
	}
	if got := listed(h); len(got) != 1 || got[0].Effort != "" {
		t.Errorf("archive = %+v, want the session unfiled now that its effort has gone", got)
	}
}

func TestArchiveTakesInWhatClosedWhileSlorpWasStopped(t *testing.T) {
	h, k, st := newHub(t)
	last := time.Now().Add(-5 * time.Hour).Truncate(time.Second)
	open := func(id, agent string) Session {
		return Session{ID: id, Agent: agent, Status: Idle, Title: "Session " + id, LastPrompt: "do it", PromptAt: last}
	}
	h.SetRoster(agent, []Session{open("stays", agent), open("goes", agent)})
	h.SetRoster("opencode", []Session{open("other", "opencode")})

	h2 := New(k, st)
	if got := h2.State().Archive; len(got) != 0 {
		t.Fatalf("archive = %+v before any agent has said what is open", got)
	}
	h2.SetRoster(agent, []Session{open("stays", agent)})
	got := h2.State().Archive
	if len(got) != 1 || got[0].ID != "goes" || !got[0].EndedAt.Equal(last) {
		t.Fatalf("archive = %+v; want the one session its agent no longer lists, closed as of the last thing known of it", got)
	}

	h2.Retire("opencode", time.Hour)
	if got := h2.State().Archive; len(got) != 1 {
		t.Errorf("archive = %+v; a session was given up on before its bridge had time to be heard from", got)
	}
	h2.started = time.Now().Add(-2 * time.Hour)
	h2.Retire("opencode", time.Hour)
	if got := h2.State().Archive; len(got) != 2 {
		t.Errorf("archive = %+v, want the other agent's session too once it has had its time", got)
	}
	if got := New(k, st).State().Archive; len(got) != 2 {
		t.Errorf("after another restart, archive = %+v", got)
	}
}

func TestArchiveListsTheLatestToCloseFirst(t *testing.T) {
	h, _, _ := newHub(t)
	for i := range archiveShown + 3 {
		id := "s" + strconv.Itoa(1000+i)
		h.archive[id] = sessionDoc{Label: id, Ended: time.Unix(int64(1_700_000_000+i), 0), Timeline: []Moment{{Prompt: "x"}}}
	}
	got := h.State().Archive
	if len(got) != archiveShown || got[0].ID != "s"+strconv.Itoa(1000+archiveShown+2) || got[archiveShown-1].ID != "s1003" {
		t.Errorf("listed %d from %q to %q, want the latest %d to close, newest first", len(got), got[0].ID, got[len(got)-1].ID, archiveShown)
	}
}

func TestRetiredSessionsAreArchived(t *testing.T) {
	h, _, _ := newHub(t)
	h.Apply("opencode", "o", func(s *Session) { s.LastPrompt, s.PromptAt = "do it", time.Now() })
	h.Retire("opencode", time.Hour)
	if got := h.State().Archive; len(got) != 0 {
		t.Fatalf("archive = %+v while the session's bridge is still heard from", got)
	}
	h.sessions["o"].bridgeSeen = time.Now().Add(-2 * time.Hour)
	h.Retire("opencode", time.Hour)
	if got := h.State().Archive; len(got) != 1 || got[0].ID != "o" || got[0].Agent != "opencode" {
		t.Errorf("archive = %+v, want the session whose bridge went quiet", got)
	}
}
