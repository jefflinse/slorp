package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
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
	observations := make(chan configToolObservation, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role       string          `json:"role"`
				ToolCallID string          `json:"tool_call_id"`
				Content    json.RawMessage `json:"content"`
			}
			Tools []struct {
				Function struct{ Name, Description string }
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			http.Error(w, "invalid fixture request", 400)
			return
		}
		observation := configToolObservation{Descriptions: map[string]string{}, Outputs: map[string]string{}}
		for _, item := range body.Tools {
			if slices.Contains(names, item.Function.Name) {
				observation.Descriptions[item.Function.Name] = item.Function.Description
			}
		}
		for _, message := range body.Messages {
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
		if len(observation.Outputs) > 0 || len(names) == 0 {
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
		"model":       "mock/test",
		"policies":    []map[string]string{{"action": "provider.use", "resource": "*", "effect": "deny"}, {"action": "provider.use", "resource": "mock", "effect": "allow"}},
		"permissions": []map[string]string{{"action": "*", "resource": "*", "effect": "allow"}},
		"snapshots":   false, "plugins": plugins,
		"agents":    map[string]any{"title": map[string]any{"disabled": true}, "summary": map[string]any{"disabled": true}},
		"providers": map[string]any{"mock": map[string]any{"package": "aisdk:@ai-sdk/openai-compatible", "settings": map[string]string{"apiKey": "isolated", "baseURL": endpoint + "/v1"}, "models": map[string]any{"test": map[string]string{"name": "Test"}}}},
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
	if err := r.request(ctx, http.MethodPost, "/api/session", map[string]any{"title": "Custom tool parity", "location": map[string]string{"directory": r.dir}}, &session); err != nil {
		t.Fatal(err)
	}
	if err := r.request(ctx, http.MethodPost, "/api/session/"+session.ID+"/prompt", map[string]string{"text": "Execute the fixture tools."}, nil); err != nil {
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

func configToolPlugin(t *testing.T, dir, id, setup string) {
	t.Helper()
	put(t, filepath.Join(dir, "package.json"), `{"type":"module","main":"index.ts","exports":{".":"./index.ts"}}`)
	put(t, filepath.Join(dir, "index.ts"), fmt.Sprintf(`export default { id:%q, async setup(ctx) { %s } }`, id, setup))
}

func configToolDefinition(name, value string) string {
	return fmt.Sprintf(`{name:%q,description:%q,options:{codemode:false},input:{type:"object",properties:{},additionalProperties:false},execute:async()=>({content:%q})}`, name, value, value)
}

func TestConfigNativeToolExportsAndAncestorImports(t *testing.T) {
	a := configNativeFixture(t)
	model, observations := configToolModel(t, []string{"same", "globalOnly", "projectOnly", "ancestor", "selfImport"})
	global := filepath.Join(a.Home, "global-plugin")
	project := filepath.Join(a.Home, "project 'quoted", "nested")
	local := filepath.Join(project, ".opencode", "plugins", "project-tools")
	source := filepath.Join(project, "user", "plugins", "project-tools")
	configToolPlugin(t, global, "fixture.global", `const {value}=await import("fixture-global/value");await ctx.tool.transform(editor=>{editor.add(`+configToolDefinition("same", "global")+`);editor.add(`+configToolDefinition("globalOnly", "global-only")+`);editor.add({name:"selfImport",description:"self import",options:{codemode:false},input:{type:"object",properties:{}},execute:async()=>({content:value})});})`)
	put(t, filepath.Join(global, "package.json"), `{"name":"fixture-global","type":"module","exports":{".":"./index.ts","./value":"./value.ts"}}`)
	put(t, filepath.Join(global, "value.ts"), `export const value="self-package-value"`)
	shared := filepath.Join(project, "shared.ts")
	put(t, shared, `export const value = "shared-original"`)
	configToolPlugin(t, source, "fixture.project", `const {value} = await import("../../../shared.ts"); await ctx.tool.transform(editor=>{editor.add(`+configToolDefinition("same", "project")+`);editor.add(`+configToolDefinition("projectOnly", "project-only")+`);editor.add({name:"ancestor",description:"ancestor",options:{codemode:false},input:{type:"object",properties:{}},execute:async()=>({content:value+"|"+ctx.location.directory})});})`)
	if err := os.MkdirAll(filepath.Dir(local), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(source, local); err != nil {
		t.Fatal(err)
	}
	configToolProvider(t, a, model.URL, []string{global})
	// Explicit plugins apply after auto-discovered plugins in V2.
	beforeFiles := configSourceHashes(t, global, local, source, shared, filepath.Join(a.ConfigDir, "opencode.json"))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	managed, err := a.configured(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	defer a.finish(managed)
	after := configExecuteTools(t, ctx, managed, observations)
	if files := configSourceHashes(t, global, local, source, shared, filepath.Join(a.ConfigDir, "opencode.json")); !reflect.DeepEqual(files, beforeFiles) {
		t.Fatal("managed plugin loading changed original files")
	}
	baseline := configBaseline(t, a, project)
	before := configExecuteTools(t, ctx, baseline, observations)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("tool parity: native %+v; managed %+v", before, after)
	}
	for name, want := range map[string]string{"same": "global", "globalOnly": "global-only", "projectOnly": "project-only"} {
		if after.Outputs[name] != want || after.Descriptions[name] != want {
			t.Errorf("%s: %+v", name, after)
		}
	}
	if after.Outputs["ancestor"] != "shared-original|"+project {
		t.Fatal(after.Outputs)
	}
	if after.Outputs["selfImport"] != "self-package-value" {
		t.Fatal(after.Outputs)
	}
}

func TestConfigNativeToolWorkspaceRejected(t *testing.T) {
	a := configNativeFixture(t)
	plugin := filepath.Join(a.Home, "workspace-plugin")
	configToolPlugin(t, plugin, "fixture.workspace", `await ctx.tool.transform(editor=>editor.add(`+configToolDefinition("workspace", "works")+`))`)
	put(t, filepath.Join(plugin, "package.json"), `{"type":"module","main":"index.ts","workspaces":["packages/*"]}`)
	configToolProvider(t, a, "http://127.0.0.1:1", []string{plugin})
	before := configSourceHashes(t, plugin)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := a.configured(ctx, a.Home)
	if err == nil || !strings.Contains(err.Error(), "unsupported custom tool workspace") {
		t.Fatalf("unsafe workspace accepted: %v", err)
	}
	if after := configSourceHashes(t, plugin); !reflect.DeepEqual(before, after) {
		t.Fatal("workspace rejection changed source")
	}
}

func TestConfigNativeBrokenToolFailsDiscovery(t *testing.T) {
	a := configNativeFixture(t)
	plugin := filepath.Join(a.Home, "broken-plugin")
	configToolPlugin(t, plugin, "fixture.broken", `throw new Error("broken fixture")`)
	configToolProvider(t, a, "http://127.0.0.1:1", []string{plugin})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := a.configured(ctx, a.Home)
	if err == nil || !strings.Contains(err.Error(), "failed to load") {
		t.Fatalf("broken plugin silently discarded: %v", err)
	}
}

func TestConfigNativeAncestorWorkspaceRejected(t *testing.T) {
	a := configNativeFixture(t)
	project := filepath.Join(a.Home, "project")
	plugin := filepath.Join(project, "plugin")
	configToolPlugin(t, plugin, "fixture.workspace", `await ctx.tool.transform(editor=>editor.add(`+configToolDefinition("workspace", "works")+`))`)
	put(t, filepath.Join(project, "package.json"), `{"name":"project","workspaces":["packages/*"]}`)
	configToolProvider(t, a, "http://127.0.0.1:1", []string{plugin})
	before := configSourceHashes(t, project)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := a.configured(ctx, project)
	if err == nil || !strings.Contains(err.Error(), "unsupported custom tool workspace in ancestor") {
		t.Fatalf("unsafe ancestor workspace accepted: %v", err)
	}
	if after := configSourceHashes(t, project); !reflect.DeepEqual(before, after) {
		t.Fatal("ancestor rejection changed source")
	}
}
