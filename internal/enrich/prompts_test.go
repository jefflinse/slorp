package enrich

import (
	"slices"
	"strings"
	"testing"

	"github.com/jefflinse/slorp/internal/hub"
	"github.com/jefflinse/slorp/internal/kb"
	"github.com/jefflinse/slorp/internal/testdb"
)

// told is the system message of the request that planned lookups, and of
// the one sent last.
func (f *fakeLLM) told() (plan, last string) {
	if len(f.plan.Messages) > 0 {
		plan = f.plan.Messages[0].Content
	}
	return plan, f.system()
}

func TestEveryTaskHasInstructionsOfItsOwn(t *testing.T) {
	own := Defaults()
	tasks := []string{hub.ActDescribe, hub.PromptLookup, hub.ActFile, hub.ActRole, hub.ActNote, hub.ActBrief, hub.ActWrapup, hub.ActBackfill}
	if len(own) != len(tasks) {
		t.Errorf("Defaults has %d tasks, want %d", len(own), len(tasks))
	}
	var seen []string
	for _, task := range tasks {
		text := own[task]
		if text == "" || text != strings.TrimSpace(text) || slices.Contains(seen, text) {
			t.Errorf("instructions for %s = %q; want some of its own, with no space around them", task, text)
		}
		seen = append(seen, text)
	}
	own[hub.ActNote] = "changed by the caller"
	if Defaults()[hub.ActNote] == "changed by the caller" {
		t.Error("changing what Defaults returned changed Slorp's own instructions")
	}
}

func TestEachTaskIsToldWhatTheUserWroteForIt(t *testing.T) {
	st := testdb.Open(t)
	h := hub.New(testdb.KB(t, st), st)
	tell := func(global string, tasks map[string]string) {
		s := h.Settings()
		s.Prompts = hub.Prompts{Global: global, Tasks: tasks}
		if _, err := h.SetSettings(s); err != nil {
			t.Fatal(err)
		}
	}
	llm := &fakeLLM{content: insightJSON, calls: []call{{"tracker_item", `{"n":59}`}}}
	e := llm.enricher(t)
	e.Hub, e.Offer = h, (&lookups{result: "Item 59 is done."}).offer
	does := map[string]func(){
		hub.ActDescribe: func() { e.describe(ctx, "m", session()) },
		hub.ActFile: func() {
			e.route(ctx, "m", hub.Unrouted{Session: hub.Session{Insight: &hub.Insight{}}, Efforts: []kb.Effort{{ID: "a", Name: "A"}}})
		},
		hub.ActRole: func() {
			e.role(ctx, "m", hub.Unroled{Session: hub.Session{Insight: &hub.Insight{}}, Roles: []kb.Role{{ID: "r", Name: "R"}}})
		},
		hub.ActNote:   func() { e.note(ctx, "m", hub.Unnoted{Effort: kb.Effort{Name: "A"}}) },
		hub.ActBrief:  func() { e.brief(ctx, "m", briefed()) },
		hub.ActWrapup: func() { e.wrapup(ctx, "m", dayLog()) },
		hub.ActBackfill: func() {
			e.backfill(ctx, "m", hub.Unfilled{Label: "s", Exchanges: []hub.Exchange{{Prompt: "p", Reply: "r"}}})
		},
	}

	own := Defaults()
	for task, do := range does {
		do()
		if _, got := llm.told(); got != own[task] {
			t.Errorf("%s, with nothing written by the user, was told %q; want Slorp's own instructions for it", task, got)
		}
	}
	does[hub.ActDescribe]()
	if plan, _ := llm.told(); plan != own[hub.PromptLookup] {
		t.Errorf("choosing lookups, with nothing written by the user, was told %q", plan)
	}

	written := map[string]string{}
	for task := range own {
		written[task] = "The user's instructions for " + task + "."
	}
	tell("", written)
	for task, do := range does {
		do()
		if _, got := llm.told(); got != written[task] {
			t.Errorf("%s was told %q; want what the user wrote for it, in place of Slorp's own", task, got)
		}
		if len(llm.request.ResponseFormat.JSONSchema.Schema) == 0 || !llm.request.ResponseFormat.JSONSchema.Strict {
			t.Errorf("%s, under the user's instructions, was not held to the shape of answer it needs", task)
		}
	}
	does[hub.ActDescribe]()
	if plan, _ := llm.told(); plan != written[hub.PromptLookup] {
		t.Errorf("choosing lookups was told %q; want what the user wrote for it", plan)
	}

	tell("  Write in British English.\n", map[string]string{hub.ActNote: written[hub.ActNote]})
	does[hub.ActNote]()
	if _, got := llm.told(); got != "Write in British English.\n\n"+written[hub.ActNote] {
		t.Errorf("a note was told %q; want what goes ahead of every task, then what the user wrote for it", got)
	}
	does[hub.ActDescribe]()
	if plan, got := llm.told(); got != "Write in British English.\n\n"+own[hub.ActDescribe] || plan != "Write in British English.\n\n"+own[hub.PromptLookup] {
		t.Errorf("describing was told %q, and choosing lookups %q; want what goes ahead of every task, then Slorp's own for each", got, plan)
	}
}

func TestACheckSendsNothingTheUserWrote(t *testing.T) {
	st := testdb.Open(t)
	h := hub.New(testdb.KB(t, st), st)
	s := h.Settings()
	s.Prompts.Global = "Write in British English."
	if _, err := h.SetSettings(s); err != nil {
		t.Fatal(err)
	}
	llm := &fakeLLM{models: `{"data":[{"id":"warm","type":"llm","state":"loaded"}]}`}
	e := llm.enricher(t)
	e.Hub = h
	if check := e.Probe(ctx, nil); !check.Answered {
		t.Fatalf("check = %+v, want an answer", check)
	}
	if got := llm.request.Messages; len(got) != 1 || got[0].Role != "user" || got[0].Content != "ping" {
		t.Errorf("a check sent %+v; want the one word and nothing else", got)
	}
}
