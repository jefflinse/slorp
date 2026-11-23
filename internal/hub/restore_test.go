package hub

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/testdb"
)

func TestAReportAboutASessionThatCannotBeReadKeepsWhatWasKept(t *testing.T) {
	first, second := time.Now().Add(-time.Hour).Truncate(time.Second), time.Now().Add(-time.Minute).Truncate(time.Second)
	for _, entry := range []struct {
		name  string
		agent string
		// report tells the hub of a session with a prompt, and reports whether
		// the session is then known.
		report func(h *Hub, id, prompt string, at time.Time) bool
		// end leaves the session behind, to the archive.
		end func(h *Hub)
	}{
		{"a bridge", "opencode",
			func(h *Hub, id, prompt string, at time.Time) bool {
				h.Apply("opencode", id, func(s *Session) { s.LastPrompt, s.PromptAt = prompt, at })
				return h.sessions[id] != nil
			},
			func(h *Hub) { h.Retire("opencode", -time.Second) }},
		{"a roster", agent,
			func(h *Hub, id, prompt string, at time.Time) bool {
				s := idle(id, "/w", "")
				s.LastPrompt, s.PromptAt = prompt, at
				h.SetRoster(agent, []Session{s})
				return h.sessions[id] != nil
			},
			func(h *Hub) { h.SetRoster(agent, nil) }},
	} {
		t.Run(entry.name, func(t *testing.T) {
			st := testdb.Open(t)
			c := &faultyReplies{Store: st}
			h := New(testdb.KB(t, st), c)
			stored := func() sessionDoc {
				t.Helper()
				var d sessionDoc
				if found, err := st.Get("session", "ses_named", &d); err != nil || !found {
					t.Fatalf("ses_named is not saved: %v", err)
				}
				return d
			}

			entry.report(h, "ses_named", "the first turn", first)
			h.Rename("ses_named", "Named by hand")
			entry.end(h)
			before := stored()
			if _, archived := h.archive["ses_named"]; !archived || before.Given != "Named by hand" || len(before.Timeline) != 1 {
				t.Fatalf("ses_named is archived %v as %+v, want named with one turn", archived, before)
			}

			c.unreadable = true
			if entry.report(h, "ses_named", "the second turn", second) {
				t.Error("a session whose record could not be read was admitted")
			}
			c.unreadable = false
			if _, archived := h.archive["ses_named"]; !archived {
				t.Error("a session whose record could not be read left the archive")
			}
			if after := stored(); !reflect.DeepEqual(after, before) {
				t.Errorf("a session whose record could not be read is saved as %+v, want %+v", after, before)
			}

			for range 2 {
				if !entry.report(h, "ses_named", "the second turn", second) {
					t.Fatal("a session whose record can be read again was not admitted")
				}
			}
			want := []string{"the first turn", "the second turn"}
			if s := h.sessions["ses_named"]; s.given != "Named by hand" || !reflect.DeepEqual(turns(s.Timeline), want) {
				t.Errorf("once it could be read, ses_named is named %q with turns %q, want %q with %q", s.given, turns(s.Timeline), "Named by hand", want)
			}
			if after := stored(); after.Given != "Named by hand" || !reflect.DeepEqual(turns(after.Timeline), want) {
				t.Errorf("once it could be read, ses_named is saved named %q with turns %q, want %q with %q", after.Given, turns(after.Timeline), "Named by hand", want)
			}

			if !entry.report(h, "ses_new", "a first turn", second) {
				t.Error("a session never seen before was not admitted")
			}
		})
	}
}

func TestASessionThatCannotBeReadIsNotTakenUpByOneItContinues(t *testing.T) {
	st := testdb.Open(t)
	c := &faultyReplies{Store: st}
	h := New(testdb.KB(t, st), c)
	at := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	h.SetRoster(agent, []Session{said("old", "add flanges", at)})
	h.Rename("old", "Flange work")
	queued, err := h.Enqueue("old", "and washers")
	must(t, err)
	h.SetRoster(agent, nil)
	var before sessionDoc
	if found, err := st.Get("session", "old", &before); err != nil || !found || before.Given != "Flange work" {
		t.Fatalf("old is saved as %+v, %v, %v; want named", before, found, err)
	}

	h.Link("new", "old")
	both := []Session{said("old", "more flanges", at.Add(time.Hour)), said("new", "now washers", at.Add(2*time.Hour))}
	c.unreadableID = "old"
	h.SetRoster(agent, both)
	c.unreadableID = ""
	if _, archived := h.archive["old"]; !archived {
		t.Error("a session reported open whose record could not be read left the archive")
	}
	var after sessionDoc
	if found, err := st.Get("session", "old", &after); err != nil || !found || !reflect.DeepEqual(after, before) {
		t.Errorf("a session reported open whose record could not be read is saved as %+v, %v, %v; want %+v", after, found, err, before)
	}
	var r Reply
	if found, err := st.Get("reply", queued.ID, &r); err != nil || !found || r.SessionID != "old" {
		t.Errorf("the reply queued for it is saved as %+v, %v, %v; want still for old", r, found, err)
	}
	if s := session(t, h, "new"); s.given != "" || prompts(s) != "now washers" {
		t.Errorf("the session continuing it is named %q with %q, want nothing of the one that could not be read", s.given, prompts(s))
	}

	h.SetRoster(agent, both)
	if s := session(t, h, "old"); s.given != "Flange work" || prompts(s) != "add flanges | more flanges" {
		t.Errorf("once it could be read, old is named %q with %q, want its name and both its turns", s.given, prompts(s))
	}
	if got, err := h.Claim("new"); err != nil || len(got) != 0 {
		t.Errorf("the session continuing it claimed %+v, %v; want nothing", got, err)
	}
	if got, err := h.Claim("old"); err != nil || len(got) != 1 || got[0].ID != queued.ID {
		t.Errorf("old claimed %+v, %v; want the reply queued for it", got, err)
	}
}

