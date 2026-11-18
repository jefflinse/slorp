package hub

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSearch(t *testing.T) {
	h, _, _ := newHub(t)
	now := time.Now()
	say := func(id, title, prompt string, at time.Time, reply string) {
		h.SetRoster(agent, []Session{{ID: id, Agent: agent, Status: Idle, Title: title, LastPrompt: prompt, PromptAt: at, LastReply: reply, ActiveAt: at}})
	}
	// A session that closes with one moment, then one that stays open with two.
	say("docs", "Docs refresh", "update the install page to Go 1.27", now.Add(-5*time.Hour), "")
	say("flange", "Flange support", "add flange support", now.Add(-3*time.Hour), "")
	h.SetInsight("flange", h.sessions["flange"].key, Insight{Title: "Flanges added", Summary: "It added the flange type and its tests pass."})
	say("flange", "Flange support", "now bump the Postgres driver"+strings.Repeat(" and so on", 30)+" to the tail-end", now.Add(-2*time.Hour), "Bumped pgx to v5.9 and reran the suite.")

	where := func(hits []Hit) string {
		var out []string
		for _, x := range hits {
			out = append(out, x.SessionID+":"+x.Where)
		}
		return strings.Join(out, " ")
	}
	tests := []struct{ name, query, want string }{
		{"a word in the name, the summary and a moment", "FLANGE", "flange:name flange:summary flange:moment"},
		{"two words in one moment, in any order and case", "driver POSTGRES", "flange:moment"},
		{"words that are never in one place together", "flange postgres", ""},
		{"a word only the last reply has", "pgx", "flange:reply"},
		{"a word the latest moment and the last reply have", "bump", "flange:moment flange:reply"},
		{"a word only the last prompt has, past where its moment was cut", "tail-end", "flange:prompt"},
		{"a closed session's moment", "install go", "docs:moment"},
		{"a closed session's name", "refresh", "docs:name"},
		{"nothing", "zebra", ""},
		{"no words", "   ", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := h.Search(tt.query)
			if got == nil || where(got) != tt.want {
				t.Errorf("Search(%q) = %q (nil: %v), want %q", tt.query, where(got), got == nil, tt.want)
			}
		})
	}

	moment := h.Search("flange support")[1]
	if moment.Where != "moment" || moment.Closed || moment.Agent != agent || moment.Label != "Flange support" || moment.Line != "Flanges added" ||
		!strings.HasPrefix(moment.Text, "add flange support Flanges added") || !moment.At.Equal(now.Add(-3*time.Hour)) {
		t.Errorf("a moment's hit = %+v; want the session, what was said with what came of it, and when", moment)
	}
	if closed := h.Search("install")[0]; !closed.Closed || closed.Label != "Docs refresh" || !closed.At.Equal(now.Add(-5*time.Hour)) {
		t.Errorf("a closed session's hit = %+v", closed)
	}
}

func TestSearchKeepsToAFewPlacesInEachSessionAndInAll(t *testing.T) {
	h, _, _ := newHub(t)
	now := time.Now()
	for s := range searchHits {
		id := "s" + strconv.Itoa(100+s)
		for m := range sessionHits + 2 {
			at := now.Add(time.Duration(s)*time.Hour + time.Duration(m)*time.Minute)
			h.SetRoster("agent-"+id, []Session{{ID: id, Agent: "agent-" + id, Status: Idle, Title: "Quiet", LastPrompt: "needle " + strconv.Itoa(m), PromptAt: at, ActiveAt: at}})
		}
	}
	got := h.Search("needle")
	if len(got) != searchHits {
		t.Fatalf("%d hits, want no more than %d", len(got), searchHits)
	}
	latest := "s" + strconv.Itoa(100+searchHits-1)
	for i, x := range got[:sessionHits] {
		if x.SessionID != latest {
			t.Fatalf("hit %d is in %q; want the latest session's first, and no more than %d from any one: %+v", i, x.SessionID, sessionHits, got[:sessionHits+1])
		}
	}
	if got[0].Text != "needle "+strconv.Itoa(sessionHits+1) || got[sessionHits].SessionID == latest {
		t.Errorf("first hit %q, then %q after %d; want a session's latest moments, then the next session", got[0].Text, got[sessionHits].SessionID, sessionHits)
	}
}

func TestMatchCutsALongTextDownToAroundTheMatch(t *testing.T) {
	long := strings.Repeat("alpha ", 60) + "the needle is here " + strings.Repeat("omega ", 60)
	got, ok := match(long, []string{"needle"})
	if r := []rune(got); !ok || !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "…") || !strings.Contains(got, "the needle is here") || len(r) > snippetRunes+2 {
		t.Errorf("match = %q (%d runes), %v; want the part around the word, marked where it was cut", got, len([]rune(got)), ok)
	}
	if at := strings.Index(got, "needle"); at > snippetLead+8 {
		t.Errorf("the match is %d bytes in, want it near the start of what is shown", at)
	}
	if got, ok := match("short and sweet", []string{"sweet"}); !ok || got != "short and sweet" {
		t.Errorf("match = %q, %v; a short text is shown whole", got, ok)
	}
	// Each of these letters is a byte longer once lowered, so the place the
	// word is found at is past the end of the text as it was written.
	if got, ok := match(strings.Repeat("İ", 300)+" flange", []string{"flange"}); !ok || !strings.HasPrefix(got, "İİİ") {
		t.Errorf("match = %q, %v; a text whose lowering changes its length is still matched, and shown from its start", got, ok)
	}
	if _, ok := match("one two", []string{"one", "three"}); ok {
		t.Error("a text with only some of the words matched")
	}
}
