package opencode

import (
	"context"
	"encoding/json"
	"errors"
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

	"github.com/jefflinse/slorp/internal/hub"
)

func configNativeFixture(t *testing.T) *Adapter {
	t.Helper()
	if os.Getenv("SLORP_NATIVE_TEST") != "1" {
		t.Skip("set SLORP_NATIVE_TEST=1 for isolated native v1.19.6 config parity tests")
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
	t.Setenv("TMPDIR", t.TempDir())
	a := fixture(t)
	a.Bin = bin
	a.initLifecycle()
	t.Setenv("HOME", a.Home)
	t.Setenv("OPENCODE_TEST_HOME", a.Home)
	for _, key := range []string{"XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(key, filepath.Join(a.Home, key))
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Dir(a.ConfigDir))
	t.Setenv("OPENCODE_DISABLE_MODELS_FETCH", "true")
	t.Setenv("OPENCODE_DISABLE_UPDATE_CHECK", "true")
	cmd := a.command(context.Background(), "--version")
	out, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "1.19.6" {
		t.Fatalf("native parity requires 1.19.6: %s %v", out, err)
	}
	wrapper := filepath.Join(a.Home, "native-debug")
	put(t, wrapper, "#!/bin/sh\nexec "+hub.ShellQuote(bin)+" --print-logs \"$@\"\n")
	if err := os.Chmod(wrapper, 0o700); err != nil {
		t.Fatal(err)
	}
	a.Bin = wrapper
	t.Cleanup(a.Shutdown)
	return a
}

func configBaseline(t *testing.T, a *Adapter, dir string) *runtime {
	t.Helper()
	cmd := a.command(context.Background(), "serve", "--hostname", "127.0.0.1", "--port", "0", "--mdns=false")
	cmd.Dir = dir
	output := &readyOutput{ready: make(chan string, 1)}
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cmd.Process.Kill(); err != nil && !strings.Contains(err.Error(), "process already finished") {
			t.Error(err)
		}
		_ = cmd.Wait() // The isolated baseline is deliberately killed.
	})
	select {
	case base := <-output.ready:
		return &runtime{base: base, dir: dir, cmd: cmd, client: &http.Client{Timeout: 30 * time.Second}}
	case <-time.After(30 * time.Second):
		t.Fatal("native baseline failed to listen")
		return nil
	}
}

