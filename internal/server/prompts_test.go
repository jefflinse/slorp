package server

import (
	"encoding/json"
	"fmt"
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

	if code, body := save(with{"rewrite": hub.RewriteAlways}); code != 204 {
		t.Fatalf("choosing to always rewrite = %d %s", code, body)
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

func TestRewritingWhatTheLocalModelWrote(t *testing.T) {
	f := newFixture(t)
	save := func(prompts string) (int, string) {
		t.Helper()
		return f.do("POST", "/api/settings", `{"prompts":`+prompts+`}`, nil)
	}
	rewritten := func() string {
		got := f.state().Settings.Rewritten
		return fmt.Sprintf("describe %d, file %d, role %d, note %d, brief %d", got[hub.ActDescribe], got[hub.ActFile], got[hub.ActRole], got[hub.ActNote], got[hub.ActBrief])
	}
	if got := f.state().Settings.Prompts.Rewrite; got != hub.RewriteAsk {
		t.Fatalf("to begin with, rewriting is %q; want it left to be asked", got)
	}

	if code, body := save(`{"tasks":{"note":"Say where it stands."}}`); code != 200 || strings.TrimSpace(body) != `{"behind":["note"]}` {
		t.Errorf("a save that changed a task's instructions = %d %s; want 200 naming what it left behind", code, body)
	}
	if code, body := save(`{"tasks":{"note":"Say where it stands."}}`); code != 204 {
		t.Errorf("saving the same again = %d %s; want 204 with nothing left behind", code, body)
	}
	if code, body := save(`{"tasks":{"wrapup":"Write the day up."}}`); code != 204 {
		t.Errorf("a save for a task done only when asked for = %d %s; want 204", code, body)
	}
	if code, body := save(`{"rewrite":"sometimes"}`); code != 400 || !strings.Contains(body, hub.RewriteNever) {
		t.Errorf("a way of rewriting there is not = %d %q; want 400 naming the ways there are", code, body)
	}
	if code, _ := save(`{"rewrite":"never"}`); code != 204 || f.state().Settings.Prompts.Rewrite != hub.RewriteNever {
		t.Errorf("choosing never to rewrite = %d, leaving %q", code, f.state().Settings.Prompts.Rewrite)
	}
	if code, body := save(`{"global":"Write in British English."}`); code != 200 || strings.TrimSpace(body) != `{"behind":["describe","brief"]}` {
		t.Errorf("a save of what goes ahead of every task = %d %s; want 200 naming what it left behind", code, body)
	}
	if got := rewritten(); got != "describe 0, file 0, role 0, note 0, brief 0" {
		t.Errorf("with nothing asked for again: %s", got)
	}

	if code := f.post("/api/model/rewrite", `{"tasks":["note"]}`); code != 204 {
		t.Errorf("rewriting for one task = %d, want 204", code)
	}
	if code, body := f.do("POST", "/api/model/rewrite", `{"tasks":["note","wrapup"]}`, nil); code != 400 || !strings.Contains(body, "wrapup") {
		t.Errorf("rewriting for a task nothing is kept for = %d %q; want 400 naming it", code, body)
	}
	if got := rewritten(); got != "describe 0, file 0, role 0, note 1, brief 0" {
		t.Errorf("after rewriting notes, and a request that was refused: %s", got)
	}
	if code := f.post("/api/model/rewrite", `{}`); code != 204 {
		t.Errorf("rewriting it all = %d, want 204", code)
	}
	if got := rewritten(); got != "describe 1, file 0, role 0, note 1, brief 1" {
		t.Errorf("after rewriting it all: %s; want sessions described and pull requests briefed again, the rest following", got)
	}

	if code, _ := save(`{"rewrite":"always"}`); code != 204 {
		t.Fatalf("choosing to always rewrite = %d", code)
	}
	if code, body := save(`{"tasks":{"file":"File it carefully."}}`); code != 204 {
		t.Errorf("a save with rewriting set to always = %d %s; want 204, with nothing left behind", code, body)
	}
	if got := rewritten(); got != "describe 1, file 1, role 0, note 1, brief 1" {
		t.Errorf("after that save: %s; want filings written again", got)
	}
	if code := f.post("/api/settings", `{"rewritten":{"file":7}}`); code != 204 || f.state().Settings.Rewritten[hub.ActFile] != 1 {
		t.Errorf("settings that count rewrites themselves = %d, leaving %d; want the count left alone", code, f.state().Settings.Rewritten[hub.ActFile])
	}
}
