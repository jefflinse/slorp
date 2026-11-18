package hub

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/integrations"
	"github.com/jefflinse/slorp/internal/kb"
	"github.com/jefflinse/slorp/internal/store"
	"github.com/jefflinse/slorp/internal/testdb"
)

const agent = "claude"

func newHub(t *testing.T) (*Hub, *kb.Store, *store.Store) {
	t.Helper()
	st := testdb.Open(t)
	k := testdb.KB(t, st)
	return New(k, st), k, st
}

func effort(t *testing.T, k *kb.Store, name string) string {
	t.Helper()
	e, err := k.Create(name, "")
	if err != nil {
		t.Fatal(err)
	}
	return e.ID
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func idle(id, cwd, reply string) Session {
	return Session{ID: id, Agent: agent, Status: Idle, Cwd: cwd, LastReply: reply}
}

// quieten backdates a session's last content change past the quiet period.
func quieten(h *Hub, id string) {
	h.sessions[id].keySince = time.Now().Add(-time.Minute)
}

// describe attaches an insight written for the session's current content.
func describe(h *Hub, id, kind string) {
	h.SetInsight(id, h.sessions[id].key, Insight{Title: "title", Summary: "summary", Kind: kind, Ask: "ask", Replies: []string{"ok"}})
}

func session(t *testing.T, h *Hub, id string) Session {
	t.Helper()
	for _, s := range h.Sessions() {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("session %q is not in the hub", id)
	return Session{}
}

func TestItems(t *testing.T) {
	insight := func(kind string) *Insight {
		return &Insight{Title: "model title", Summary: "model summary", Kind: kind, Ask: "model ask", Replies: []string{"yes", "no"}}
	}
	base := Session{ID: "0123456789", Label: "its name", Status: Idle, key: "content:1", insightKey: "content:1"}
	with := func(edit func(*Session)) Session {
		s := base
		edit(&s)
		return s
	}

	tests := []struct {
		name          string
		s             Session
		kind, urgency string
		subtitle      string
	}{
		{"idle with nothing said about it", with(func(s *Session) {}), "", "", ""},
		{"model says nothing is needed", with(func(s *Session) { s.Insight = insight(KindNone) }), "", "", ""},
		{"model says it asked a question", with(func(s *Session) { s.Insight = insight(KindQuestion) }), KindQuestion, "awaiting", "model title"},
		{"model says it finished something", with(func(s *Session) { s.Insight = insight(KindReview) }), KindReview, "whenever", "model title"},
		{"model says permission is wanted, with no prompt pending", with(func(s *Session) { s.Insight = insight(KindPermission) }), KindQuestion, "awaiting", "model title"},
		{"still working, though the model said permission is wanted", with(func(s *Session) { s.Insight, s.Status = insight(KindPermission), Working }), "", "", ""},
		{"still working, whatever the model said earlier", with(func(s *Session) { s.Insight, s.Status = insight(KindQuestion), Working }), "", "", ""},
		{"insight is about earlier content", with(func(s *Session) { s.Insight, s.insightKey = insight(KindQuestion), "older:1" }), "", "", ""},
		{"insight predates a context edit only", with(func(s *Session) { s.Insight, s.insightKey = insight(KindQuestion), "content:0" }), KindQuestion, "awaiting", "model title"},
		{"permission prompt with no insight", with(func(s *Session) { s.Pending, s.Status = &Pending{Tool: "Bash", Summary: "rm x"}, Waiting }), KindPermission, "awaiting", ""},
		{"permission prompt outranks a review", with(func(s *Session) {
			s.Pending, s.Insight = &Pending{Tool: "Bash", Summary: "rm x"}, insight(KindReview)
		}), KindPermission, "awaiting", "model title"},
		{"blocked with no insight", with(func(s *Session) { s.Status = Waiting }), KindQuestion, "awaiting", ""},
		{"blocked on a question it asked", with(func(s *Session) {
			s.Pending, s.Status = &Pending{Tool: "AskUserQuestion", Summary: "Which one?", Question: true}, Waiting
		}), KindQuestion, "awaiting", ""},
		{"blocked on a question, whatever the model made of it", with(func(s *Session) {
			s.Pending, s.Insight = &Pending{Tool: "AskUserQuestion", Summary: "Which one?", Question: true}, insight(KindPermission)
		}), KindQuestion, "awaiting", "model title"},
		{"failed", with(func(s *Session) { s.Status = Failed }), KindFailed, "awaiting", ""},
		{"quiet and nearly out of context", with(func(s *Session) { s.ContextTokens, s.ContextWindow = 900, 1000 }), KindContext, "whenever", ""},
		{"quiet with room left", with(func(s *Session) { s.ContextTokens, s.ContextWindow = 899, 1000 }), "", "", ""},
		{"quiet, with no word on how much its model can take", with(func(s *Session) { s.ContextTokens = 900 }), "", "", ""},
		{"working and nearly out of context", with(func(s *Session) { s.ContextTokens, s.ContextWindow, s.Status = 950, 1000, Working }), "", "", ""},
		{"nearly out of context, and asking something", with(func(s *Session) { s.ContextTokens, s.ContextWindow, s.Status = 950, 1000, Waiting }), KindQuestion, "awaiting", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := items([]Session{tt.s})
			if tt.kind == "" {
				if len(got) != 0 {
					t.Fatalf("got item %+v, want none", got[0])
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("got %d items, want 1", len(got))
			}
			it := got[0]
			if it.Kind != tt.kind || it.Urgency != tt.urgency || it.Subtitle != tt.subtitle {
				t.Errorf("item = kind %q urgency %q subtitle %q; want %q %q %q", it.Kind, it.Urgency, it.Subtitle, tt.kind, tt.urgency, tt.subtitle)
			}
			if it.Title != "its name" {
				t.Errorf("title = %q, want the name the session goes by", it.Title)
			}
			if it.Ask == "" || it.Replies == nil {
				t.Errorf("item needs an ask and a non-nil reply list for the UI: %+v", it)
			}
		})
	}

	t.Run("permission ask names the tool and command", func(t *testing.T) {
		it := items([]Session{with(func(s *Session) { s.Pending = &Pending{Tool: "Bash", Summary: "rm x"} })})[0]
		if !strings.Contains(it.Ask, "Bash") || !strings.Contains(it.Ask, "rm x") {
			t.Errorf("ask = %q", it.Ask)
		}
	})

	t.Run("a question's ask is the question", func(t *testing.T) {
		asked := items([]Session{with(func(s *Session) { s.Pending = &Pending{Tool: "AskUserQuestion", Summary: "Which one?", Question: true} })})[0]
		unsaid := items([]Session{with(func(s *Session) { s.Pending = &Pending{Tool: "question", Question: true} })})[0]
		if asked.Ask != "Which one?" || !strings.Contains(unsaid.Ask, "asked you something") {
			t.Errorf("asks = %q and %q", asked.Ask, unsaid.Ask)
		}
	})

	t.Run("a new ask is a new item, so a dismissal does not hide it", func(t *testing.T) {
		first := items([]Session{with(func(s *Session) { s.Insight = insight(KindQuestion) })})[0]
		second := items([]Session{with(func(s *Session) {
			s.Insight, s.key, s.insightKey = insight(KindQuestion), "different:1", "different:1"
		})})[0]
		if first.ID == second.ID {
			t.Errorf("both items have id %q", first.ID)
		}
	})
}

func TestContentKey(t *testing.T) {
	base := Session{Status: Idle, LastPrompt: "do it", LastReply: "done"}
	same := base
	same.Status = Done // the roster's word for a finished session is not new content
	if contentKey(&base) != contentKey(&same) {
		t.Error("key changed with nothing new to describe")
	}
	changes := map[string]func(*Session){
		"reply":      func(s *Session) { s.LastReply = "done, and tested" },
		"prompt":     func(s *Session) { s.LastPrompt = "do it again" },
		"blocked":    func(s *Session) { s.Status = Waiting },
		"failed":     func(s *Session) { s.Status = Failed },
		"permission": func(s *Session) { s.Pending = &Pending{Tool: "Bash", Summary: "ls"} },
	}
	for name, edit := range changes {
		s := base
		edit(&s)
		if contentKey(&s) == contentKey(&base) {
			t.Errorf("a change of %s left the key unchanged", name)
		}
	}
	a, b := base, base
	a.Pending, b.Pending = &Pending{Tool: "Bash", Summary: "ls"}, &Pending{Tool: "Bash", Summary: "rm -rf /"}
	if contentKey(&a) == contentKey(&b) {
		t.Error("two different permission requests share a key")
	}
}

func TestFilingPrecedence(t *testing.T) {
	h, k, _ := newHub(t)
	a, b := effort(t, k, "A"), effort(t, k, "B")
	h.SetRoster(agent, []Session{idle("s1", "/work/a", "done")})

	check := func(step, wantEffort, wantVia string) {
		t.Helper()
		h.Touch()
		if s := session(t, h, "s1"); s.Effort != wantEffort || s.EffortVia != wantVia {
			t.Fatalf("%s: effort %q via %q, want %q via %q", step, s.Effort, s.EffortVia, wantEffort, wantVia)
		}
	}

	check("nothing known", "", "")
	h.SetGuess("s1", "k", b)
	check("the model's guess", b, ViaGuess)
	must(t, k.AddDir(a, "/work/a"))
	check("a directory rule beats the guess", a, kb.ViaDir)
	must(t, k.Assign("s1", b))
	check("the user's filing beats the rule", b, kb.ViaUser)
	must(t, k.Assign("s1", ""))
	check("the user leaving it unsorted beats the rule", "", kb.ViaUser)
}

func TestGuessOfNoneLeavesSessionUnsorted(t *testing.T) {
	h, k, _ := newHub(t)
	effort(t, k, "A")
	h.SetRoster(agent, []Session{idle("s1", "/w", "done")})
	h.SetGuess("s1", "k", "")
	if s := session(t, h, "s1"); s.Effort != "" || s.EffortVia != "" {
		t.Errorf("effort %q via %q, want unsorted", s.Effort, s.EffortVia)
	}
}

func TestGuessStandsUntilReplaced(t *testing.T) {
	h, k, _ := newHub(t)
	a := effort(t, k, "A")
	h.SetRoster(agent, []Session{idle("s1", "/w", "done")})
	describe(h, "s1", KindReview)
	work := h.Unrouted()
	if len(work) != 1 {
		t.Fatalf("Unrouted = %d sessions, want 1", len(work))
	}
	h.SetGuess("s1", work[0].Key, a)
	if len(h.Unrouted()) != 0 {
		t.Fatal("a session with a current guess is still waiting to be filed")
	}

	effort(t, k, "B") // the model should look again, now that there is more to choose from
	if again := h.Unrouted(); len(again) != 1 || len(again[0].Efforts) != 2 {
		t.Fatalf("after a new effort, Unrouted = %+v", again)
	}
	if s := session(t, h, "s1"); s.Effort != a {
		t.Errorf("the old filing vanished while a new guess is pending: effort %q", s.Effort)
	}

	must(t, k.Delete(a))
	h.Touch()
	if s := session(t, h, "s1"); s.Effort != "" {
		t.Errorf("still filed under a deleted effort: %q", s.Effort)
	}
}

func TestUnroutedLeavesFiledSessionsAlone(t *testing.T) {
	h, k, _ := newHub(t)
	a := effort(t, k, "A")
	must(t, k.AddDir(a, "/work/a"))
	must(t, k.Add(kb.SectionRouting, kb.Entry{Text: "hint"}))
	must(t, k.Add(kb.SectionContext, kb.Entry{Text: "fact"}))
	h.SetRoster(agent, []Session{idle("ruled", "/work/a", "x"), idle("loose", "/elsewhere", "y"), idle("undescribed", "/other", "z")})
	describe(h, "ruled", KindNone)
	describe(h, "loose", KindNone)

	work := h.Unrouted()
	if len(work) != 1 || work[0].Session.ID != "loose" {
		t.Fatalf("Unrouted = %+v, want only the described, unfiled session", work)
	}
	if got := strings.Join(work[0].Hints, "|"); got != "fact|hint" {
		t.Errorf("hints = %q, want the context then the routing hints", got)
	}
}

func TestUndescribed(t *testing.T) {
	h, k, _ := newHub(t)
	ids := func() string {
		var out []string
		for _, w := range h.Undescribed() {
			out = append(out, w.Session.ID)
		}
		return strings.Join(out, ",")
	}

	h.SetRoster(agent, []Session{
		idle("ready", "/w", "all done"),
		idle("silent", "/w", ""),
		{ID: "busy", Agent: agent, Status: Working, LastReply: "halfway"},
	})
	if got := ids(); got != "" {
		t.Fatalf("content that has not held still is already queued: %s", got)
	}
	for _, id := range []string{"ready", "silent", "busy"} {
		quieten(h, id)
	}
	if got := ids(); got != "ready" {
		t.Fatalf("Undescribed = %q, want only the settled session with something to describe", got)
	}

	describe(h, "ready", KindReview)
	if got := ids(); got != "" {
		t.Fatalf("described session queued again: %s", got)
	}

	h.SetRoster(agent, []Session{idle("ready", "/w", "all done, and one more thing")})
	quieten(h, "ready")
	if got := ids(); got != "ready" {
		t.Fatal("new content was not queued for describing")
	}
	if len(h.State().Items) != 0 {
		t.Error("an item built from an insight about older content is still showing")
	}
	describe(h, "ready", KindReview)

	must(t, k.Add(kb.SectionContext, kb.Entry{Text: "a new fact"}))
	h.Touch()
	quieten(h, "ready")
	work := h.Undescribed()
	if len(work) != 1 || len(work[0].Context) != 1 || work[0].Context[0] != "a new fact" {
		t.Fatalf("after a context edit, Undescribed = %+v", work)
	}
	if len(h.State().Items) != 1 {
		t.Error("the item vanished while the context edit is being re-described")
	}
}

func TestPermissionPromptIsDescribedEvenWhileWorking(t *testing.T) {
	h, _, _ := newHub(t)
	h.Ask(agent, "s1", Pending{Tool: "Bash", Summary: "ls"}, true)
	h.sessions["s1"].Status = Working
	quieten(h, "s1")
	if len(h.Undescribed()) != 1 {
		t.Error("a session stopped on a permission prompt was not queued")
	}
}

func TestModelOutputSurvivesRestart(t *testing.T) {
	h, k, st := newHub(t)
	a := effort(t, k, "A")
	roster := []Session{idle("s1", "/w", "done")}
	h.SetRoster(agent, roster)
	describe(h, "s1", KindReview)
	h.SetGuess("s1", h.Unrouted()[0].Key, a)
	h.SetNote(a, "note-key", Note{Line: "line", Stands: "stands"})

	h2 := New(k, st)
	h2.SetRoster(agent, roster)
	quieten(h2, "s1")

	s := session(t, h2, "s1")
	if s.Insight == nil || s.Insight.Title != "title" || s.Effort != a {
		t.Fatalf("after restart: insight %+v, effort %q", s.Insight, s.Effort)
	}
	if len(h2.Undescribed()) != 0 || len(h2.Unrouted()) != 0 {
		t.Error("unchanged content was queued for the model again after a restart")
	}
	if e := h2.State().Efforts[0]; e.Line != "line" || e.Stands != "stands" {
		t.Errorf("effort note lost: %+v", e)
	}
	if len(h2.State().Items) != 1 {
		t.Error("the review item did not come back")
	}
}

func TestEffortNoteFollowsItsSessions(t *testing.T) {
	h, k, _ := newHub(t)
	a := effort(t, k, "A")
	must(t, k.AddDir(a, "/work/a"))
	h.SetRoster(agent, []Session{idle("s1", "/work/a", "done")})
	if len(h.Unnoted()) != 0 {
		t.Fatal("an effort with nothing described yet was queued for a note")
	}

	describe(h, "s1", KindReview)
	work := h.Unnoted()
	if len(work) != 1 || len(work[0].Members) != 1 {
		t.Fatalf("Unnoted = %+v", work)
	}
	h.SetNote(a, work[0].Key, Note{Line: "line", Stands: "stands"})
	if len(h.Unnoted()) != 0 {
		t.Fatal("an effort with a current note is queued again")
	}

	must(t, k.Add(kb.SectionQuestions, kb.Entry{Text: "opt-in?", Effort: a}))
	again := h.Unnoted()
	if len(again) != 1 || len(again[0].Questions) != 1 || again[0].Questions[0] != "opt-in?" {
		t.Fatalf("a new open question did not queue the note again: %+v", again)
	}

	h.SetRoster(agent, []Session{idle("unrelated", "/elsewhere", "x")})
	if e := h.State().Efforts[0]; e.Line != "" || e.Stands != "" {
		t.Errorf("an effort with no open sessions still shows a status: %+v", e)
	}
}

func TestReplyOutbox(t *testing.T) {
	h, k, st := newHub(t)
	if _, ok := h.Enqueue("ghost", "hello"); ok {
		t.Fatal("queued a reply for a session that does not exist")
	}
	h.SetRoster(agent, []Session{idle("s1", "/w", ""), idle("s2", "/w", "")})
	first, _ := h.Enqueue("s1", "first")
	h.Enqueue("s1", "second")
	h.Enqueue("s2", "for the other session")

	claimed := h.Claim("s1")
	if len(claimed) != 2 || claimed[0].Text != "first" || claimed[1].Text != "second" {
		t.Fatalf("Claim = %+v, want this session's two replies in order", claimed)
	}
	if again := h.Claim("s1"); len(again) != 0 {
		t.Fatalf("replies were handed out twice: %+v", again)
	}

	h.Ack(first.ID, Delivered)
	h.Ack(first.ID, Dropped) // a second verdict must not overwrite the first
	states := func(h *Hub) string {
		var out []string
		for _, r := range session(t, h, "s1").Replies {
			out = append(out, string(r.State))
		}
		return strings.Join(out, ",")
	}
	if got := states(h); got != "delivered,claimed" {
		t.Errorf("reply states = %q", got)
	}

	h2 := New(k, st)
	h2.SetRoster(agent, []Session{idle("s1", "/w", "")})
	if got := states(h2); got != "delivered,claimed" {
		t.Errorf("after restart, reply states = %q", got)
	}
	if again := h2.Claim("s1"); len(again) != 0 {
		t.Errorf("a restart handed out claimed replies again: %+v", again)
	}
}

func TestSettingsPersist(t *testing.T) {
	h, k, st := newHub(t)
	if !h.Settings().AnswerPermissions {
		t.Fatal("answering permissions from Slorp should start on")
	}
	h.SetSettings(Settings{AnswerPermissions: false})
	if New(k, st).Settings().AnswerPermissions {
		t.Error("the setting did not survive a restart")
	}
}

func TestIntegrationSettings(t *testing.T) {
	h, k, st := newHub(t)
	h.SetRoster(agent, []Session{idle("s1", "/w", "done")})
	describe(h, "s1", KindReview)
	quieten(h, "s1")
	if len(h.Undescribed()) != 0 {
		t.Fatal("setup: the session should be described")
	}

	h.SetIntegration("github", true)
	quieten(h, "s1")
	if len(h.Undescribed()) != 1 {
		t.Error("turning an integration on did not queue sessions to be described again with it")
	}
	if len(h.State().Items) != 1 {
		t.Error("the item vanished while its session is described again")
	}
	describe(h, "s1", KindReview)

	h.SetIntegration("github", true)
	quieten(h, "s1")
	if len(h.Undescribed()) != 0 {
		t.Error("setting an integration to what it already was queued everything again")
	}

	h.SetSettings(Settings{AnswerPermissions: false})
	if got := h.Settings(); !got.Integrations["github"] || got.AnswerPermissions {
		t.Errorf("changing another setting disturbed the integrations: %+v", got)
	}
	if got := New(k, st).Settings(); !got.Integrations["github"] {
		t.Errorf("after restart: %+v", got)
	}
	if New(testdb.KB(t, testdb.Open(t)), testdb.Open(t)).Settings().Integrations == nil {
		t.Error("a new hub should have an empty set of integrations, not none")
	}
}

func TestInsightBuiltOnALookupExpires(t *testing.T) {
	h, _, _ := newHub(t)
	h.SetRoster(agent, []Session{idle("plain", "/w", "done"), idle("looked", "/w", "PR 59 was pending")})
	h.SetInsight("plain", h.sessions["plain"].key, Insight{Title: "t", Kind: KindNone})
	h.SetInsight("looked", h.sessions["looked"].key, Insight{Title: "t", Kind: KindNone, Lookups: []string{"GitHub PR #59"}})
	quieten(h, "plain")
	quieten(h, "looked")
	if len(h.Undescribed()) != 0 {
		t.Fatal("fresh insights were queued again")
	}

	old := time.Now().Add(-lookupTTL - time.Minute)
	h.sessions["plain"].insightAt, h.sessions["looked"].insightAt = old, old
	work := h.Undescribed()
	if len(work) != 1 || work[0].Session.ID != "looked" {
		t.Fatalf("Undescribed = %+v; want only the session whose insight relied on a lookup", work)
	}
	h.SetInsight("looked", work[0].Key, Insight{Title: "t", Kind: KindNone, Lookups: []string{"GitHub PR #59"}})
	if len(h.Undescribed()) != 0 {
		t.Error("a refreshed insight is still queued")
	}
}

func TestRecentLookupsAreKept(t *testing.T) {
	h, _, _ := newHub(t)
	if h.State().Lookups == nil {
		t.Error("lookups should be an empty list, which the UI reads, not null")
	}
	for i := 0; i < keptLookups+5; i++ {
		h.Looked(Lookup{What: strings.Repeat("x", i+1), OK: true})
	}
	got := h.State().Lookups
	if len(got) != keptLookups || len(got[len(got)-1].What) != keptLookups+5 || len(got[0].What) != 6 {
		t.Errorf("kept %d lookups, first of length %d, last of length %d; want the newest %d, oldest first", len(got), len(got[0].What), len(got[len(got)-1].What), keptLookups)
	}
}

// sandwich is a question prompt's input: one question taking a single pick
// and one taking several.
const sandwich = `{"questions":[
	{"question":"Which soup?","header":"Soup","multiSelect":false,"options":[{"label":"Tomato","description":"Classic"},{"label":"Matzo ball"},{"label":"No soup"}]},
	{"question":"Which sides?","header":"Sides","multiSelect":true,"options":[{"label":"Pickle"},{"label":"Crisps"},{"label":"Coleslaw"}]}]}`

func TestAQuestionSlorpCannotReadIsForTheTerminal(t *testing.T) {
	inputs := map[string]string{
		"no input":                  "",
		"input cut short":           sandwich[:80],
		"no questions":              `{"questions":[]}`,
		"a question with no text":   `{"questions":[{"question":"","options":[{"label":"A"}]}]}`,
		"a question with no option": `{"questions":[{"question":"Which?","options":[]}]}`,
		"an option with no label":   `{"questions":[{"question":"Which?","options":[{"label":"A"},{"label":""}]}]}`,
		"the same option twice":     `{"questions":[{"question":"Which?","options":[{"label":"A"},{"label":"A"}]}]}`,
		"the same question twice":   `{"questions":[{"question":"Which?","options":[{"label":"A"}]},{"question":"Which?","options":[{"label":"B"}]}]}`,
	}
	for name, input := range inputs {
		h, _, _ := newHub(t)
		p := h.Ask(agent, "s1", Pending{Tool: "AskUserQuestion", Summary: "Which?", Input: input, Question: true}, true)
		if p.Answerable || !session(t, h, "s1").Pending.Question {
			t.Errorf("%s: pending = %+v; want a question only the terminal can answer", name, p)
		}
		if h.Answer(p.ID, map[string][]string{"Which?": {"A"}}) || h.Decide(p.ID, Decision{Behavior: Allow}) {
			t.Errorf("%s: answered from Slorp all the same", name)
		}
	}
}

func TestAQuestionIsAnsweredByPickingItsOptions(t *testing.T) {
	ctx := context.Background()
	ask := func(t *testing.T) (*Hub, Pending) {
		h, _, _ := newHub(t)
		return h, h.Ask(agent, "s1", Pending{Tool: "AskUserQuestion", Summary: "Which soup? Which sides?", Input: sandwich, Question: true}, true)
	}
	h, p := ask(t)
	if !p.Answerable {
		t.Fatalf("a question Slorp can read was kept for the terminal: %+v", p)
	}

	refused := map[string]map[string][]string{
		"nothing picked":                         {},
		"one question left out":                  {"Which soup?": {"Tomato"}},
		"a question with no pick":                {"Which soup?": {"Tomato"}, "Which sides?": {}},
		"two picks where one is taken":           {"Which soup?": {"Tomato", "No soup"}, "Which sides?": {"Pickle"}},
		"a pick that was not offered":            {"Which soup?": {"Gazpacho"}, "Which sides?": {"Pickle"}},
		"a pick from another question":           {"Which soup?": {"Pickle"}, "Which sides?": {"Pickle"}},
		"the same pick twice":                    {"Which soup?": {"Tomato"}, "Which sides?": {"Pickle", "Pickle"}},
		"an answer to a question never asked":    {"Which soup?": {"Tomato"}, "Which sides?": {"Pickle"}, "Which bread?": {"Rye"}},
		"a question never asked in place of one": {"Which soup?": {"Tomato"}, "Which bread?": {"Rye"}},
	}
	for name, picks := range refused {
		if h.Answer(p.ID, picks) {
			t.Fatalf("%s was taken as an answer", name)
		}
	}
	if h.Decide(p.ID, Decision{Behavior: Allow}) || h.Decide(p.ID, Decision{Behavior: Allow, Answers: map[string]string{"Which soup?": "Tomato"}}) {
		t.Fatal("a question was allowed without picking its options")
	}
	if session(t, h, "s1").Pending == nil {
		t.Fatal("a refused answer settled the question")
	}

	if !h.Answer(p.ID, map[string][]string{"Which soup?": {"Tomato"}, "Which sides?": {"Crisps", "Pickle"}}) {
		t.Fatal("picks from the options offered were refused")
	}
	dec, ok := h.Await(ctx, p.ID, time.Second)
	want := map[string]string{"Which soup?": "Tomato", "Which sides?": "Pickle, Crisps"}
	if !ok || dec.Behavior != Allow || !reflect.DeepEqual(dec.Answers, want) {
		t.Errorf("the bridge was told %+v, %v; want allow with %v, several picks in the order offered", dec, ok, want)
	}
	if s := session(t, h, "s1"); s.Pending != nil || s.Status != Working {
		t.Errorf("after the answer: status %q pending %+v", s.Status, s.Pending)
	}
	if h.Answer(p.ID, map[string][]string{"Which soup?": {"Tomato"}, "Which sides?": {"Pickle"}}) {
		t.Error("a question was answered twice")
	}

	h, p = ask(t)
	if !h.Decide(p.ID, Decision{Behavior: Deny, Message: "not now", Answers: map[string]string{"Which soup?": "Tomato"}}) {
		t.Fatal("declining to answer was refused")
	}
	if dec, _ := h.Await(ctx, p.ID, time.Second); dec.Behavior != Deny || dec.Message != "not now" || dec.Answers != nil {
		t.Errorf("after declining, the bridge was told %+v", dec)
	}

	h, _, _ = newHub(t)
	command := h.Ask(agent, "s1", Pending{Tool: "Bash", Input: sandwich}, true)
	if h.Answer(command.ID, map[string][]string{"Which soup?": {"Tomato"}, "Which sides?": {"Pickle"}}) {
		t.Error("a permission prompt was answered as if it were a question")
	}
	h.SetSettings(Settings{AnswerPermissions: false})
	if p := h.Ask(agent, "s2", Pending{Tool: "AskUserQuestion", Input: sandwich, Question: true}, true); p.Answerable {
		t.Error("the setting to keep prompts in the terminal was ignored for a question")
	}
	if p := h.Ask(agent, "s3", Pending{Tool: "AskUserQuestion", Input: sandwich, Question: true}, false); p.Answerable {
		t.Error("a question the bridge cannot pass an answer to was offered")
	}
}

func TestAskDecidesWhoMayAnswer(t *testing.T) {
	tests := []struct {
		name               string
		setting, canAnswer bool
		want               bool
	}{
		{"bridge can and setting on", true, true, true},
		{"bridge cannot", true, false, false},
		{"setting off", false, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, _, _ := newHub(t)
			h.SetSettings(Settings{AnswerPermissions: tt.setting})
			p := h.Ask(agent, "s1", Pending{Tool: "Bash", Summary: "ls", Input: "ls"}, tt.canAnswer)
			s := session(t, h, "s1")
			if p.Answerable != tt.want || s.Pending == nil || s.Pending.Answerable != tt.want {
				t.Errorf("answerable = %v (session %+v), want %v", p.Answerable, s.Pending, tt.want)
			}
			if s.Status != Waiting || s.Pending.ID == "" || s.Pending.Input != "ls" {
				t.Errorf("session after Ask: status %q pending %+v", s.Status, s.Pending)
			}
			if got := h.Decide(p.ID, Decision{Behavior: Allow}); got != tt.want {
				t.Errorf("Decide = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDecisionReachesTheBridge(t *testing.T) {
	ctx := context.Background()

	t.Run("decided before the bridge asks", func(t *testing.T) {
		h, _, _ := newHub(t)
		p := h.Ask(agent, "s1", Pending{Tool: "Bash"}, true)
		if !h.Decide(p.ID, Decision{Behavior: Deny, Message: "not now"}) {
			t.Fatal("Decide refused a waiting request")
		}
		if dec, ok := h.Await(ctx, p.ID, time.Second); !ok || dec.Behavior != Deny || dec.Message != "not now" {
			t.Fatalf("Await = %+v, %v", dec, ok)
		}
		if s := session(t, h, "s1"); s.Pending != nil || s.Status != Working {
			t.Errorf("after a decision: status %q pending %+v", s.Status, s.Pending)
		}
		if h.Decide(p.ID, Decision{Behavior: Allow}) {
			t.Error("a request was decided twice")
		}
	})

	t.Run("decided while the bridge waits", func(t *testing.T) {
		h, _, _ := newHub(t)
		p := h.Ask(agent, "s1", Pending{Tool: "Bash"}, true)
		got := make(chan Decision, 1)
		go func() {
			dec, _ := h.Await(ctx, p.ID, 5*time.Second)
			got <- dec
		}()
		time.Sleep(20 * time.Millisecond)
		h.Decide(p.ID, Decision{Behavior: Allow})
		select {
		case dec := <-got:
			if dec.Behavior != Allow {
				t.Errorf("Await = %+v", dec)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the waiting bridge never heard the decision")
		}
	})

	t.Run("undecided when the wait runs out", func(t *testing.T) {
		h, _, _ := newHub(t)
		p := h.Ask(agent, "s1", Pending{Tool: "Bash"}, true)
		if dec, ok := h.Await(ctx, p.ID, 20*time.Millisecond); ok {
			t.Fatalf("Await = %+v, want still waiting", dec)
		}
		if !h.Decide(p.ID, Decision{Behavior: Allow}) {
			t.Error("a request that outlasted one wait can no longer be decided")
		}
	})

	t.Run("a request Slorp is not answering goes to the terminal", func(t *testing.T) {
		h, _, _ := newHub(t)
		p := h.Ask(agent, "s1", Pending{Tool: "Bash"}, false)
		for _, id := range []string{p.ID, "never-asked"} {
			if dec, ok := h.Await(ctx, id, time.Second); !ok || dec.Behavior != Terminal {
				t.Errorf("Await(%q) = %+v, %v", id, dec, ok)
			}
		}
	})
}

func TestRequestSettledOutsideSlorp(t *testing.T) {
	ctx := context.Background()
	expired := func(h *Hub) { h.sessions["s1"].Pending.At = time.Now().Add(-time.Minute) }
	assertRetired := func(t *testing.T, h *Hub, id string) {
		t.Helper()
		if s := session(t, h, "s1"); s.Pending != nil {
			t.Errorf("request still pending: %+v", s.Pending)
		}
		if dec, ok := h.Await(ctx, id, time.Second); !ok || dec.Behavior != Terminal {
			t.Errorf("Await = %+v, %v; want the bridge told to stand down", dec, ok)
		}
		if h.Decide(id, Decision{Behavior: Allow}) {
			t.Error("Slorp accepted an answer to a request the terminal already settled")
		}
	}

	t.Run("the bridge reports it done", func(t *testing.T) {
		h, _, _ := newHub(t)
		p := h.Ask(agent, "s1", Pending{Tool: "Bash"}, true)
		h.Done(p.ID)
		assertRetired(t, h, p.ID)
	})

	t.Run("the roster shows the session working again", func(t *testing.T) {
		h, _, _ := newHub(t)
		p := h.Ask(agent, "s1", Pending{Tool: "Bash"}, true)
		expired(h)
		h.SetRoster(agent, []Session{{ID: "s1", Agent: agent, Status: Working}})
		assertRetired(t, h, p.ID)
	})

	t.Run("the turn ends without an answer", func(t *testing.T) {
		h, _, _ := newHub(t)
		p := h.Ask(agent, "s1", Pending{Tool: "Bash"}, true)
		h.Apply(agent, "s1", func(s *Session) { s.Pending, s.Status = nil, Idle })
		assertRetired(t, h, p.ID)
	})

	// A bridge already blocked in Await must be woken, not left to time out.
	woken := func(t *testing.T, settle func(h *Hub, id string)) {
		t.Helper()
		h, _, _ := newHub(t)
		p := h.Ask(agent, "s1", Pending{Tool: "Bash"}, true)
		got := make(chan Decision, 1)
		go func() {
			dec, _ := h.Await(ctx, p.ID, 5*time.Second)
			got <- dec
		}()
		time.Sleep(20 * time.Millisecond)
		settle(h, p.ID)
		select {
		case dec := <-got:
			if dec.Behavior != Terminal {
				t.Errorf("the waiting bridge was told %+v", dec)
			}
		case <-time.After(time.Second):
			t.Fatal("the waiting bridge was left waiting")
		}
	}
	t.Run("a waiting bridge hears that the bridge reported it done", func(t *testing.T) {
		woken(t, func(h *Hub, id string) { h.Done(id) })
	})
	t.Run("a waiting bridge hears that the roster moved on", func(t *testing.T) {
		woken(t, func(h *Hub, id string) {
			h.mu.Lock()
			h.sessions["s1"].Pending.At = time.Now().Add(-time.Minute)
			h.mu.Unlock()
			h.SetRoster(agent, []Session{{ID: "s1", Agent: agent, Status: Working}})
		})
	})

	t.Run("a request the roster has not caught up with is kept", func(t *testing.T) {
		h, _, _ := newHub(t)
		p := h.Ask(agent, "s1", Pending{Tool: "Bash"}, true)
		h.SetRoster(agent, []Session{{ID: "s1", Agent: agent, Status: Working}})
		if s := session(t, h, "s1"); s.Pending == nil || s.Status != Waiting {
			t.Fatalf("a fresh request was dropped: status %q pending %+v", s.Status, s.Pending)
		}
		if !h.Decide(p.ID, Decision{Behavior: Allow}) {
			t.Error("a fresh request can no longer be decided")
		}
	})

	t.Run("a request is kept for as long as the roster says waiting", func(t *testing.T) {
		h, _, _ := newHub(t)
		p := h.Ask(agent, "s1", Pending{Tool: "Bash"}, true)
		expired(h)
		h.SetRoster(agent, []Session{{ID: "s1", Agent: agent, Status: Waiting, WaitingFor: "permission prompt"}})
		if !h.Decide(p.ID, Decision{Behavior: Allow}) {
			t.Error("an old but still open request can no longer be decided")
		}
	})
}

func TestASessionGoesByALastingName(t *testing.T) {
	h, k, st := newHub(t)
	const id = "abcdef0123456"
	roster := func(h *Hub, name, title string) {
		h.SetRoster(agent, []Session{{ID: id, Agent: agent, Status: Waiting, Name: name, Title: title}})
	}
	check := func(h *Hub, when, want, via string) {
		t.Helper()
		s := session(t, h, id)
		if s.Label != want || s.LabelVia != via {
			t.Errorf("%s: label %q via %q, want %q via %q", when, s.Label, s.LabelVia, want, via)
		}
		if items := h.State().Items; len(items) != 1 || items[0].Title != want {
			t.Errorf("%s: items %+v, want one titled %q", when, items, want)
		}
	}

	roster(h, "", "")
	check(h, "with nothing known", "abcdef01", "")
	roster(h, "dir-9c", "")
	check(h, "with only its agent's name", "dir-9c", "")

	h.SetInsight(id, "k1", Insight{Title: "First thing it did"})
	h.SetInsight(id, "k2", Insight{Title: "Second thing it did"})
	check(h, "once the model has written about it twice", "First thing it did", ViaGuess)

	roster(h, "dir-9c", "Its agent's title")
	check(h, "once its agent titled it", "Its agent's title", "")

	if !h.Rename(id, "Loop reviewer") || h.Rename("nobody", "x") {
		t.Fatal("Rename must report whether the session is known")
	}
	roster(h, "dir-9c", "Its agent's later title")
	check(h, "after the user named it", "Loop reviewer", kb.ViaUser)
	named := func(h *Hub, as string) {
		h.SetRoster(agent, []Session{{ID: id, Agent: agent, Status: Waiting, Name: "dir-9c", Title: "Its agent's later title", Named: as}})
	}
	named(h, "Named in the agent")
	check(h, "after the user also named it in its agent", "Loop reviewer", kb.ViaUser)
	h.Rename(id, "")
	check(h, "with only the name given in its agent", "Named in the agent", kb.ViaUser)
	named(h, "")
	check(h, "once that name is gone too", "Its agent's later title", "")
	h.Rename(id, "Loop reviewer")

	h2 := New(k, st)
	roster(h2, "dir-9c", "Its agent's later title")
	check(h2, "after a restart", "Loop reviewer", kb.ViaUser)
	h2.Rename(id, "")
	check(h2, "after the user took the name back", "Its agent's later title", "")
	roster(h2, "dir-9c", "")
	check(h2, "after a restart, with no title from its agent", "First thing it did", ViaGuess)
}

func TestASessionIsMatchedToThePullRequestFromItsBranch(t *testing.T) {
	h, _, _ := newHub(t)
	pr := func(repo string, number int, branch, base string) integrations.PullRequest {
		return integrations.PullRequest{Repo: repo, Number: number, Branch: branch, Base: base}
	}
	h.SetFeed("github", nil, []integrations.PullRequest{
		pr("Other/Thing", 1, "feat", "main"),
		pr("Acme/Widgets", 2, "main", "main"),
		pr("Acme/Widgets", 3, "feat", "main"),
	}, nil)
	h.SetCheckouts(map[string][]string{"/w/widgets": {"acme/widgets"}, "/w/none": nil})
	on := func(id, cwd, branch string) Session {
		return Session{ID: id, Agent: agent, Status: Idle, Cwd: cwd, Branch: branch}
	}
	h.SetRoster(agent, []Session{
		on("match", "/w/widgets", "feat"),
		on("default", "/w/widgets", "main"),
		on("elsewhere", "/w/none", "feat"),
		on("unknown", "/w/unsearched", "feat"),
		on("branchless", "/w/widgets", ""),
		on("other", "/w/widgets", "fix"),
	})

	want := map[string]string{"match": "Acme/Widgets#3"}
	for _, s := range h.Sessions() {
		if s.Pull != want[s.ID] {
			t.Errorf("session %q: pull %q, want %q", s.ID, s.Pull, want[s.ID])
		}
	}
	if got := h.Cwds(); !slices.Equal(got, []string{"/w/none", "/w/unsearched", "/w/widgets"}) {
		t.Errorf("cwds = %v, want each directory once, in order", got)
	}
}

func TestTheContextItemOutlastsAnExchange(t *testing.T) {
	full := Session{ID: "0123456789", Label: "its name", Status: Idle, key: "content:1", ContextTokens: 934, ContextWindow: 1000}
	first := items([]Session{full})
	full.key = "content:2"
	second := items([]Session{full})
	if len(first) != 1 || len(second) != 1 || first[0].ID != second[0].ID {
		t.Fatalf("items %+v then %+v; want the same item before and after an exchange, so a dismissal holds", first, second)
	}
	if !strings.Contains(first[0].Ask, "93% full") {
		t.Errorf("ask = %q, want it to say how full", first[0].Ask)
	}
	asking := full
	asking.Status = Waiting
	if got := items([]Session{asking}); got[0].ID == first[0].ID {
		t.Error("a question shares the context item's id, so dismissing one would hide the other")
	}
}

func TestLimits(t *testing.T) {
	h, _, _ := newHub(t)
	now := time.Now()
	later := now.Add(time.Hour)
	read := func() []Limit { return h.State().Limits }
	windows := func(pct float64) []Limit {
		return []Limit{{Kind: "five_hour", PercentUsed: pct, ResetsAt: later}, {Kind: "seven_day", PercentUsed: pct + 1}}
	}
	if got := read(); got == nil || len(got) != 0 {
		t.Fatalf("limits = %#v before any reading, want an empty list", got)
	}

	h.SetLimits("claude", time.Time{}, windows(10))
	h.SetLimits("claude", time.Time{}, windows(11))
	if got := read(); len(got) != 2 || got[0].PercentUsed != 10 || got[0].Agent != "claude" || got[1].Kind != "seven_day" {
		t.Fatalf("limits = %+v; a reading of unknown age fills in once, and no more", got)
	}
	h.SetLimits("claude", now, windows(20))
	h.SetLimits("claude", now.Add(-time.Minute), windows(30))
	h.SetLimits("claude", now, windows(31))
	h.SetLimits("claude", time.Time{}, windows(32))
	h.SetLimits("claude", later, nil)
	if got := read(); len(got) != 2 || got[0].PercentUsed != 20 || !got[0].At.Equal(now) {
		t.Fatalf("limits = %+v; want the latest reading that had anything in it", got)
	}
	h.SetLimits("claude", now.Add(time.Second), windows(40))
	if got := read(); got[0].PercentUsed != 40 {
		t.Errorf("limits = %+v; a later reading was not taken", got)
	}

	h.SetLimits("aaa", now, []Limit{{Kind: "gone", PercentUsed: 99, ResetsAt: now.Add(-time.Second)}, {Kind: "spend_limit", PercentUsed: 5}})
	if got := read(); len(got) != 3 || got[0].Agent != "aaa" || got[0].Kind != "spend_limit" {
		t.Errorf("limits = %+v; want each agent's in turn, without the window that has started over", got)
	}
}

func TestAStoredTimelineLosesItsRepeats(t *testing.T) {
	h, _, _ := newHub(t)
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	stored := []Moment{
		{At: at, Prompt: "push it"},
		{At: at.Add(time.Minute), Prompt: "one, two, three, four", Line: ""},
		{At: at.Add(2 * time.Minute), Prompt: "one, two, thr…", Line: "Counted to four", Summary: "It counted."},
		{At: at.Add(3 * time.Minute), Prompt: "one, two, and stop there"},
		{At: at.Add(4 * time.Minute), Prompt: "a long one cut sh…", Line: "Old line", Summary: "Old summary"},
		{At: at.Add(5 * time.Minute), Prompt: "a long one cut short by its agent", Line: "New line"},
	}
	if err := h.cache.Put("session", "s", sessionDoc{Timeline: stored}); err != nil {
		t.Fatal(err)
	}
	h.SetRoster(agent, []Session{{ID: "s", Agent: agent, Status: Idle}})

	got := session(t, h, "s").Timeline
	want := []Moment{
		stored[0],
		{At: at.Add(time.Minute), Prompt: "one, two, three, four", Line: "Counted to four", Summary: "It counted."},
		stored[3],
		{At: at.Add(4 * time.Minute), Prompt: "a long one cut short by its agent", Line: "New line"},
	}
	if len(got) != len(want) {
		t.Fatalf("timeline = %+v, want %d moments", got, len(want))
	}
	for i := range want {
		if got[i].Prompt != want[i].Prompt || got[i].Line != want[i].Line || got[i].Summary != want[i].Summary || !got[i].At.Equal(want[i].At) {
			t.Errorf("moment %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestTimeline(t *testing.T) {
	h, k, st := newHub(t)
	t1 := time.Now().Add(-time.Hour)
	said := func(h *Hub, prompt string, at time.Time) {
		h.SetRoster(agent, []Session{{ID: "s", Agent: agent, Status: Idle, LastPrompt: prompt, PromptAt: at}})
	}
	line := func(h *Hub, title string) {
		h.SetInsight("s", h.sessions["s"].key, Insight{Title: title, Summary: "In full: " + title})
	}
	timeline := func(h *Hub) []Moment { return session(t, h, "s").Timeline }
	want := func(h *Hub, when string, moments ...Moment) {
		t.Helper()
		got := timeline(h)
		if !slices.Equal(got, moments) {
			t.Fatalf("%s:\n got %+v\nwant %+v", when, got, moments)
		}
	}

	h.SetRoster(agent, []Session{{ID: "s", Agent: agent, Status: Idle}})
	want(h, "before anything was said")

	said(h, "do the\n  first thing", t1)
	said(h, "do the\n  first thing", t1)
	first := Moment{At: t1, Prompt: "do the first thing"}
	want(h, "a prompt reported twice", first)

	before := h.State()
	line(h, "Did the first thing")
	line(h, "Did the first thing, then asked")
	first.Line, first.Summary = "Did the first thing, then asked", "In full: Did the first thing, then asked"
	want(h, "described twice", first)
	if got := before.Sessions[0].Timeline[0].Line; got != "" {
		t.Errorf("a state handed out earlier was changed under its reader: line %q", got)
	}

	t3 := t1.Add(10 * time.Minute)
	h.Apply(agent, "s", func(s *Session) { s.LastPrompt, s.PromptAt = "now the second", t3 })
	second := Moment{At: t3, Prompt: "now the second"}
	want(h, "a second prompt", first, second)
	said(h, "do the first thing", t1)
	want(h, "a roster still reporting the earlier prompt", first, second)
	said(h, "now  the second", t3.Add(-time.Second))
	want(h, "the same prompt from another witness", first, second)
	said(h, "now the second", t3.Add(time.Second))
	want(h, "the same prompt from a witness whose clock runs ahead", first, second)

	said(h, "do the first thing", time.Time{})
	want(h, "an earlier prompt reported again with no time", first, second)
	said(h, "now the sec…", time.Time{})
	want(h, "the same prompt reported cut short", first, second)
	said(h, "now the sec…", t3.Add(time.Minute))
	want(h, "the same prompt reported cut short, and later", first, second)
	said(h, "now the second", t3)

	h.SetInsight("s", "content the session has moved on from", Insight{Title: "Stale"})
	want(h, "a description of older content", first, second)

	said(h, strings.Repeat("é", momentLimit+50), time.Time{})
	got := timeline(h)
	if len(got) != 3 || got[2].Prompt != strings.Repeat("é", momentLimit) || time.Since(got[2].At) > time.Minute {
		t.Fatalf("a long prompt with no time of its own: %+v; want it cut to %d runes and timed now", got, momentLimit)
	}
	third := got[2]

	h2 := New(k, st)
	said(h2, "", time.Time{})
	if got := timeline(h2); len(got) != 3 || got[0].Prompt != first.Prompt || got[0].Line != first.Line || !got[0].At.Equal(t1) ||
		got[2].Prompt != third.Prompt || !got[2].At.Equal(third.At) {
		t.Fatalf("after a restart: %+v; want the same three moments", got)
	}
	said(h2, strings.Repeat("é", momentLimit+50), time.Time{})
	if got := timeline(h2); len(got) != 3 {
		t.Fatalf("the prompt it already had, reported again after a restart: %+v", got)
	}

	for i := range timelineLimit + 5 {
		said(h2, "prompt "+strconv.Itoa(i), time.Now().Add(time.Duration(i+1)*time.Second))
	}
	got = timeline(h2)
	if len(got) != timelineLimit || got[0].Prompt != "prompt 5" || got[timelineLimit-1].Prompt != "prompt "+strconv.Itoa(timelineLimit+4) {
		t.Errorf("after many prompts: %d moments, from %q to %q; want the latest %d", len(got), got[0].Prompt, got[len(got)-1].Prompt, timelineLimit)
	}
}

func TestTheTimelineMarksCompactions(t *testing.T) {
	h, k, st := newHub(t)
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)
	at := func(mins int) time.Time { return t0.Add(time.Duration(mins) * time.Minute) }
	report := func(h *Hub, prompt string, when time.Time, c *Compaction) {
		h.SetRoster(agent, []Session{{ID: "s", Agent: agent, Status: Idle, LastPrompt: prompt, PromptAt: when, Compacted: c}})
	}
	want := func(h *Hub, when string, moments ...Moment) {
		t.Helper()
		got := session(t, h, "s").Timeline
		if len(got) != len(moments) {
			t.Fatalf("%s:\n got %+v\nwant %+v", when, got, moments)
		}
		for i, m := range moments {
			if g := got[i]; !g.At.Equal(m.At) || g.Prompt != m.Prompt || g.Line != m.Line || (g.Compacted == nil) != (m.Compacted == nil) || g.Compacted != nil && *g.Compacted != *m.Compacted {
				t.Fatalf("%s: moment %d = %+v (%+v), want %+v (%+v)", when, i, g, g.Compacted, m, m.Compacted)
			}
		}
	}
	squeezed := func(c *Compaction) Moment { return Moment{At: c.At, Compacted: c} }

	c1 := &Compaction{At: at(5), Auto: true, Before: 400000, After: 16000}
	first := Moment{At: at(0), Prompt: "do the first thing"}
	report(h, "do the first thing", at(0), c1)
	want(h, "a prompt and a later compaction heard of together", first, squeezed(c1))

	h.SetInsight("s", h.sessions["s"].key, Insight{Title: "Did the first thing"})
	first.Line = "Did the first thing"
	want(h, "described after the compaction", first, squeezed(c1))

	report(h, "do the first thing", at(0), c1)
	report(h, "do the first thing", time.Time{}, c1)
	want(h, "the same reported again, timed and untimed", first, squeezed(c1))

	c2 := &Compaction{At: at(20)}
	second := Moment{At: at(25), Prompt: "do the first thing"}
	report(h, "do the first thing", at(25), c2)
	want(h, "a compaction and the same words said again after it, heard of together", first, squeezed(c1), squeezed(c2), second)

	report(h, "do the first thing", at(25), c1)
	want(h, "an agent still reporting an earlier compaction", first, squeezed(c1), squeezed(c2), second)

	h2 := New(k, st)
	report(h2, "do the first thing", at(25), c2)
	want(h2, "after a restart", first, squeezed(c1), squeezed(c2), second)
}

func TestAStoredTimelineKeepsItsCompactionsApart(t *testing.T) {
	h, _, _ := newHub(t)
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	c := &Compaction{At: at.Add(time.Minute), Before: 9}
	stored := []Moment{
		{At: at, Prompt: "continue"},
		{At: c.At, Compacted: c},
		{At: c.At, Compacted: &Compaction{At: c.At, Before: 9}},
		{At: at.Add(2 * time.Minute), Compacted: &Compaction{At: at.Add(2 * time.Minute)}},
		{At: at.Add(3 * time.Minute), Prompt: "continue"},
	}
	if err := h.cache.Put("session", "s", sessionDoc{Timeline: stored}); err != nil {
		t.Fatal(err)
	}
	h.SetRoster(agent, []Session{{ID: "s", Agent: agent, Status: Idle}})
	got := session(t, h, "s").Timeline
	if len(got) != 4 || got[0].Prompt != "continue" || got[1].Compacted == nil || got[2].Compacted == nil || !got[2].At.Equal(at.Add(2*time.Minute)) || got[3].Prompt != "continue" {
		t.Errorf("timeline = %+v; want the compaction told twice kept once, the other kept, and the prompts either side of them left apart", got)
	}
}

func TestSetRoster(t *testing.T) {
	h, _, _ := newHub(t)
	h.SetRoster(agent, []Session{idle("a", "/w", "from the transcript")})
	h.SetRoster("opencode", []Session{{ID: "o", Agent: "opencode", Status: Idle}})
	h.Apply(agent, "a", func(s *Session) { s.LastPrompt, s.ContextWindow, s.CostUSD = "from the bridge", 200000, 1.5 })

	h.SetRoster(agent, []Session{{ID: "a", Agent: agent, Status: Working}, idle("b", "/w", "")})
	a := session(t, h, "a")
	if a.LastReply != "from the transcript" || a.LastPrompt != "from the bridge" || !a.CanReply || a.ContextWindow != 200000 || a.CostUSD != 1.5 {
		t.Errorf("a roster with less detail erased what was known: %+v", a)
	}
	session(t, h, "o")

	h.SetRoster(agent, []Session{idle("b", "/w", "")})
	got := h.Sessions()
	if len(got) != 2 {
		t.Fatalf("sessions = %d, want the remaining claude session and the opencode one", len(got))
	}
	for _, s := range got {
		if s.ID == "a" {
			t.Error("a session the roster dropped is still listed")
		}
	}
}

func TestSessionsOrderByNeed(t *testing.T) {
	h, _, _ := newHub(t)
	h.SetRoster(agent, []Session{
		{ID: "idle", Agent: agent, Status: Idle},
		{ID: "working", Agent: agent, Status: Working},
		{ID: "failed", Agent: agent, Status: Failed},
		{ID: "waiting", Agent: agent, Status: Waiting},
	})
	var order []string
	for _, s := range h.Sessions() {
		order = append(order, s.ID)
	}
	if got := strings.Join(order, ","); got != "waiting,failed,working,idle" {
		t.Errorf("order = %s", got)
	}
}

func TestCanReplyFollowsTheBridge(t *testing.T) {
	h, _, _ := newHub(t)
	h.SetRoster(agent, []Session{idle("s1", "/w", "")})
	if session(t, h, "s1").CanReply {
		t.Fatal("a session no bridge has polled for is marked connected")
	}
	h.Apply(agent, "s1", nil)
	if !session(t, h, "s1").CanReply {
		t.Fatal("a session whose bridge just polled is not marked connected")
	}
	h.sessions["s1"].bridgeSeen = time.Now().Add(-time.Minute)
	if session(t, h, "s1").CanReply {
		t.Error("a bridge that stopped polling is still marked connected")
	}
}

func TestSubscribersHearOnlyChanges(t *testing.T) {
	h, _, _ := newHub(t)
	roster := []Session{idle("s1", "/w", "done")}
	h.SetRoster(agent, roster)
	ticks, cancel := h.Subscribe()
	defer cancel()
	heard := func() bool {
		select {
		case <-ticks:
			return true
		case <-time.After(50 * time.Millisecond):
			return false
		}
	}

	h.SetRoster(agent, roster)
	if heard() {
		t.Error("an identical roster was announced as a change")
	}
	h.SetRoster(agent, []Session{idle("s1", "/w", "done, with more")})
	if !heard() {
		t.Error("a real change was not announced")
	}
	cancel()
	h.SetRoster(agent, nil)
	if heard() {
		t.Error("a cancelled subscription still heard a change")
	}
}

func found(id, repo string) Item {
	return Item{ID: id, Kind: "ready-to-merge", Urgency: "whenever", Title: "Ready: " + id, Ask: "Merge it.", Summary: "facts",
		URL: "https://example.com/" + id, Repo: repo, At: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
}

func feedItems(h *Hub) []Item {
	var out []Item
	for _, it := range h.State().Items {
		if it.Source != "" {
			out = append(out, it)
		}
	}
	return out
}

func TestFeedItemsSitBesideSessionItems(t *testing.T) {
	h, _, _ := newHub(t)
	if st := h.State(); st.Feeds == nil || len(st.Feeds) != 0 {
		t.Errorf("Feeds = %v before any collection, want an empty map", st.Feeds)
	}
	h.SetRoster(agent, []Session{{ID: "s1", Agent: agent, Status: Waiting}})
	in := found("gh-1", "acme/widgets")
	in.SessionID, in.Source, in.Replies = "s1", "wrong", nil
	h.SetFeed("github", []Item{in}, nil, nil)

	st := h.State()
	if len(st.Items) != 2 || st.Items[0].SessionID != "s1" || st.Items[0].Source != "" {
		t.Fatalf("items = %+v, want the session's and then the feed's", st.Items)
	}
	got, want := st.Items[1], found("gh-1", "acme/widgets")
	want.Source, want.Replies = "github", []string{}
	if got.Replies == nil || len(got.Replies) != 0 {
		t.Errorf("Replies = %#v, want an empty list for the page to read", got.Replies)
	}
	got.Replies = []string{}
	if got.SessionID != "" || got.Source != "github" || got.ID != want.ID || got.Title != want.Title || got.Ask != want.Ask || got.Summary != want.Summary ||
		got.URL != want.URL || got.Repo != want.Repo || !got.At.Equal(want.At) || got.Kind != want.Kind || got.Urgency != want.Urgency || got.Effort != "" {
		t.Errorf("feed item = %+v, want %+v", got, want)
	}
	if f := st.Feeds["github"]; f.Count != 1 || f.Error != "" || time.Since(f.At) > time.Minute {
		t.Errorf("feed status = %+v", f)
	}
}

func TestFeedsAreKeptApartAndReplaced(t *testing.T) {
	h, _, _ := newHub(t)
	h.SetFeed("jira", []Item{found("j-1", "")}, nil, nil)
	h.SetFeed("github", []Item{found("gh-1", "acme/a"), found("gh-2", "acme/b")}, nil, nil)
	if got := feedItems(h); len(got) != 3 || got[0].ID != "gh-1" || got[1].ID != "gh-2" || got[2].ID != "j-1" || got[2].Source != "jira" {
		t.Fatalf("items = %+v, want each feed's, in a steady order", got)
	}

	h.SetFeed("github", []Item{found("gh-2", "acme/b")}, nil, nil)
	if got := feedItems(h); len(got) != 2 || got[0].ID != "gh-2" || got[1].ID != "j-1" {
		t.Errorf("items = %+v, want the new collection to replace that feed's old one and leave the other", got)
	}
	if st := h.State(); st.Feeds["github"].Count != 1 || st.Feeds["jira"].Count != 1 {
		t.Errorf("feeds = %+v", st.Feeds)
	}

	h.DropFeed("github")
	st := h.State()
	if _, kept := st.Feeds["github"]; kept || len(feedItems(h)) != 1 {
		t.Errorf("after dropping github: feeds %+v, items %+v", st.Feeds, feedItems(h))
	}
}

func TestFailedCollectionKeepsWhatWasFoundBefore(t *testing.T) {
	h, _, _ := newHub(t)
	h.SetFeed("github", nil, nil, errors.New("gh: not signed in"))
	if st := h.State(); st.Feeds["github"].Error != "gh: not signed in" || st.Feeds["github"].Count != 0 || len(feedItems(h)) != 0 {
		t.Errorf("after a first failed collection: %+v", st.Feeds)
	}

	h.SetFeed("github", []Item{found("gh-1", "acme/a")}, nil, nil)
	if st := h.State(); st.Feeds["github"].Error != "" {
		t.Errorf("a good collection left the old error: %+v", st.Feeds)
	}
	h.SetFeed("github", nil, nil, errors.New("timed out"))
	st := h.State()
	if got := feedItems(h); len(got) != 1 || got[0].ID != "gh-1" {
		t.Errorf("items = %+v, want what was found before the failure", got)
	}
	if f := st.Feeds["github"]; f.Error != "timed out" || f.Count != 1 {
		t.Errorf("feed status = %+v, want the error and the count still shown", f)
	}
}

func TestFeedItemsFileByRepository(t *testing.T) {
	h, k, _ := newHub(t)
	a, b := effort(t, k, "A"), effort(t, k, "B")
	must(t, k.AddDir(a, "/work/a"))
	must(t, k.AddDir(b, "/work/b"))
	h.SetFeed("github", []Item{found("gh-1", "Acme/Widgets"), found("gh-2", "acme/gadgets"), found("gh-3", "acme/shared")}, nil, nil)
	filed := func() map[string][2]string {
		out := map[string][2]string{}
		for _, it := range feedItems(h) {
			out[it.ID] = [2]string{it.Effort, it.EffortVia}
		}
		return out
	}

	if got := filed(); got["gh-1"] != [2]string{} || got["gh-2"] != [2]string{} {
		t.Errorf("before any directory is known to hold a checkout: %v, want everything unsorted", got)
	}

	h.SetClones(map[string][]string{"/work/a": {"acme/other", "acme/widgets", "acme/shared"}, "/work/b": {"acme/shared"}, "/elsewhere": {"acme/gadgets"}})
	got := filed()
	if got["gh-1"] != [2]string{a, kb.ViaDir} {
		t.Errorf("gh-1 = %v, want it under the one effort holding a checkout, whatever the case of the name", got["gh-1"])
	}
	if got["gh-2"] != [2]string{} {
		t.Errorf("gh-2 = %v, want unsorted: its checkout is in no effort's directory", got["gh-2"])
	}
	if got["gh-3"] != [2]string{} {
		t.Errorf("gh-3 = %v, want unsorted: two efforts hold a checkout", got["gh-3"])
	}

	must(t, k.AssignRepo("acme/shared", b))
	must(t, k.AssignRepo("acme/widgets", ""))
	h.Touch()
	got = filed()
	if got["gh-3"] != [2]string{b, kb.ViaUser} || got["gh-1"] != [2]string{"", kb.ViaUser} {
		t.Errorf("after the user filed them: %v, want their choices over the rule", got)
	}
}

func TestSessionItemsCarryTheirSessionsEffort(t *testing.T) {
	h, k, _ := newHub(t)
	a := effort(t, k, "A")
	must(t, k.AddDir(a, "/work/a"))
	h.SetRoster(agent, []Session{{ID: "s1", Agent: agent, Status: Waiting, Cwd: "/work/a"}, {ID: "s2", Agent: agent, Status: Waiting, Cwd: "/x"}})
	for _, it := range h.State().Items {
		want := map[string][2]string{"s1": {a, kb.ViaDir}, "s2": {}}[it.SessionID]
		if got := [2]string{it.Effort, it.EffortVia}; got != want {
			t.Errorf("item for %s filed %v, want %v", it.SessionID, got, want)
		}
	}
}

func TestFeedChangesAreAnnounced(t *testing.T) {
	h, k, _ := newHub(t)
	a := effort(t, k, "A")
	must(t, k.AddDir(a, "/work/a"))
	ticks, cancel := h.Subscribe()
	defer cancel()
	heard := func() bool {
		select {
		case <-ticks:
			return true
		case <-time.After(50 * time.Millisecond):
			return false
		}
	}

	h.SetFeed("github", []Item{found("gh-1", "acme/widgets")}, nil, nil)
	if !heard() {
		t.Error("a collection was not announced")
	}
	h.SetFeed("github", []Item{found("gh-1", "acme/widgets")}, nil, nil)
	if !heard() {
		t.Error("an identical collection was not announced, so the page would not show it was checked again")
	}
	h.SetFeed("github", nil, nil, errors.New("down"))
	if !heard() {
		t.Error("a failed collection was not announced")
	}
	h.SetClones(map[string][]string{"/work/a": {"acme/widgets"}})
	if !heard() {
		t.Error("a checkout that files an item was not announced")
	}
	h.SetClones(map[string][]string{"/work/a": {"acme/widgets"}})
	if heard() {
		t.Error("an unchanged search of the directories was announced")
	}
	h.DropFeed("nope")
	if heard() {
		t.Error("dropping a feed that was never collected was announced")
	}
	h.DropFeed("github")
	if !heard() {
		t.Error("dropping a feed was not announced")
	}
}

func TestApplyReportsSessionsItCreates(t *testing.T) {
	h, _, _ := newHub(t)
	if !h.Apply("opencode", "o", nil) {
		t.Error("the first report about a session did not say the session was new")
	}
	if h.Apply("opencode", "o", nil) {
		t.Error("a second report about the same session said it was new")
	}
	h.SetRoster(agent, []Session{idle("a", "/w", "")})
	if h.Apply(agent, "a", nil) {
		t.Error("a report about a session the roster had listed said it was new")
	}
}

func TestBridgeCannotOpenASessionItsRosterDoesNotList(t *testing.T) {
	h, _, _ := newHub(t)
	h.Rostered(agent)
	closed := Session{ID: "closed", Agent: agent, Status: Idle, Cwd: "/w", LastPrompt: "add flange support", PromptAt: time.Now().Add(-time.Hour)}
	h.SetRoster(agent, []Session{idle("open", "/w", ""), closed})
	h.SetRoster(agent, []Session{idle("open", "/w", "")})
	was, ok := h.Archive("closed")
	if !ok {
		t.Fatal("a session the roster stopped listing was not archived")
	}

	for _, id := range []string{"closed", "never-listed"} {
		ran := false
		if h.Apply(agent, id, func(*Session) { ran = true }) || ran {
			t.Errorf("a report about %q, which the roster does not list, was taken", id)
		}
	}
	if p := h.Ask(agent, "closed", Pending{Tool: "Bash"}, true); p.Answerable || len(h.waiters) != 0 {
		t.Errorf("a prompt from a session the roster does not list is offered for an answer: %+v", p)
	}
	if got := h.Sessions(); len(got) != 1 || got[0].ID != "open" {
		t.Errorf("sessions = %+v, want only the one the roster lists", got)
	}
	if now, ok := h.Archive("closed"); !ok || !now.EndedAt.Equal(was.EndedAt) {
		t.Errorf("archived = %+v, %v; want the closed session left as it was at %v", now, ok, was.EndedAt)
	}

	ran := false
	if h.Apply(agent, "open", func(*Session) { ran = true }) || !ran {
		t.Error("a report about a session the roster lists was not applied")
	}
	if !h.Apply("opencode", "o", nil) {
		t.Error("an agent without a roster could not have its bridge open a session")
	}
	h.SetRoster(agent, []Session{idle("open", "/w", ""), closed})
	if _, archived := h.Archive("closed"); archived || session(t, h, "closed").ID != "closed" {
		t.Error("a session the roster lists again did not reopen")
	}
}

func TestSetPlugin(t *testing.T) {
	h, _, _ := newHub(t)
	if got := h.State().Plugins; got == nil || len(got) != 0 {
		t.Fatalf("plugins = %#v before any agent has said, want an empty map", got)
	}
	ticks, stop := h.Subscribe()
	defer stop()
	told := func() bool {
		select {
		case <-ticks:
			return true
		default:
			return false
		}
	}

	h.SetPlugin(agent, Plugin{})
	if got, ok := h.State().Plugins[agent]; !ok || got != (Plugin{}) || !told() {
		t.Errorf("plugins = %+v, want the agent listed as lacking it and subscribers told", h.State().Plugins)
	}
	h.SetPlugin(agent, Plugin{})
	if told() {
		t.Error("subscribers were told of a plugin that had not changed")
	}
	want := Plugin{Installed: true, Enabled: true, Version: "0.1.4"}
	h.SetPlugin(agent, want)
	if got := h.State().Plugins[agent]; got != want || !told() {
		t.Errorf("plugin = %+v, want %+v and subscribers told", got, want)
	}
}

func TestRetireDropsSessionsWhoseBridgeWentSilent(t *testing.T) {
	h, _, _ := newHub(t)
	for _, id := range []string{"silent", "polling"} {
		h.Apply("opencode", id, nil)
	}
	h.SetRoster(agent, []Session{idle("claude", "/w", "")})
	h.sessions["silent"].bridgeSeen = time.Now().Add(-time.Minute)
	ticks, stop := h.Subscribe()
	defer stop()

	h.Retire("opencode", 30*time.Second)
	var left []string
	for _, s := range h.Sessions() {
		left = append(left, s.ID)
	}
	sort.Strings(left)
	if got := strings.Join(left, ","); got != "claude,polling" {
		t.Errorf("sessions = %s, want the one still polling and the other agent's, which no bridge ever polled for", got)
	}
	select {
	case <-ticks:
	default:
		t.Error("subscribers were not told a session had gone")
	}

	h.sessions["polling"].bridgeSeen = time.Now().Add(-29 * time.Second)
	h.Retire("opencode", 30*time.Second)
	session(t, h, "polling")
}

func pull(repo string, n int, onYou bool, at time.Time) integrations.PullRequest {
	p := integrations.PullRequest{Repo: repo, Number: n, Title: "A change", OnYou: onYou, At: at}
	if onYou {
		p.Urgency = "awaiting"
	}
	return p
}

func TestPullRequestsAreListedAndFiledByRepository(t *testing.T) {
	h, k, _ := newHub(t)
	if st := h.State(); st.PullRequests == nil || len(st.PullRequests) != 0 {
		t.Errorf("PullRequests = %v before any collection, want an empty list", st.PullRequests)
	}
	a := effort(t, k, "A")
	must(t, k.AddDir(a, "/work/a"))
	h.SetClones(map[string][]string{"/work/a": {"acme/widgets"}})
	day := func(d int) time.Time { return time.Date(2026, 11, d, 0, 0, 0, 0, time.UTC) }
	blocking, whenever := pull("acme/gadgets", 6, true, day(1)), pull("acme/gadgets", 7, true, day(9))
	blocking.Urgency, whenever.Urgency = "blocking", "whenever"
	h.SetFeed("github", nil, []integrations.PullRequest{
		pull("acme/gadgets", 1, false, day(9)), pull("Acme/Widgets", 2, true, day(1)), pull("acme/gadgets", 4, true, day(5)),
		pull("acme/gadgets", 3, false, day(9)), pull("acme/gadgets", 5, true, day(5)), whenever, blocking,
	}, nil)

	got := h.State().PullRequests
	var order []int
	for _, p := range got {
		order = append(order, p.Number)
	}
	if want := []int{6, 4, 5, 2, 7, 1, 3}; !reflect.DeepEqual(order, want) {
		t.Fatalf("order = %v, want %v: where you hold someone up, then what awaits you, then what will keep, then the rest; within each the most recently changed, then by number", order, want)
	}
	if p := got[3]; p.Effort != a || p.EffortVia != kb.ViaDir || p.Source != "github" || p.Title != "A change" {
		t.Errorf("widgets pull request = %+v, want it filed where its checkout is and marked as GitHub's", p)
	}
	if p := got[1]; p.Effort != "" || p.EffortVia != "" {
		t.Errorf("gadgets pull request = %+v, want it unsorted", p)
	}

	h.SetFeed("github", nil, nil, errors.New("timed out"))
	if got := h.State().PullRequests; len(got) != 7 {
		t.Errorf("after a failed collection %d pull requests are listed, want the 7 found before", len(got))
	}
	h.SetFeed("github", nil, []integrations.PullRequest{pull("acme/gadgets", 1, false, day(9))}, nil)
	if got := h.State().PullRequests; len(got) != 1 {
		t.Errorf("after the next collection %d pull requests are listed, want the 1 it found", len(got))
	}
	h.DropFeed("github")
	if got := h.State().PullRequests; len(got) != 0 {
		t.Errorf("after the feed is turned off %d pull requests are still listed", len(got))
	}
}

// role adds a role to the knowledge base and returns its ID.
func role(t *testing.T, k *kb.Store, name, attention string) string {
	t.Helper()
	must(t, k.Add(kb.SectionRoles, kb.Entry{Name: name, Type: attention}))
	roles := k.Knowledge().Roles
	return roles[len(roles)-1].ID
}

func TestRolePrecedence(t *testing.T) {
	h, k, _ := newHub(t)
	a, b := role(t, k, "A", ""), role(t, k, "B", "")
	h.SetRoster(agent, []Session{idle("s1", "/w", "done")})
	check := func(step, wantRole, wantVia string) {
		t.Helper()
		h.Touch()
		if s := session(t, h, "s1"); s.Role != wantRole || s.RoleVia != wantVia {
			t.Fatalf("%s: role %q via %q, want %q via %q", step, s.Role, s.RoleVia, wantRole, wantVia)
		}
	}

	check("nothing known", "", "")
	h.SetRoleGuess("s1", "k", "")
	check("the model found none", "", "")
	h.SetRoleGuess("s1", "k", a)
	check("the model's guess", a, ViaGuess)
	must(t, k.SetRole("s1", b))
	check("the user's choice beats the guess", b, kb.ViaUser)
	must(t, k.SetRole("s1", ""))
	check("the user choosing none beats the guess", "", kb.ViaUser)
	must(t, k.Remove(kb.SectionRoles, b))
	check("removing an unrelated role changes nothing", "", kb.ViaUser)

	h.SetRoster(agent, []Session{idle("s1", "/w", "done"), idle("s2", "/w", "done")})
	h.SetRoleGuess("s2", "k", a)
	must(t, k.Remove(kb.SectionRoles, a))
	h.Touch()
	if s := session(t, h, "s2"); s.Role != "" {
		t.Errorf("still in a removed role: %q", s.Role)
	}
}

func TestARoleCanKeepASessionFromAsking(t *testing.T) {
	h, k, _ := newHub(t)
	r := role(t, k, "Loop", "")
	h.SetRoster(agent, []Session{
		idle("review", "/w", "done"), idle("question", "/w", "which?"), idle("nothing", "/w", "ok"), idle("prompt", "/w", "running"),
		{ID: "waiting", Agent: agent, Status: Waiting}, {ID: "failed", Agent: agent, Status: Failed},
	})
	describe(h, "review", KindReview)
	describe(h, "question", KindQuestion)
	describe(h, "nothing", KindNone)
	h.Ask(agent, "prompt", Pending{Tool: "Bash", Summary: "rm x"}, true)
	asking := func() string {
		h.Touch()
		var ids []string
		for _, it := range h.State().Items {
			ids = append(ids, it.SessionID)
		}
		sort.Strings(ids)
		return strings.Join(ids, " ")
	}
	const everything, stopped = "failed prompt question review waiting", "failed prompt waiting"

	if got := asking(); got != everything {
		t.Fatalf("with no roles: %q, want %q", got, everything)
	}
	for _, id := range []string{"review", "question", "nothing", "prompt", "waiting", "failed"} {
		h.SetRoleGuess(id, "k", r)
	}
	must(t, k.SetRoleAttention(r, kb.AttentionNone))
	if got := asking(); got != everything {
		t.Errorf("with a role only the model guessed: %q; a guess must not silence anything", got)
	}
	for _, id := range []string{"review", "question", "nothing", "prompt", "waiting", "failed"} {
		must(t, k.SetRole(id, r))
	}
	for _, tt := range [][2]string{{kb.AttentionAll, everything}, {kb.AttentionStopped, stopped}, {kb.AttentionNone, ""}} {
		must(t, k.SetRoleAttention(r, tt[0]))
		if got := asking(); got != tt[1] {
			t.Errorf("with attention %q: %q, want %q", tt[0], got, tt[1])
		}
	}
	must(t, k.SetRole("review", ""))
	if got := asking(); got != "review" {
		t.Errorf("after one session left the role: %q, want only it asking", got)
	}
}

func TestUnroled(t *testing.T) {
	h, k, st := newHub(t)
	roster := []Session{idle("loose", "/w", "x"), idle("chosen", "/w", "y"), idle("undescribed", "/w", "z")}
	h.SetRoster(agent, roster)
	describe(h, "loose", KindNone)
	describe(h, "chosen", KindNone)
	if got := h.Unroled(); len(got) != 0 {
		t.Fatalf("with no roles to choose among, Unroled = %+v", got)
	}
	a := role(t, k, "A", "")
	must(t, k.SetRole("chosen", a))

	work := h.Unroled()
	if len(work) != 1 || work[0].Session.ID != "loose" || len(work[0].Roles) != 1 || work[0].Roles[0].ID != a {
		t.Fatalf("Unroled = %+v, want only the described session the user has not given a role, with the roles", work)
	}
	h.SetRoleGuess("loose", work[0].Key, a)
	if len(h.Unroled()) != 0 {
		t.Fatal("a session with a current guess is still waiting for one")
	}

	h2 := New(k, st)
	h2.SetRoster(agent, roster)
	h2.SetRoster(agent, roster)
	if s := session(t, h2, "loose"); s.Role != a || s.RoleVia != ViaGuess || len(h2.Unroled()) != 0 {
		t.Errorf("after a restart: role %q via %q, %d waiting; want the guess kept and nothing asked again", s.Role, s.RoleVia, len(h2.Unroled()))
	}

	role(t, k, "B", "")
	if again := h.Unroled(); len(again) != 1 || len(again[0].Roles) != 2 {
		t.Errorf("after a new role, Unroled = %+v; want the model to look again", again)
	}
	if s := session(t, h, "loose"); s.Role != a {
		t.Errorf("the old guess vanished while a new one is pending: %q", s.Role)
	}
}

func TestBridgeSaysHowSubagentsStand(t *testing.T) {
	h, _, _ := newHub(t)
	roster := []Session{{ID: "s1", Agent: "claude", Subagents: []Subagent{
		{ID: "read", Type: "Explore", Description: "Find the callers", Tools: 3},
		{ID: "quiet", Description: "Write the fix", Done: true},
	}}}
	h.SetRoster("claude", roster)
	subagents := func() []Subagent { return h.State().Sessions[0].Subagents }
	if got := subagents(); len(got) != 2 || got[0].Status != "" {
		t.Fatalf("subagents = %+v, want the two the roster listed, as it listed them", got)
	}

	h.Apply("claude", "s1", func(s *Session) {
		s.Delegated([]Subagent{
			{ID: "read", Type: "general-purpose", Description: "Something else", Model: "claude-haiku-5-5", Status: "failed", Doing: "Reading hub.go", Samples: []int{10, 20}, CostUSD: 0.5, ParentID: "quiet"},
			{ID: "new", Type: "Plan", Description: "Plan the change", Status: "running"},
			{ID: "gone", Status: "killed"},
		})
	})
	got := subagents()
	if len(got) != 4 || got[0].ID != "read" || got[1].ID != "quiet" || got[2].ID != "new" || got[3].ID != "gone" {
		t.Fatalf("subagents = %+v, want those read from the record and then those only the bridge knows of", got)
	}
	read := got[0]
	if read.Status != "failed" || read.Doing != "Reading hub.go" || len(read.Samples) != 2 || read.CostUSD != 0.5 || read.ParentID != "quiet" || !read.Done {
		t.Errorf("one the bridge spoke of = %+v, want how it stands, and that it is over", read)
	}
	if read.Type != "Explore" || read.Description != "Find the callers" || read.Model != "claude-haiku-5-5" || read.Tools != 3 {
		t.Errorf("one the bridge spoke of = %+v, want its record to stand where it says, and the bridge to fill in the rest", read)
	}
	if got[1].Status != "" || !got[1].Done {
		t.Errorf("one the bridge did not speak of = %+v, want it as its record has it", got[1])
	}
	if got[2].Done || got[2].Description != "Plan the change" || !got[3].Done {
		t.Errorf("those only the bridge knows of = %+v and %+v, want one running and one over", got[2], got[3])
	}

	h.SetRoster("claude", roster)
	if got := subagents(); len(got) != 4 || got[0].Status != "failed" {
		t.Errorf("subagents = %+v after the roster was read again, want what the bridge said kept", got)
	}
	h.Apply("claude", "s1", func(s *Session) { s.Delegated(nil) })
	if got := subagents(); len(got) != 2 || got[0].Status != "" || got[0].Done {
		t.Errorf("subagents = %+v once the bridge has none to speak of, want the record alone", got)
	}

	h.Apply("claude", "s1", func(s *Session) { s.Delegated([]Subagent{{ID: "read", Status: "running"}}) })
	h.mu.Lock()
	h.sessions["s1"].bridgeSeen = time.Now().Add(-2 * bridgeTTL)
	h.mu.Unlock()
	if got := subagents(); len(got) != 2 || got[0].Status != "" {
		t.Errorf("subagents = %+v with the bridge gone quiet, want nothing of what it last said", got)
	}
}
