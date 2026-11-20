package opencode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if key != "PATH" && key != "TMPDIR" {
			t.Setenv(key, "")
		}
	}
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
	old := filepath.Join(a.Home, "old-plugin")
	put(t, filepath.Join(old, "package.json"), `{"name":"opencode-slorp","type":"module","main":"server.ts"}`)
	put(t, filepath.Join(old, "server.ts"), `throw new Error("old Slorp plugin must not load")`)
	global := `{"model":"mock/global","plugin":[` + stringJSON(old) + `],"provider":{"mock":{"name":"Mock provider","npm":"@ai-sdk/openai-compatible","options":{"apiKey":"test-only","baseURL":"http://127.0.0.1:1/v1"},"models":{"global":{"name":"Global"},"project":{"name":"Project","variants":{"high":{}}}}}}}`
	globalPath := filepath.Join(a.ConfigDir, "opencode.json")
	put(t, globalPath, global)
	dir := filepath.Join(a.Home, "project 'quoted\"")
	project := `{"model":"mock/project","default_agent":"review","agent":{"review":{"description":"Review root","mode":"primary","variant":"high"}}}`
	projectPath := filepath.Join(dir, "opencode.json")
	put(t, projectPath, project)
	setup, stopSetup := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopSetup()
	if out, err := exec.CommandContext(setup, "git", "init", "--quiet", dir).CombinedOutput(); err != nil {
		t.Fatalf("isolating project config discovery: %s %v", out, err)
	}
	a.ConfigFile = filepath.Join(a.Home, "explicit.json")
	put(t, a.ConfigFile, `{"model":"mock/global","agent":{"review":{"description":"Explicit"}}}`)
	a.ExtraConfigDir = filepath.Join(a.Home, "extra")
	put(t, filepath.Join(a.ExtraConfigDir, "opencode.json"), `{"model":"mock/global","agent":{"review":{"description":"Extra"}}}`)
	put(t, filepath.Join(a.Home, "prompt.txt"), "Private file-backed agent instructions")
	t.Setenv("OPENCODE_CONFIG_CONTENT", `{"model":"mock/project","agent":{"review":{"description":"Inline","prompt":"{file:../prompt.txt}"}}}`)
	put(t, filepath.Join(dir, ".opencode", "plugins", "old.ts"), "const AGENT = 'opencode'; const path = '/api/bridge/poll'; throw new Error('duplicate Slorp')")
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
	var resolved struct {
		Agent map[string]struct{ Prompt string }
	}
	if err := r.request(ctx, http.MethodGet, "/config", nil, &resolved); err != nil || resolved.Agent["review"].Prompt != "Private file-backed agent instructions" {
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
	seed := map[string]any{"agent": "review", "model": map[string]string{"providerID": "mock", "modelID": "project"}, "variant": "high", "noReply": true, "parts": []map[string]string{{"type": "text", "text": "Isolated fork-history seed; no model response requested."}}}
	if err := r.request(ctx, http.MethodPost, "/session/"+id+"/message", seed, nil); err != nil {
		t.Fatal(err)
	}
	var fork nativeSession
	if err := r.request(ctx, http.MethodPost, "/session/"+id+"/fork", map[string]any{}, &fork); err != nil {
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
