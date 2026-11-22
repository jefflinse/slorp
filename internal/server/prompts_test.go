package server

import (
	"encoding/json"
	"maps"
	"strings"
	"testing"

	"github.com/jefflinse/slorp/internal/enrich"
	"github.com/jefflinse/slorp/internal/hub"
)

func TestTheLocalModelsInstructionsOverHTTP(t *testing.T) {
	f := newFixture(t)
	kept := func() hub.Prompts { return f.state().Settings.Prompts }
	save := func(prompts any) (int, string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"prompts": prompts})
		return f.do("POST", "/api/settings", string(body), nil)
	}
	type tasks = map[string]string
	type with = map[string]any

	code, body := f.do("GET", "/api/prompts", "", nil)
	var said struct{ Defaults map[string]string }
	if err := json.Unmarshal([]byte(body), &said); code != 200 || err != nil || !maps.Equal(said.Defaults, enrich.Defaults()) {
		t.Fatalf("GET /api/prompts = %d %s; want Slorp's own instructions for each task", code, body)
	}
	if got := kept(); got.Global != "" || got.Tasks == nil || len(got.Tasks) != 0 {
		t.Fatalf("to begin with: %+v; want nothing written, as an empty set of tasks", got)
	}

	if code, body := save(with{"global": "  Write in British English.\r\nBe brief. \n"}); code != 204 {
		t.Fatalf("saving what goes ahead of every task = %d %s", code, body)
	}
	if got := kept().Global; got != "Write in British English.\nBe brief." {
		t.Errorf("ahead of every task = %q; want it trimmed, with plain line endings", got)
	}
	if code, body := save(with{"tasks": tasks{hub.ActNote: " Say where it stands. ", hub.ActFile: "File it."}}); code != 204 {
		t.Fatalf("saving instructions for two tasks = %d %s", code, body)
	}
	if code, body := save(with{"tasks": tasks{hub.ActFile: "File it carefully."}}); code != 204 {
		t.Fatalf("saving instructions for one task = %d %s", code, body)
	}
	want := hub.Prompts{Global: "Write in British English.\nBe brief.", Tasks: tasks{hub.ActNote: "Say where it stands.", hub.ActFile: "File it carefully."}}
	if got := kept(); got.Global != want.Global || !maps.Equal(got.Tasks, want.Tasks) {
		t.Errorf("kept %+v; want %+v: each save changing only what it names", got, want)
	}
	if !f.state().Settings.AnswerPermissions {
		t.Error("saving instructions changed where permission prompts are answered")
	}

	if code, body := save(with{"tasks": tasks{"haiku": "Write one."}}); code != 400 || !strings.Contains(body, "haiku") {
		t.Errorf("instructions for a task the model does not do = %d %q; want 400 naming it", code, body)
	}
	if code, body := save(with{"tasks": tasks{hub.ActNote: strings.Repeat("é", promptLimit+1)}}); code != 400 || !strings.Contains(body, "at most") {
		t.Errorf("instructions too long to keep = %d %q; want 400 saying how long they may be", code, body)
	}
	if code, _ := save(with{"global": strings.Repeat("é", promptLimit+1)}); code != 400 {
		t.Errorf("too much ahead of every task = %d, want 400", code)
	}
	if code, _ := save(with{"tasks": tasks{hub.ActNote: strings.Repeat("é", promptLimit)}}); code != 204 {
		t.Errorf("instructions as long as may be kept = %d, want 204", code)
	}
	want.Tasks[hub.ActNote] = strings.Repeat("é", promptLimit)
	if got := kept(); got.Global != want.Global || !maps.Equal(got.Tasks, want.Tasks) {
		t.Errorf("after saves that were refused, kept global %q and %d tasks; want what was there left alone", got.Global, len(got.Tasks))
	}

	if code, _ := save(with{"global": " ", "tasks": tasks{hub.ActNote: "", hub.ActFile: "\n" + enrich.Defaults()[hub.ActFile] + "\n"}}); code != 204 {
		t.Fatalf("clearing = %d, want 204", code)
	}
	if got := kept(); got.Global != "" || len(got.Tasks) != 0 {
		t.Errorf("kept %+v; want nothing for a task left empty or given Slorp's own instructions back", got)
	}
}