func TestConfigNativeDiscoveryParity(t *testing.T) {
	a := configNativeFixture(t)
	t.Setenv("CONFIG_SENTINEL", "must-not-expand")
	calls := make(chan string, 16)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string
				Content any
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		var system strings.Builder
		for _, msg := range body.Messages {
			if msg.Role == "system" {
				fmt.Fprintln(&system, msg.Content)
			}
		}
		calls <- system.String()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"test\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"test\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n")
	}))
	defer model.Close()
	project := filepath.Join(a.Home, "project")
	dir := filepath.Join(project, "nested")
	put(t, filepath.Join(project, ".git", "HEAD"), "ref: refs/heads/main\n")
	put(t, filepath.Join(dir, "AGENTS.md"), "NESTED_NATIVE_INSTRUCTIONS")
	if out, err := exec.Command("git", "init", "--quiet", project).CombinedOutput(); err != nil {
		t.Fatalf("initializing isolated repository: %s %v", out, err)
	}
	put(t, filepath.Join(project, "AGENTS.md"), "ANCESTOR_NATIVE_INSTRUCTIONS")
	put(t, filepath.Join(dir, "CLAUDE.md"), "SHADOWED_PROJECT_CLAUDE")
	put(t, filepath.Join(a.ConfigDir, "AGENTS.md"), "GLOBAL_NATIVE_INSTRUCTIONS")
	put(t, filepath.Join(a.Home, ".claude", "CLAUDE.md"), "SHADOWED_GLOBAL_CLAUDE")
	put(t, filepath.Join(dir, "rules", "relative.md"), "RELATIVE_NATIVE_INSTRUCTIONS")
	put(t, filepath.Join(dir, "rules", "deep", "nested.md"), "RECURSIVE_NATIVE_INSTRUCTIONS")
	put(t, filepath.Join(a.Home, "rules", "home.md"), "HOME_NATIVE_INSTRUCTIONS")
	config := fmt.Sprintf(`{"$schema":"https://opencode.ai/config.json","model":"mock/test","small_model":"mock/test","instructions":["rules/**/*.md","~/rules/*.md"],"provider":{"mock":{"npm":"@ai-sdk/openai-compatible","options":{"apiKey":"test-only","baseURL":%q},"models":{"test":{"name":"Test"}}}},"agent":{"title":{"disable":true},"summary":{"disable":true}}}`, model.URL+"/v1")
	put(t, filepath.Join(a.ConfigDir, "opencode.json"), config)
	put(t, filepath.Join(a.Home, ".opencode", "bin", "opencode"), strings.Repeat("x", 17<<20))
	put(t, filepath.Join(project, ".opencode", "agents", "literal.md"), "---\nmode: primary\ndescription: '{env:CONFIG_SENTINEL}'\n---\nLITERAL_NATIVE_PROMPT {file:missing-literal.md}")
	for name, root := range map[string]string{
		"global-agents":  filepath.Join(a.Home, ".agents", "skills"),
		"global-claude":  filepath.Join(a.Home, ".claude", "skills"),
		"project-agents": filepath.Join(project, ".agents", "skills"),
		"project-claude": filepath.Join(dir, ".claude", "skills"),
		"config-skill":   filepath.Join(a.ConfigDir, "skills"),
		"project-skill":  filepath.Join(project, ".opencode", "skill"),
	} {
		put(t, filepath.Join(root, name, "SKILL.md"), fmt.Sprintf("---\nname: %s\ndescription: Native skill %s\n---\nSkill body", name, name))
	}
	tools := filepath.Join(project, ".opencode")
	put(t, filepath.Join(project, "shared.ts"), `export const prefix = 'NATIVE_TOOL_'`)
	put(t, filepath.Join(tools, "lib", "helper.ts"), `import { prefix } from '../../shared.ts'; import { value } from 'config-parity-dep'; export const description = prefix + value`)
	put(t, filepath.Join(tools, "node_modules", "config-parity-dep", "package.json"), `{"name":"config-parity-dep","type":"module","main":"index.js"}`)
	put(t, filepath.Join(tools, "node_modules", "config-parity-dep", "index.js"), `export const value = 'IMPORTED'`)
	put(t, filepath.Join(tools, "dep", "package.json"), `{"name":"config-parity-dep","version":"1.0.0","type":"module","main":"index.js"}`)
	put(t, filepath.Join(tools, "dep", "index.js"), `export const value = 'IMPORTED'`)
	put(t, filepath.Join(tools, "package.json"), `{"dependencies":{"config-parity-dep":"file:./dep"}}`)
	put(t, filepath.Join(tools, "tools", "parity.ts"), `import { tool } from '@opencode-ai/plugin'; import { description } from '../lib/helper.ts'; export default tool({ description, args: {}, execute: async () => 'ok' })`)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	managed, err := a.configured(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer a.finish(managed)
	var managedConfig struct{ Plugin []any }
	if err := managed.request(ctx, http.MethodGet, "/config", nil, &managedConfig); err != nil {
		t.Fatal(err)
	}
	if len(managedConfig.Plugin) != 2 || !strings.HasSuffix(pluginSpec(managedConfig.Plugin[0]), "/native-tools.mjs") || !slorpSpec(pluginSpec(managedConfig.Plugin[1])) {
		t.Fatalf("bundled plugin was rediscovered: %+v", managedConfig.Plugin)
	}
	var toolIDs []string
	if err := managed.request(ctx, http.MethodGet, "/experimental/tool/ids", nil, &toolIDs); err != nil {
		if output, ok := managed.cmd.Stdout.(*readyOutput); ok {
			output.mu.Lock()
			t.Logf("native output: %s", output.buf)
			output.mu.Unlock()
		}
		t.Fatal("managed tools before native has installed source dependencies:", err)
	}
	if !slices.Contains(toolIDs, "parity") {
		t.Fatalf("missing custom tool: %v", toolIDs)
	}
	if data, err := os.ReadFile(filepath.Join(tools, "package.json")); err != nil || string(data) != `{"dependencies":{"config-parity-dep":"file:./dep"}}` {
		t.Fatalf("managed runtime changed the source dependency manifest: %s %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(tools, "node_modules", "@opencode-ai", "plugin")); !os.IsNotExist(err) {
		t.Fatalf("managed runtime installed into the source tree: %v", err)
	}
	baseline := configBaseline(t, a, dir)
	for _, path := range []string{"/skill", "/experimental/tool/ids"} {
		var before, after any
		if err := baseline.request(ctx, http.MethodGet, path, nil, &before); err != nil {
			t.Fatal(err)
		}
		if err := managed.request(ctx, http.MethodGet, path, nil, &after); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(before)
		a, _ := json.Marshal(after)
		// Native discovery uses concurrent skill parsing; compare sorted entries.
		normalize := func(data []byte) []string {
			var list []json.RawMessage
			if err := json.Unmarshal(data, &list); err != nil {
				t.Fatal(err)
			}
			result := make([]string, len(list))
			for i, v := range list {
				result[i] = string(v)
			}
			slices.Sort(result)
			return result
		}
		if !reflect.DeepEqual(normalize(b), normalize(a)) {
			t.Fatalf("%s differs:\nbaseline %s\nmanaged %s", path, b, a)
		}
	}
	var prompts []string
	for _, r := range []*runtime{baseline, managed} {
		c, err := r.catalog(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if agent := c.agent("literal"); agent == nil || agent.Description != "{env:CONFIG_SENTINEL}" {
			t.Fatalf("catalog expanded markdown description: %+v", agent)
		}
		var session nativeSession
		if err := r.request(ctx, http.MethodPost, "/session", map[string]any{"title": "Config parity"}, &session); err != nil {
			t.Fatal(err)
		}
		body := map[string]any{"agent": "literal", "parts": []map[string]string{{"type": "text", "text": "Reply ok."}}}
		if err := r.request(ctx, http.MethodPost, "/session/"+session.ID+"/message", body, nil); err != nil {
			t.Fatal(err)
		}
		select {
		case prompt := <-calls:
			prompts = append(prompts, prompt)
		case <-time.After(10 * time.Second):
			t.Fatal("stub model received no request")
		}
	}
	if prompts[0] != prompts[1] {
		t.Fatalf("system prompt differs:\nbaseline %s\nmanaged %s", prompts[0], prompts[1])
	}
	for _, want := range []string{"GLOBAL_NATIVE_INSTRUCTIONS", "NESTED_NATIVE_INSTRUCTIONS", "RELATIVE_NATIVE_INSTRUCTIONS", "RECURSIVE_NATIVE_INSTRUCTIONS", "HOME_NATIVE_INSTRUCTIONS", "{file:missing-literal.md}"} {
		if !strings.Contains(prompts[1], want) {
			t.Errorf("missing native prompt input %s", want)
		}
	}
	if strings.Contains(prompts[1], "SHADOWED_") || strings.Contains(prompts[1], "must-not-expand") {
		t.Fatal("shadowed instructions or expanded sentinel reached model")
	}
}

func TestConfigNativeRemoteSlorpRejected(t *testing.T) {
	a := configNativeFixture(t)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"config":{"plugin":["opencode-slorp"]}}`)
	}))
	defer remote.Close()
	put(t, filepath.Join(os.Getenv("XDG_DATA_HOME"), "opencode", "auth.json"), fmt.Sprintf(`{%q:{"type":"wellknown","key":"TEST_REMOTE_TOKEN","token":"isolated-test"}}`, remote.URL))
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := a.configured(ctx, a.Home)
	if err == nil || !strings.Contains(err.Error(), "remote-origin Slorp") {
		t.Fatalf("remote Slorp must fail before the managed plugin loads: %v", err)
	}
}

func TestConfigNativeDisableFlags(t *testing.T) {
	a := configNativeFixture(t)
	put(t, filepath.Join(a.Home, ".claude", "CLAUDE.md"), "DISABLED_CLAUDE_PROMPT")
	for _, name := range []string{".claude", ".agents"} {
		put(t, filepath.Join(a.Home, name, "skills", "external", "SKILL.md"), "---\nname: external\ndescription: External skill\n---\nbody")
	}
	put(t, filepath.Join(a.Home, ".opencode", "skills", "native", "SKILL.md"), "---\nname: native\ndescription: Native skill\n---\nbody")
	project := filepath.Join(a.Home, "project")
	put(t, filepath.Join(project, "opencode.json"), `{"agent":{"excluded":{"mode":"primary"}}}`)
	put(t, filepath.Join(project, "AGENTS.md"), "DISABLED_PROJECT_PROMPT")
	t.Setenv("OPENCODE_DISABLE_PROJECT_CONFIG", "true")
	t.Setenv("OPENCODE_DISABLE_EXTERNAL_SKILLS", "true")
	t.Setenv("OPENCODE_DISABLE_CLAUDE_CODE_PROMPT", "true")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	baseline := configBaseline(t, a, project)
	managed, err := a.configured(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	defer a.finish(managed)
	for _, r := range []*runtime{baseline, managed} {
		var skills []struct{ Name string }
		if err := r.request(ctx, http.MethodGet, "/skill", nil, &skills); err != nil {
			t.Fatal(err)
		}
		if slices.ContainsFunc(skills, func(s struct{ Name string }) bool { return s.Name == "external" }) || !slices.ContainsFunc(skills, func(s struct{ Name string }) bool { return s.Name == "native" }) {
			t.Fatalf("disable flags discarded native skills or included external skills: %+v", skills)
		}
		var config struct {
			Instructions []string
			Agent        map[string]any
		}
		if err := r.request(ctx, http.MethodGet, "/config", nil, &config); err != nil {
			t.Fatal(err)
		}
		if len(config.Instructions) != 0 || config.Agent["excluded"] != nil {
			t.Fatalf("disabled discovery restored: %+v", config)
		}
	}
}

func TestConfigNativeModelDefaults(t *testing.T) {
	a := configNativeFixture(t)
	project := filepath.Join(a.Home, "project")
	put(t, filepath.Join(project, "AGENTS.md"), "isolated model defaults")
	for _, tc := range []struct{ name, model, recent, agent, want string }{
		{"recent later provider", "", "z", "build", "z/test"},
		{"configured fallback", "", "missing", "build", "a/test"},
		{"explicit model", "a/test", "z", "build", "a/test"},
		{"agent model", "z/test", "z", "review", "a/test"},
		{"other agent base model", "z/test", "z", "build", "z/test"},
		{"declaration order fallback", "", "missing", "build", "z/test"},
		{"custom root order", "a/test", "missing", "", "a/test"},
		{"explicit missing", "gone/test", "z", "build", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := map[string]any{
				"$schema": "https://opencode.ai/config.json", "enabled_providers": []string{"a", "z"}, "default_agent": "review",
				"agent": map[string]any{"review": map[string]any{"mode": "primary", "model": "a/test"}},
			}
			if tc.model != "" {
				config["model"] = tc.model
			}
			if tc.name == "custom root order" {
				delete(config, "default_agent")
				config["agent"] = json.RawMessage(`{"build":{"disable":true},"plan":{"disable":true},"z-root":{"mode":"primary"},"a-root":{"mode":"primary"}}`)
			}
			providers := map[string]any{}
			for _, name := range []string{"a", "z"} {
				providers[name] = map[string]any{"npm": "@ai-sdk/openai-compatible", "options": map[string]any{"apiKey": "isolated", "baseURL": "http://127.0.0.1:1/v1"}, "models": map[string]any{"test": map[string]any{"name": "Test"}}}
			}
			config["provider"] = providers
			if tc.name == "declaration order fallback" {
				first, err := json.Marshal(providers["z"])
				if err != nil {
					t.Fatal(err)
				}
				second, err := json.Marshal(providers["a"])
				if err != nil {
					t.Fatal(err)
				}
				config["provider"] = json.RawMessage(`{"z":` + string(first) + `,"a":` + string(second) + `}`)
			}
			data, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			put(t, filepath.Join(a.ConfigDir, "opencode.json"), string(data))
			put(t, filepath.Join(os.Getenv("XDG_STATE_HOME"), "opencode", "model.json"), fmt.Sprintf(`{"recent":[{"providerID":%q,"modelID":"test"}]}`, tc.recent))
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			baseline := configBaseline(t, a, project)
			var session nativeSession
			if err := baseline.request(ctx, http.MethodPost, "/session", map[string]any{"title": "Model default parity"}, &session); err != nil {
				t.Fatal(err)
			}
			var result struct {
				Info struct {
					Agent string
					Model struct{ ProviderID, ModelID string }
				}
			}
			body := map[string]any{"agent": tc.agent, "noReply": true, "parts": []map[string]string{{"type": "text", "text": "No inference requested."}}}
			if tc.agent == "" {
				delete(body, "agent")
			}
			requestErr := baseline.request(ctx, http.MethodPost, "/session/"+session.ID+"/message", body, &result)
			if tc.name == "explicit missing" {
				if requestErr == nil && result.Info.Model.ProviderID+"/"+result.Info.Model.ModelID != tc.model {
					t.Fatal("native silently replaced an unavailable explicit model")
				}
			} else if requestErr != nil {
				t.Fatal(requestErr)
			}
			native := result.Info.Model.ProviderID + "/" + result.Info.Model.ModelID
			if tc.name != "explicit missing" && native != tc.want {
				t.Fatalf("native default %s, want %s", native, tc.want)
			}
			managed, err := a.configured(ctx, project)
			if err != nil {
				t.Fatal(err)
			}
			defer a.finish(managed)
			catalog, err := managed.catalog(ctx)
			if err != nil {
				t.Fatal(err)
			}
			launch := hub.Launch{AgentName: tc.agent}
			choiceErr := catalog.choose(&launch)
			if tc.name == "explicit missing" {
				if !errors.Is(choiceErr, hub.ErrLaunchInvalid) {
					t.Fatalf("managed accepted an unavailable explicit model: %v", choiceErr)
				}
				return
			}
			if choiceErr != nil || launch.Model != native {
				t.Fatalf("managed choice %+v %v; native %s", launch, choiceErr, native)
			}
			if launch.AgentName != result.Info.Agent {
				t.Fatalf("managed root %s; native %s", launch.AgentName, result.Info.Agent)
			}
		})
	}
}
