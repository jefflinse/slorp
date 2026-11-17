package hub

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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
	base := Session{ID: "0123456789", Name: "raw-name", Status: Idle, key: "content:1", insightKey: "content:1"}
	with := func(edit func(*Session)) Session {
		s := base
		edit(&s)
		return s
	}

	tests := []struct {
		name          string
		s             Session
		kind, urgency string
		title         string
	}{
		{"idle with nothing said about it", with(func(s *Session) {}), "", "", ""},
		{"model says nothing is needed", with(func(s *Session) { s.Insight = insight(KindNone) }), "", "", ""},
		{"model says it asked a question", with(func(s *Session) { s.Insight = insight(KindQuestion) }), KindQuestion, "blocking", "model title"},
		{"model says it finished something", with(func(s *Session) { s.Insight = insight(KindReview) }), KindReview, "whenever", "model title"},
		{"still working, whatever the model said earlier", with(func(s *Session) { s.Insight, s.Status = insight(KindQuestion), Working }), "", "", ""},
		{"insight is about earlier content", with(func(s *Session) { s.Insight, s.insightKey = insight(KindQuestion), "older:1" }), "", "", ""},
		{"insight predates a context edit only", with(func(s *Session) { s.Insight, s.insightKey = insight(KindQuestion), "content:0" }), KindQuestion, "blocking", "model title"},
		{"permission prompt with no insight", with(func(s *Session) { s.Pending, s.Status = &Pending{Tool: "Bash", Summary: "rm x"}, Waiting }), KindPermission, "blocking", "raw-name"},
		{"permission prompt outranks a review", with(func(s *Session) {
			s.Pending, s.Insight = &Pending{Tool: "Bash", Summary: "rm x"}, insight(KindReview)
		}), KindPermission, "blocking", "model title"},
		{"blocked with no insight", with(func(s *Session) { s.Status = Waiting }), KindQuestion, "blocking", "raw-name"},
		{"failed", with(func(s *Session) { s.Status = Failed }), KindFailed, "soon", "raw-name"},
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
			if it.Kind != tt.kind || it.Urgency != tt.urgency || it.Title != tt.title {
				t.Errorf("item = kind %q urgency %q title %q; want %q %q %q", it.Kind, it.Urgency, it.Title, tt.kind, tt.urgency, tt.title)
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

func TestSetRoster(t *testing.T) {
	h, _, _ := newHub(t)
	h.SetRoster(agent, []Session{idle("a", "/w", "from the transcript")})
	h.SetRoster("opencode", []Session{{ID: "o", Agent: "opencode", Status: Idle}})
	h.Apply(agent, "a", func(s *Session) { s.LastPrompt = "from the bridge" })

	h.SetRoster(agent, []Session{{ID: "a", Agent: agent, Status: Working}, idle("b", "/w", "")})
	a := session(t, h, "a")
	if a.LastReply != "from the transcript" || a.LastPrompt != "from the bridge" || !a.CanReply {
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
	h.SetFeed("github", []Item{in}, nil)

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
	h.SetFeed("jira", []Item{found("j-1", "")}, nil)
	h.SetFeed("github", []Item{found("gh-1", "acme/a"), found("gh-2", "acme/b")}, nil)
	if got := feedItems(h); len(got) != 3 || got[0].ID != "gh-1" || got[1].ID != "gh-2" || got[2].ID != "j-1" || got[2].Source != "jira" {
		t.Fatalf("items = %+v, want each feed's, in a steady order", got)
	}

	h.SetFeed("github", []Item{found("gh-2", "acme/b")}, nil)
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
	h.SetFeed("github", nil, errors.New("gh: not signed in"))
	if st := h.State(); st.Feeds["github"].Error != "gh: not signed in" || st.Feeds["github"].Count != 0 || len(feedItems(h)) != 0 {
		t.Errorf("after a first failed collection: %+v", st.Feeds)
	}

	h.SetFeed("github", []Item{found("gh-1", "acme/a")}, nil)
	if st := h.State(); st.Feeds["github"].Error != "" {
		t.Errorf("a good collection left the old error: %+v", st.Feeds)
	}
	h.SetFeed("github", nil, errors.New("timed out"))
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
	h.SetFeed("github", []Item{found("gh-1", "Acme/Widgets"), found("gh-2", "acme/gadgets"), found("gh-3", "acme/shared")}, nil)
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

	h.SetFeed("github", []Item{found("gh-1", "acme/widgets")}, nil)
	if !heard() {
		t.Error("a collection was not announced")
	}
	h.SetFeed("github", []Item{found("gh-1", "acme/widgets")}, nil)
	if !heard() {
		t.Error("an identical collection was not announced, so the page would not show it was checked again")
	}
	h.SetFeed("github", nil, errors.New("down"))
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
