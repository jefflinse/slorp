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

func TestPast(t *testing.T) {
	h, k, st := newHub(t)
	a, err := k.Create("A", "")
	must(t, err)
	b, err := k.Create("B", "")
	must(t, err)
	must(t, k.AddDir(a.ID, "/work/a"))
	must(t, k.AddDir(b.ID, "/work/b"))
	closing := func(id, cwd string, used Tokens, cost float64) {
		t.Helper()
		open := append(h.Sessions(), Session{ID: id, Agent: agent, Status: Idle, Cwd: cwd, LastPrompt: "go", PromptAt: time.Now(), Used: used, CostUSD: cost})
		h.SetRoster(agent, open)
		h.SetRoster(agent, open[:len(open)-1])
	}
	closing("a1", "/work/a", Tokens{In: 100, CacheWrite: 10, CacheRead: 1000, Out: 20}, 1.25)
	closing("b1", "/work/b", Tokens{In: 7, Out: 7}, 9)
	closing("a2", "/work/a", Tokens{In: 1, CacheWrite: 2, CacheRead: 3, Out: 4}, 0.5)
	h.wrapups["2026-11-16"] = Wrapup{Day: "2026-11-16", State: WrapupReady, Efforts: []WrapupEffort{{ID: a.ID, Name: "A", Summary: "Began."}}}
	h.wrapups["2026-11-17"] = Wrapup{Day: "2026-11-17", State: WrapupReady, Efforts: []WrapupEffort{{ID: b.ID, Name: "B", Summary: "Elsewhere."}}}
	h.wrapups["2026-11-18"] = Wrapup{Day: "2026-11-18", State: WrapupReady, Efforts: []WrapupEffort{{ID: b.ID, Name: "B"}, {ID: a.ID, Name: "A", Summary: "Finished.", Points: []WrapupPoint{{Text: "Did it"}}}}}

	check := func(h *Hub) {
		t.Helper()
		p, ok := h.Past(a.ID)
		if !ok {
			t.Fatal("Past found no such effort")
		}
		if len(p.Closed) != 2 || p.ClosedCount != 2 || p.Closed[0].ID != "a2" || p.Closed[1].ID != "a1" || p.Closed[0].Timeline != nil || p.Closed[1].CostUSD != 1.25 {
			t.Errorf("closed = %+v (%d), want the effort's two closed sessions, the latest first, without their timelines", p.Closed, p.ClosedCount)
		}
		if want := (Tokens{In: 101, CacheWrite: 12, CacheRead: 1003, Out: 24}); p.Used != want || p.CostUSD != 1.75 {
			t.Errorf("used %+v costing %v, want %+v costing 1.75", p.Used, p.CostUSD, want)
		}
	}
	check(h)
	if p, _ := h.Past(a.ID); len(p.Days) != 2 || p.Days[0].Day != "2026-11-18" || p.Days[0].Summary != "Finished." || len(p.Days[0].Points) != 1 || p.Days[1].Day != "2026-11-16" {
		t.Errorf("days = %+v, want the effort's part of the two days it moved on, the latest first", p.Days)
	}
	check(New(k, st))

	if p, ok := h.Past(b.ID); !ok || p.ClosedCount != 1 || p.CostUSD != 9 || len(p.Days) != 2 {
		t.Errorf("the other effort's past = %+v", p)
	}
	if _, ok := h.Past("nope"); ok {
		t.Error("Past answered for an effort there is not")
	}
	empty, err := k.Create("C", "")
	must(t, err)
	if p, ok := h.Past(empty.ID); !ok || p.Days == nil || p.Closed == nil || len(p.Days)+len(p.Closed) != 0 {
		t.Errorf("an effort with nothing behind it: %+v, %v; want empty lists", p, ok)
	}
}

func TestMergeEfforts(t *testing.T) {
	h, k, st := newHub(t)
	a, err := k.Create("A", "")
	must(t, err)
	b, err := k.Create("B", "")
	must(t, err)
	must(t, k.AddDir(a.ID, "/work/a"))
	gone := Session{ID: "gone", Agent: agent, Status: Idle, Cwd: "/work/a", LastPrompt: "go", PromptAt: time.Now(), CostUSD: 2}
	h.SetRoster(agent, []Session{gone, idle("guessed", "/w", "done"), idle("ruled", "/work/a", "done")})
	describe(h, "guessed", KindReview)
	h.SetGuess("guessed", h.Unrouted()[0].Key, a.ID)
	h.SetRoster(agent, []Session{idle("guessed", "/w", "done"), idle("ruled", "/work/a", "done")})
	h.wrapups["2026-11-17"] = Wrapup{Day: "2026-11-17", State: WrapupReady, Efforts: []WrapupEffort{{ID: a.ID, Name: "A", Summary: "As A.", Points: []WrapupPoint{{Text: "one"}}},
		{ID: b.ID, Name: "B", Summary: "As B.", Points: []WrapupPoint{{Text: "two"}}}}}
	h.notes[a.ID] = note{Key: "k", Note: Note{Line: "old"}}

	must(t, h.MergeEfforts(a.ID, b.ID))

	for _, id := range []string{"guessed", "ruled"} {
		if s := session(t, h, id); s.Effort != b.ID {
			t.Errorf("open session %s is filed under %q, want the effort merged into", id, s.Effort)
		}
	}
	check := func(h *Hub) {
		t.Helper()
		p, ok := h.Past(b.ID)
		if !ok || p.ClosedCount != 1 || p.Closed[0].ID != "gone" || p.CostUSD != 2 || len(p.Days) != 1 || p.Days[0].Summary != "As A. As B." || len(p.Days[0].Points) != 2 {
			t.Errorf("the past of the effort merged into = %+v, want the other's closed session, and one part for the day both moved on", p)
		}
		if st := h.State(); len(st.Efforts) != 1 || st.Efforts[0].ID != b.ID || st.Efforts[0].ActiveAt.IsZero() {
			t.Errorf("efforts = %+v, want the one merged into, active when its sessions were", st.Efforts)
		}
	}
	check(h)
	check(New(k, st))
	if _, kept := h.notes[a.ID]; kept {
		t.Error("the note on the effort that is gone was kept")
	}
	if h.MergeEfforts("nope", b.ID) == nil || h.MergeEfforts(b.ID, b.ID) == nil {
		t.Error("a merge with an unknown effort or with itself was accepted")
	}
}
