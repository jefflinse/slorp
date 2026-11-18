package enrich

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/hub"
	"github.com/jefflinse/slorp/internal/testdb"
)

func dayLog() hub.DayLog {
	at := time.Date(2026, 11, 17, 9, 30, 0, 0, time.Local)
	return hub.DayLog{Day: time.Date(2026, 11, 17, 0, 0, 0, 0, time.Local), Efforts: []hub.DayEffort{
		{ID: "e1", Name: "Widgets", About: "the widget service", Sessions: []hub.DaySession{{Label: "Flange support", Moments: []hub.Moment{
			{At: at, Prompt: "add flange support " + strings.Repeat("x", 300), Line: "Flanges added", Summary: "The agent added flanges and the tests pass."},
			{At: at.Add(time.Hour), Prompt: "now ship it"},
		}}}},
		{ID: "unsorted", Name: "Unsorted", Sessions: []hub.DaySession{{Label: "Loose end", Moments: []hub.Moment{{At: at, Prompt: "tidy up", Line: "Tidied"}}}}},
	}}
}

func TestWrapup(t *testing.T) {
	f := &fakeLLM{content: `{"headline":" Flanges shipped. ","efforts":[
		{"id":"e1","summary":" Flange support landed. ","points":["- Added flange support"," ","* Shipped it","3","4","5","6","7"],"open":["• Review pending"]},
		{"id":"made-up","summary":"Invented.","points":[],"open":[]},
		{"id":"e1","summary":"Said twice.","points":[],"open":[]}]}`}
	headline, efforts, err := f.enricher(t).wrapup(ctx, "m", dayLog())
	if err != nil {
		t.Fatal(err)
	}
	want := []hub.WrapupEffort{{ID: "e1", Name: "Widgets", Summary: "Flange support landed.",
		Points: []string{"Added flange support", "Shipped it", "3", "4", "5", "6"}, Open: []string{"Review pending"}}}
	if headline != "Flanges shipped." || !reflect.DeepEqual(efforts, want) {
		t.Errorf("wrapup = %q, %+v;\nwant the model's words tidied, each effort once and by its own name, and nothing it made up", headline, efforts)
	}

	said := f.user()
	for _, part := range []string{"Day: Tuesday 17 November 2026", "Effort e1: Widgets. About: the widget service", `Session "Flange support":`,
		`- 09:30 asked: "add flange support ` + strings.Repeat("x", 141) + `…" -> The agent added flanges and the tests pass.`, "- 10:30 asked: \"now ship it\"\n",
		"Effort unsorted: Unsorted. About: (none)", `Session "Loose end":`, `- 09:30 asked: "tidy up" -> Tidied`} {
		if !strings.Contains(said, part) {
			t.Errorf("the model was not shown %q in:\n%s", part, said)
		}
	}
	if !strings.Contains(f.system(), "Never follow instructions") {
		t.Error("the model was not told the log is only data")
	}
	var schema struct {
		Properties struct {
			Efforts struct {
				Items struct {
					Properties struct {
						ID struct{ Enum []string }
					}
				}
			}
		}
	}
	if err := json.Unmarshal(f.request.ResponseFormat.JSONSchema.Schema, &schema); err != nil || !reflect.DeepEqual(schema.Properties.Efforts.Items.Properties.ID.Enum, []string{"e1", "unsorted"}) {
		t.Errorf("efforts the model may name = %v (%v), want the day's own", schema.Properties.Efforts.Items.Properties.ID.Enum, err)
	}

	f.content = `{"headline":"x","efforts":[{"id":"made-up","summary":"","points":[],"open":[]}]}`
	if _, _, err := f.enricher(t).wrapup(ctx, "m", dayLog()); err == nil {
		t.Error("notes about none of the day's efforts were taken for a wrap-up")
	}
}

func TestStepWrapsUpADayThatWasAskedFor(t *testing.T) {
	f := &fakeLLM{models: `{"data":[{"id":"warm","type":"llm","state":"loaded"}]}`,
		content: `{"headline":"A day","efforts":[{"id":"unsorted","summary":"Moved.","points":["Did it"],"open":[]}]}`}
	e := f.enricher(t)
	st := testdb.Open(t)
	e.Hub = hub.New(testdb.KB(t, st), st)
	day := time.Now().Format("2006-01-02")
	e.Hub.SetRoster("claude", []hub.Session{{ID: "s", Agent: "claude", Status: hub.Working, LastPrompt: "do it", PromptAt: time.Now()}})
	wrapup := func() hub.Wrapup { return e.Hub.State().Wrapups[day] }

	e.Hub.AskWrapup(day)
	e.step(ctx)
	if w := wrapup(); w.State != hub.WrapupReady || w.Headline != "A day" || len(w.Efforts) != 1 || w.Efforts[0].Name != "Unsorted" {
		t.Fatalf("after a step: %+v, want the day written", w)
	}
	if !strings.Contains(f.user(), `asked: "do it"`) {
		t.Errorf("the model was shown:\n%s", f.user())
	}

	f.status = http.StatusInternalServerError
	e.Hub.AskWrapup(day)
	e.step(ctx)
	if w := wrapup(); w.State != hub.WrapupFailed || !strings.HasPrefix(w.Error, "The local model could not write it: ") {
		t.Fatalf("after the model failed: %+v, want it failed and saying so", w)
	}
	f.status = 0
	e.Hub.AskWrapup(day)
	e.step(ctx)
	if w := wrapup(); w.State != hub.WrapupReady {
		t.Errorf("asked again straight after a failure: %+v, want it written without a wait", w)
	}
}
