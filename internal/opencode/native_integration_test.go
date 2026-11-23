package opencode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/hub"
)

func TestNativeConfigPrecedenceAndBundledRuntime(t *testing.T) {
	if os.Getenv("SLORP_NATIVE_TEST") != "1" {
		t.Skip("set SLORP_NATIVE_TEST=1 to test the installed OpenCode in an isolated environment")
	}
	bin, err := exec.LookPath("opencode")
	if err != nil {
		t.Fatal(err)
	}
	clearEnv(t)
	a := fixture(t)
	a.Bin = bin
	t.Setenv("HOME", a.Home)
	for _, key := range []string{"XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(key, filepath.Join(a.Home, key))
	}
	t.Setenv("OPENCODE_DISABLE_MODELS_FETCH", "true")
	t.Setenv("OPENCODE_DISABLE_UPDATE_CHECK", "true")
	var mu sync.Mutex
	var polls []string
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		polls = append(polls, r.Header.Get("X-Slorp-Run"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"replies":[],"fresh":false,"epoch":"test"}`))
	}))
	defer bridge.Close()
	a.URL = bridge.URL
	model, calls := configPromptModel(t)
	old := filepath.Join(a.Home, "old-plugin")
	put(t, filepath.Join(old, "package.json"), `{"name":"opencode-slorp","type":"module","exports":{".":"./index.ts"}}`)
	put(t, filepath.Join(old, "index.ts"), `export default {id:"slorp",setup(){throw new Error("unmanaged Slorp plugin must not load")}}`)
	global := `{"model":"mock/global","plugins":[` + stringJSON(old) + `],"providers":{"mock":{"name":"Mock provider","package":"aisdk:@ai-sdk/openai-compatible","settings":{"apiKey":"test-only","baseURL":` + stringJSON(model.URL+"/v1") + `},"models":{"global":{"name":"Global"},"project":{"name":"Project","variants":[{"id":"high"}]}}}}}`
	globalPath := filepath.Join(a.ConfigDir, "opencode.json")
	put(t, globalPath, global)
	dir := filepath.Join(a.Home, "project 'quoted\"")
	project := `{"model":"mock/project","default_agent":"review","agents":{"review":{"description":"Review root","mode":"primary","model":"mock/project#high"}}}`
	projectPath := filepath.Join(dir, "opencode.json")
	put(t, projectPath, project)
	setup, stopSetup := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopSetup()
	if out, err := exec.CommandContext(setup, "git", "init", "--quiet", dir).CombinedOutput(); err != nil {
		t.Fatalf("isolating project config discovery: %s %v", out, err)
	}
	a.ConfigFile = filepath.Join(a.Home, "explicit.json")
	put(t, a.ConfigFile, `{"model":"mock/global","agents":{"review":{"description":"Explicit"}}}`)
	a.ExtraConfigDir = filepath.Join(a.Home, "extra")
	put(t, filepath.Join(a.ExtraConfigDir, "opencode.json"), global)
	put(t, filepath.Join(a.Home, "prompt.txt"), "Private file-backed agent instructions")
	t.Setenv("OPENCODE_CONFIG_CONTENT", `{"model":"mock/project","agents":{"review":{"description":"Inline","system":"{file:../prompt.txt}"}}}`)
	t.Cleanup(a.Shutdown)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	options, err := a.LaunchOptions(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if options.Defaults.AgentName != "review" || options.Defaults.Model != "mock/project" || options.Defaults.Variant != "high" {
		t.Fatalf("native precedence: %+v", options)
	}
	found := false
	for _, agent := range options.Agents {
		if agent.Name == "review" {
			found = agent.Description == "Inline"
		}
	}
	if !found {
		t.Fatal("inline configuration did not win over extra, project and explicit config")
	}
	mu.Lock()
	n := len(polls)
	mu.Unlock()
	if n != 0 {
		t.Fatal("catalog activated plugin session discovery")
	}
	id, err := a.Start(ctx, hub.Launch{Dir: dir, Name: "Isolated native test", Model: "mock/project", AgentName: "review", Variant: "high"})
	if err != nil {
		t.Fatalf("start %s: %v", id, err)
	}
	if id == "" {
		t.Fatal("empty native ID")
	}
	job := a.Hub.Sessions()[0].Job
	r := a.runtimes[job]
	var resolved struct{ ID, System string }
	if err := r.request(ctx, http.MethodGet, "/api/agent/review", nil, &resolved); err != nil || resolved.System != "Private file-backed agent instructions" {
		t.Fatalf("relative config file reference: %+v %v", resolved, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		tokens := append([]string(nil), polls...)
		mu.Unlock()
		if len(tokens) > 0 {
			for _, token := range tokens {
				if token != r.token {
					t.Fatal("old or unscoped Slorp plugin loaded")
				}
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("bundled managed plugin never polled")
		}
		time.Sleep(50 * time.Millisecond)
	}
	var fork nativeSession
	if err := r.request(ctx, http.MethodPost, "/api/session/"+id+"/prompt", map[string]string{"text": "Isolated fork seed."}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-calls:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := r.request(ctx, http.MethodPost, "/api/experimental/session/"+id+"/wait", map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.request(ctx, http.MethodPost, "/api/session/"+id+"/fork", map[string]any{}, &fork); err != nil {
		t.Fatal(err)
	}
	if fork.ID == "" || fork.ID == id || fork.ParentID != "" {
		t.Fatalf("native fork was not a distinct root: %+v", fork)
	}
	a.Hub.Apply(Agent, fork.ID, func(s *hub.Session) { s.Cwd = dir; s.LastPrompt = "retained fork" })
	a.Hub.Retire(Agent, -time.Second)
	if err := a.Stop(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := a.Resume(ctx, fork.ID, dir, ""); err != nil {
		t.Fatal("resuming native fork:", err)
	}
	forkJob := a.Hub.Sessions()[0].Job
	a.runtimeMu.Lock()
	forkRuntime := a.runtimes[forkJob]
	a.runtimeMu.Unlock()
	if forkRuntime.session.Agent != "review" || forkRuntime.session.Model == nil || forkRuntime.session.Model.ID != "project" || forkRuntime.session.Model.Variant != "high" {
		t.Fatalf("native fork choices not recovered: %+v %+v", forkRuntime.session, forkRuntime.session.Model)
	}
	if err := a.Stop(ctx, forkJob); err != nil {
		t.Fatal(err)
	}
	if err := a.Resume(ctx, id, dir, ""); err != nil {
		t.Fatal(err)
	}
	if got := a.Hub.Sessions()[0].ID; got != id {
		t.Fatalf("resumed as %s, want %s", got, id)
	}
	for path, want := range map[string]string{globalPath: global, projectPath: project} {
		b, err := os.ReadFile(path)
		if err != nil || string(b) != want {
			t.Fatalf("native runtime changed source config %s: %s %v", path, b, err)
		}
	}
}

func stringJSON(s string) string { b, _ := json.Marshal(s); return string(b) }