func TestASessionReportedOpenThatCannotBeReadIsNotClosed(t *testing.T) {
	first, second := time.Now().Add(-time.Hour).Truncate(time.Second), time.Now().Add(-time.Minute).Truncate(time.Second)
	for _, entry := range []struct {
		name, agent string
		report      func(h *Hub)
		// gone has the session go unreported for as long as it takes to be
		// thought closed.
		gone func(h *Hub)
	}{
		{"a bridge", "opencode",
			func(h *Hub) {
				h.Apply("opencode", "ses_open", func(s *Session) { s.LastPrompt, s.PromptAt = "the second turn", second })
				h.Retire("opencode", time.Minute)
			},
			func(h *Hub) {
				h.unread["ses_open"] = unreadReport{agent: "opencode", at: time.Now().Add(-2 * time.Minute)}
				h.Retire("opencode", time.Minute)
			}},
		{"a roster", agent,
			func(h *Hub) {
				s := idle("ses_open", "/w", "")
				s.LastPrompt, s.PromptAt = "the second turn", second
				h.SetRoster(agent, []Session{s})
			},
			func(h *Hub) { h.SetRoster(agent, nil) }},
	} {
		t.Run(entry.name, func(t *testing.T) {
			st := testdb.Open(t)
			k := testdb.KB(t, st)
			c := &faultyReplies{Store: st}
			stored := func() sessionDoc {
				t.Helper()
				var d sessionDoc
				if found, err := st.Get("session", "ses_open", &d); err != nil || !found {
					t.Fatalf("ses_open is not saved: %v", err)
				}
				return d
			}
			restarted := func() *Hub {
				h := New(k, c)
				h.started = time.Now().Add(-time.Hour)
				return h
			}
			h := New(k, st)
			if entry.agent == agent {
				s := idle("ses_open", "/w", "")
				s.LastPrompt, s.PromptAt = "the first turn", first
				h.SetRoster(agent, []Session{s})
			} else {
				h.Apply("opencode", "ses_open", func(s *Session) { s.LastPrompt, s.PromptAt = "the first turn", first })
			}
			h.Rename("ses_open", "Named by hand")
			before := stored()
			if !before.Ended.IsZero() || before.Given != "Named by hand" {
				t.Fatalf("ses_open is saved as %+v, want open and named", before)
			}

			unread := func(h *Hub) {
				t.Helper()
				c.unreadableID = "ses_open"
				entry.report(h)
				c.unreadableID = ""
				if after := stored(); !reflect.DeepEqual(after, before) {
					t.Errorf("a session reported open whose record could not be read is saved as %+v, want %+v", after, before)
				}
				if _, archived := h.archive["ses_open"]; archived {
					t.Error("a session reported open whose record could not be read was archived")
				}
			}

			h = restarted()
			unread(h)
			entry.report(h)
			if s := session(t, h, "ses_open"); s.given != "Named by hand" || prompts(s) != "the first turn | the second turn" {
				t.Errorf("once it could be read, ses_open is named %q with %q, want its name and both turns", s.given, prompts(s))
			}

			before = stored()
			h = restarted()
			unread(h)
			entry.gone(h)
			if _, archived := h.archive["ses_open"]; !archived || stored().Ended.IsZero() {
				t.Error("a session no longer reported was not closed")
			}
		})
	}
}

func TestARunDoesNotTakeOverASessionReportedOpenThatCannotBeRead(t *testing.T) {
	st := testdb.Open(t)
	c := &faultyReplies{Store: st}
	h := New(testdb.KB(t, st), c)
	held := func(id string) Reply {
		t.Helper()
		h.Apply("opencode", id, func(s *Session) { s.LastPrompt, s.PromptAt = "in a terminal", time.Now() })
		r, err := h.Enqueue(id, "for the terminal")
		must(t, err)
		h.Retire("opencode", -time.Second)
		c.unreadableID = id
		h.Apply("opencode", id, nil)
		c.unreadableID = ""
		if !h.HeldOpen(id) {
			t.Fatalf("%s is not held open", id)
		}
		return r
	}

	queued := held("ses_held")
	c.batches = 0
	if err := h.OpenManaged("opencode", "ses_held", "job", "token-held", nil); !errors.Is(err, ErrLifecycleConflict) {
		t.Fatalf("taking over a session reported open elsewhere = %v, want ErrLifecycleConflict", err)
	}
	if c.batches != 0 {
		t.Errorf("a refused takeover wrote %d batches, want none", c.batches)
	}
	var r Reply
	if found, err := st.Get("reply", queued.ID, &r); err != nil || !found || r.Owner != OwnerBridge || r.State != Queued {
		t.Errorf("the reply for it is saved as %+v, %v, %v; want still queued for the terminal", r, found, err)
	}
	h.Apply("opencode", "ses_held", nil)
	if got, err := h.Claim("ses_held"); err != nil || len(got) != 1 || got[0].ID != queued.ID {
		t.Errorf("the terminal's next report claimed %+v, %v; want its reply", got, err)
	}

	held("ses_silent")
	h.Retire("opencode", -time.Second)
	if h.HeldOpen("ses_silent") {
		t.Fatal("a session whose terminal fell silent is still held open")
	}
	must(t, h.OpenManaged("opencode", "ses_silent", "job", "token-silent", nil))
}

func turns(timeline []Moment) []string {
	var out []string
	for _, m := range timeline {
		out = append(out, m.Prompt)
	}
	return out
}
