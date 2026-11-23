package hub

import (
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

func turns(timeline []Moment) []string {
	var out []string
	for _, m := range timeline {
		out = append(out, m.Prompt)
	}
	return out
}
