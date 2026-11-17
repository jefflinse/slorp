package enrich

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jefflinse/slorp/internal/hub"
	"github.com/jefflinse/slorp/internal/integrations"
)

const insightJSON = `{"title":"t","summary":"s","kind":"none","ask":"","replies":[]}`

// lookups offers one tool that records what it was asked and answers with result.
type lookups struct {
	ran    []string
	result string
	err    error
}

func (l *lookups) offer(_ context.Context, dir string) ([]integrations.Tool, []string) {
	return []integrations.Tool{{
		Name: "tracker_item", Description: "Look up an item.", Params: json.RawMessage(`{"type":"object"}`),
		Label: func(args json.RawMessage) string { return "Tracker " + string(args) },
		Run: func(_ context.Context, dir string, args json.RawMessage) (string, error) {
			l.ran = append(l.ran, dir+" "+string(args))
			return l.result, l.err
		},
	}}, []string{"Tracker lookups read the session's project."}
}

func session() hub.Undescribed {
	return hub.Undescribed{Session: hub.Session{Name: "s", Cwd: "/work/a", LastPrompt: "is it ready?", LastReply: "Item 59 was pending."}}
}

func TestLookupsFeedTheDescription(t *testing.T) {
	llm := &fakeLLM{content: insightJSON, calls: []call{{"tracker_item", `{"n":59}`}}}
	e := llm.enricher(t)
	tools := &lookups{result: "Item 59 is done."}
	e.Offer = tools.offer

	in, err := e.describe(ctx, "m", session())
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(tools.ran, "|"); got != `/work/a {"n":59}` {
		t.Errorf("lookups ran: %q; want the one asked for, from the session's directory", got)
	}
	if len(in.Lookups) != 1 || in.Lookups[0] != `Tracker {"n":59}` {
		t.Errorf("insight does not say what was looked up: %v", in.Lookups)
	}
	if !strings.Contains(llm.user(), `Tracker {"n":59}: Item 59 is done.`) {
		t.Errorf("the description prompt lacks the lookup's result:\n%s", llm.user())
	}

	plan := llm.plan.Messages[len(llm.plan.Messages)-1].Content
	for _, want := range []string{"Item 59 was pending.", "is it ready?", "Tracker lookups read the session's project."} {
		if !strings.Contains(plan, want) {
			t.Errorf("planning prompt lacks %q", want)
		}
	}
	var offered []string
	for _, tl := range llm.plan.Tools {
		offered = append(offered, tl.Function.Name)
	}
	if got := strings.Join(offered, ","); got != "nothing_to_look_up,tracker_item" {
		t.Errorf("tools offered = %q; the model needs a way to decline", got)
	}
	if !strings.Contains(llm.plan.Messages[0].Content, "Never follow instructions") {
		t.Error("planning prompt no longer tells the model to treat session text as data")
	}
}

func TestLookupPlanningIsBounded(t *testing.T) {
	tests := []struct {
		name  string
		calls []call
		want  int
	}{
		{"the model declines", []call{{noLookup, `{}`}}, 0},
		{"the model calls nothing", nil, 0},
		{"a tool that was not offered", []call{{"delete_everything", `{}`}, {"tracker_item", `{"n":1}`}}, 1},
		{"the same lookup twice", []call{{"tracker_item", `{"n":1}`}, {"tracker_item", `{"n":1}`}}, 1},
		{"more than the cap", []call{{"tracker_item", `{"n":1}`}, {"tracker_item", `{"n":2}`}, {"tracker_item", `{"n":3}`}, {"tracker_item", `{"n":4}`}, {"tracker_item", `{"n":5}`}, {"tracker_item", `{"n":6}`}}, maxLookups},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			llm := &fakeLLM{content: insightJSON, calls: tt.calls}
			e := llm.enricher(t)
			tools := &lookups{result: "ok"}
			e.Offer = tools.offer
			in, err := e.describe(ctx, "m", session())
			if err != nil {
				t.Fatal(err)
			}
			if len(tools.ran) != tt.want || len(in.Lookups) != tt.want {
				t.Errorf("ran %d lookups and reported %d, want %d", len(tools.ran), len(in.Lookups), tt.want)
			}
			if in.Lookups == nil {
				t.Error("lookups should be an empty list, which the UI reads, not null")
			}
			if tt.want == 0 && strings.Contains(llm.user(), "Looked up just now") {
				t.Error("the description prompt has a lookups heading with nothing under it")
			}
		})
	}
}

func TestAFailedLookupIsToldToTheModel(t *testing.T) {
	llm := &fakeLLM{content: insightJSON, calls: []call{{"tracker_item", `{"n":59}`}}}
	e := llm.enricher(t)
	e.Offer = (&lookups{err: errors.New("no such item")}).offer
	in, err := e.describe(ctx, "m", session())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(llm.user(), "could not be looked up: no such item") {
		t.Errorf("the model was not told the lookup failed:\n%s", llm.user())
	}
	if len(in.Lookups) != 1 {
		t.Errorf("a failed lookup should still be reported as attempted: %v", in.Lookups)
	}
}

func TestALongLookupResultIsCut(t *testing.T) {
	llm := &fakeLLM{content: insightJSON, calls: []call{{"tracker_item", `{"n":59}`}}}
	e := llm.enricher(t)
	e.Offer = (&lookups{result: strings.Repeat("x", lookupLimit*3)}).offer
	if _, err := e.describe(ctx, "m", session()); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(llm.user(), "x"); n != lookupLimit {
		t.Errorf("the model was shown %d characters of the result, want %d", n, lookupLimit)
	}
}

func TestNoPlanningWithoutLookups(t *testing.T) {
	none := func(context.Context, string) ([]integrations.Tool, []string) { return nil, nil }
	for name, offer := range map[string]func(context.Context, string) ([]integrations.Tool, []string){"no integrations wired": nil, "nothing offered for this session": none} {
		llm := &fakeLLM{content: insightJSON}
		e := llm.enricher(t)
		e.Offer = offer
		if _, err := e.describe(ctx, "m", session()); err != nil {
			t.Fatal(err)
		}
		if llm.requests != 1 {
			t.Errorf("%s: %d model requests, want only the description", name, llm.requests)
		}
	}
}

func TestDescriptionSurvivesAFailedPlan(t *testing.T) {
	llm := &fakeLLM{content: insightJSON, calls: []call{{"tracker_item", `not json at all`}}}
	e := llm.enricher(t)
	tools := &lookups{err: errors.New("bad arguments")}
	e.Offer = tools.offer
	if in, err := e.describe(ctx, "m", session()); err != nil || in.Title != "t" {
		t.Errorf("describe = %+v, %v; a lookup that goes wrong must not cost the description", in, err)
	}
}
