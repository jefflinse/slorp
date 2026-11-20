package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

type configToolObservation struct {
	Descriptions map[string]string
	Outputs      map[string]string
}

func configToolModel(t *testing.T, names []string) (*httptest.Server, <-chan configToolObservation) {
	t.Helper()
	observations := make(chan configToolObservation, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw struct {
			Messages []struct {
				Role       string          `json:"role"`
				ToolCallID string          `json:"tool_call_id"`
				Content    json.RawMessage `json:"content"`
			}
			Tools []struct {
				Function struct{ Name, Description string }
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Error(err)
			http.Error(w, "invalid fixture request", 400)
			return
		}
		observation := configToolObservation{Descriptions: map[string]string{}, Outputs: map[string]string{}}
		for _, item := range raw.Tools {
			if slices.Contains(names, item.Function.Name) {
				observation.Descriptions[item.Function.Name] = item.Function.Description
			}
		}
		for _, message := range raw.Messages {
			if message.Role != "tool" {
				continue
			}
			for i, name := range names {
				if message.ToolCallID != fmt.Sprintf("config_call_%d", i) {
					continue
				}
				var text string
				if err := json.Unmarshal(message.Content, &text); err != nil {
					t.Errorf("tool output is not text: %s: %v", message.Content, err)
					return
				}
				observation.Outputs[name] = text
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := func(delta any, finish any) {
			data, err := json.Marshal(map[string]any{"id": "config-test", "object": "chat.completion.chunk", "created": 1, "model": "test", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				t.Error(err)
			}
		}
		if len(observation.Outputs) > 0 {
			select {
			case observations <- observation:
			case <-r.Context().Done():
				return
			}
			chunk(map[string]string{"role": "assistant", "content": "Done."}, nil)
			chunk(map[string]any{}, "stop")
		} else {
			calls := make([]any, 0, len(names))
			for i, name := range names {
				calls = append(calls, map[string]any{"index": i, "id": fmt.Sprintf("config_call_%d", i), "type": "function", "function": map[string]string{"name": name, "arguments": "{}"}})
			}
			chunk(map[string]any{"role": "assistant", "tool_calls": calls}, nil)
			chunk(map[string]any{}, "tool_calls")
		}
		if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	return server, observations
}

func configToolProvider(t *testing.T, a *Adapter, endpoint string, plugins []string) {
	t.Helper()
	if plugins == nil {
		plugins = []string{}
	}
	config := map[string]any{
		"$schema": "https://opencode.ai/config.json", "model": "mock/test", "enabled_providers": []string{"mock"},
		"permission": "allow", "snapshot": false, "plugin": plugins,
		"agent":    map[string]any{"title": map[string]any{"disable": true}, "summary": map[string]any{"disable": true}},
		"provider": map[string]any{"mock": map[string]any{"npm": "@ai-sdk/openai-compatible", "options": map[string]string{"apiKey": "isolated", "baseURL": endpoint + "/v1"}, "models": map[string]any{"test": map[string]string{"name": "Test"}}}},
	}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(a.ConfigDir, "opencode.json"), string(data))
}

func configExecuteTools(t *testing.T, ctx context.Context, r *runtime, observations <-chan configToolObservation) configToolObservation {
	t.Helper()
	var session nativeSession
	if err := r.request(ctx, http.MethodPost, "/session", map[string]string{"title": "Custom tool parity"}, &session); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"agent": "build", "parts": []map[string]string{{"type": "text", "text": "Execute the fixture tools."}}}
	if err := r.request(ctx, http.MethodPost, "/session/"+session.ID+"/message", body, nil); err != nil {
		if output, ok := r.cmd.Stdout.(*readyOutput); ok {
			output.mu.Lock()
			t.Logf("native output: %s", output.buf)
			output.mu.Unlock()
		}
		t.Fatal(err)
	}
	select {
	case observation := <-observations:
		return observation
	case <-ctx.Done():
		t.Fatal("stub did not receive tool results:", ctx.Err())
		return configToolObservation{}
	}
}

func TestConfigNativeToolExportsAndAncestorImports(t *testing.T) {
	a := configNativeFixture(t)
	names := []string{"same", "same_globalOnly", "same_projectOnly", "same_shared", "same_pluginOverride", "combo", "combo_globalOnly", "combo_projectOnly", "inside", "ancestor"}
	model, observations := configToolModel(t, names)
	definition := func(label string) string {
		return fmt.Sprintf(`tool({args:{},description:%q,execute:async()=>%q})`, label, label)
	}
	put(t, filepath.Join(a.ConfigDir, "tools", "same.ts"), `import {tool} from '@opencode-ai/plugin'; await Promise.resolve(); export default `+definition("global-default")+`; export const globalOnly = `+definition("global-only")+`; export const shared = `+definition("global-shared")+`; export const pluginOverride = `+definition("global-plugin")+`; export const ignored = 42;`)
	put(t, filepath.Join(a.ConfigDir, "tools", "combo.ts"), `import {tool} from '@opencode-ai/plugin'; export default `+definition("global-combo")+`; export const globalOnly = `+definition("global-combo-only"))
	project := filepath.Join(a.Home, "project 'quoted", "nested", "deep")
	root := filepath.Dir(filepath.Dir(project))
	put(t, filepath.Join(project, "AGENTS.md"), "Tool fixtures only.")
	if out, err := exec.Command("git", "init", "--quiet", root).CombinedOutput(); err != nil {
		t.Fatalf("isolating repository: %s %v", out, err)
	}
	source := filepath.Join(project, ".opencode")
	put(t, filepath.Join(source, "tools", "same.ts"), `import {tool} from '@opencode-ai/plugin'; export default 42; export const globalOnly = 42; export const projectOnly = `+definition("project-only")+`; export const shared = `+definition("project-shared")+`; export const pluginOverride = `+definition("project-plugin"))
	put(t, filepath.Join(source, "tools", "combo.ts"), `import {tool} from '@opencode-ai/plugin'; export default `+definition("project-combo")+`; export const projectOnly = `+definition("project-combo-only"))
	put(t, filepath.Join(source, "tool", "inside.js"), `export default {args:{},description:'singular-js',execute:async()=> 'singular-js'}`)
	put(t, filepath.Join(source, "tools", "inside.ts"), `export default {args:{},description:'plural-ts',execute:async()=> 'plural-ts'}`)
	put(t, filepath.Join(source, "tools", "ancestor.ts"), `import {run} from '../../../../shared/entry.ts'; export default {args:{},description:'ancestor-import',execute:async(_,ctx)=>run()+'|'+ctx.directory}`)
	shared := filepath.Join(root, "shared")
	put(t, filepath.Join(shared, "entry.ts"), `import {value} from 'ancestor-dep'; export const run = () => value`)
	sharedManifest := `{"name":"shared-fixture","type":"module","dependencies":{"ancestor-dep":"1.0.0"}}`
	put(t, filepath.Join(shared, "package.json"), sharedManifest)
	put(t, filepath.Join(shared, "node_modules", "ancestor-dep", "package.json"), `{"name":"ancestor-dep","version":"1.0.0","type":"module","main":"index.js"}`)
	put(t, filepath.Join(shared, "node_modules", "ancestor-dep", "index.js"), `export const value = 'original-shared-dependency'`)
	put(t, filepath.Join(shared, "large-resource"), strings.Repeat("x", 17<<20))
	plugin := filepath.Join(a.Home, "fixture-plugin.mjs")
	put(t, plugin, `export default async () => ({tool:{same_pluginOverride:{args:{},description:'plugin-wins',execute:async()=> 'plugin-wins'}}})`)
	configToolProvider(t, a, model.URL, []string{plugin})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	managed, err := a.configured(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	defer a.finish(managed)
	after := configExecuteTools(t, ctx, managed, observations)
	for _, dir := range []string{source, a.ConfigDir} {
		for _, name := range []string{"package.json", "package-lock.json", "node_modules"} {
			if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
				t.Fatalf("managed installation changed source %s/%s: %v", dir, name, err)
			}
		}
	}
	if data, err := os.ReadFile(filepath.Join(shared, "package.json")); err != nil || string(data) != sharedManifest {
		t.Fatalf("shared package changed: %s %v", data, err)
	}
	baseline := configBaseline(t, a, project)
	before := configExecuteTools(t, ctx, baseline, observations)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("tool parity failed:\nnative %+v\nmanaged %+v", before, after)
	}
	want := map[string]string{"same": "global-default", "same_globalOnly": "global-only", "same_projectOnly": "project-only", "same_shared": "project-shared", "same_pluginOverride": "plugin-wins", "combo": "project-combo", "combo_globalOnly": "global-combo-only", "combo_projectOnly": "project-combo-only"}
	for name, value := range want {
		if after.Outputs[name] != value || after.Descriptions[name] != value {
			t.Errorf("%s = output %q / description %q; want %q", name, after.Outputs[name], after.Descriptions[name], value)
		}
	}
	realProject, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	if after.Outputs["ancestor"] != "original-shared-dependency|"+realProject {
		t.Fatalf("ancestor imports/context changed: %q", after.Outputs["ancestor"])
	}
	for _, r := range []*runtime{baseline, managed} {
		var ids []string
		if err := r.request(ctx, http.MethodGet, "/experimental/tool/ids", nil, &ids); err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			if !slices.Contains(ids, name) {
				t.Errorf("tool ID missing: %s (%v)", name, ids)
			}
		}
		if slices.Contains(ids, "same_ignored") {
			t.Fatal("non-tool export was registered")
		}
	}
}

func TestConfigNativeToolWorkspaceRejected(t *testing.T) {
	a := configNativeFixture(t)
	model, observations := configToolModel(t, []string{"workspace"})
	configToolProvider(t, a, model.URL, nil)
	project := filepath.Join(a.Home, "project")
	source := filepath.Join(project, ".opencode")
	put(t, filepath.Join(source, "package.json"), `{"name":"tool-fixture","private":true,"workspaces":["packages/*"],"dependencies":{"workspace-dep":"*"}}`)
	put(t, filepath.Join(source, "packages", "dep", "package.json"), `{"name":"workspace-dep","version":"1.0.0","type":"module","main":"index.js"}`)
	put(t, filepath.Join(source, "packages", "dep", "index.js"), `export const value = 'native-workspace-dependency'`)
	put(t, filepath.Join(source, "tools", "workspace.ts"), `import {value} from 'workspace-dep'; export default {args:{},description:'workspace',execute:async()=>value}`)
	// Arborist's workspace detection requires a canonical install root on macOS.
	realProject, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	baseline := configBaseline(t, a, realProject)
	before := configExecuteTools(t, ctx, baseline, observations)
	if before.Outputs["workspace"] != "native-workspace-dependency" {
		t.Fatalf("native workspace fixture failed: %+v", before)
	}
	paths := []string{filepath.Join(source, "package.json"), filepath.Join(source, "package-lock.json"), filepath.Join(source, "packages", "dep", "package.json")}
	saved := map[string]string{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		saved[path] = string(data)
	}
	_, err = a.configured(ctx, realProject)
	if err == nil || !strings.Contains(err.Error(), "unsupported custom tool workspace") {
		t.Fatalf("workspace silently changed tool behavior: %v", err)
	}
	for path, want := range saved {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != want {
			t.Fatalf("managed rejection changed source %s: %s %v", path, data, err)
		}
	}
}

func TestConfigNativeBrokenToolFailsDiscovery(t *testing.T) {
	a := configNativeFixture(t)
	put(t, filepath.Join(a.ConfigDir, "tools", "broken.ts"), `throw new Error('broken native tool fixture'); export {}`)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	managed, err := a.configured(ctx, a.Home)
	if err != nil {
		t.Fatal(err)
	}
	defer a.finish(managed)
	baseline := configBaseline(t, a, a.Home)
	for _, r := range []*runtime{baseline, managed} {
		var ids []string
		if err := r.request(ctx, http.MethodGet, "/experimental/tool/ids", nil, &ids); err == nil {
			t.Fatalf("broken tool was silently dropped: %v", ids)
		}
	}
}

func TestConfigNativeAncestorWorkspaceRejected(t *testing.T) {
	a := configNativeFixture(t)
	model, observations := configToolModel(t, []string{"workspace"})
	configToolProvider(t, a, model.URL, nil)
	project := filepath.Join(a.Home, "project")
	source := filepath.Join(project, ".opencode")
	workspace := filepath.Join(project, "packages", "dep")
	put(t, filepath.Join(project, "package.json"), `{"name":"workspace-project","private":true,"workspaces":["packages/*"]}`)
	put(t, filepath.Join(workspace, "package.json"), `{"name":"workspace-dep","version":"1.0.0","type":"module","main":"index.js"}`)
	put(t, filepath.Join(workspace, "index.js"), `export const value = 'ancestor-workspace-dependency'`)
	if err := os.MkdirAll(filepath.Join(project, "node_modules"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(workspace, filepath.Join(project, "node_modules", "workspace-dep")); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(source, "tools", "workspace.ts"), `import {value} from 'workspace-dep'; export default {args:{},description:'workspace',execute:async()=>value}`)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	baseline := configBaseline(t, a, project)
	before := configExecuteTools(t, ctx, baseline, observations)
	if before.Outputs["workspace"] != "ancestor-workspace-dependency" {
		t.Fatalf("native ancestor workspace fixture failed: %+v", before)
	}
	paths := []string{filepath.Join(source, "package.json"), filepath.Join(source, "package-lock.json"), filepath.Join(project, "package.json"), filepath.Join(workspace, "package.json")}
	saved := map[string]string{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		saved[path] = string(data)
	}
	_, err := a.configured(ctx, project)
	if err == nil || !strings.Contains(err.Error(), "unsupported custom tool workspace in ancestor") {
		t.Fatalf("ancestor workspace silently changed tool behavior: %v", err)
	}
	for path, want := range saved {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != want {
			t.Fatalf("managed rejection changed source %s: %s %v", path, data, err)
		}
	}
}
