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
	"strings"
	"testing"
	"time"
)

func configNativeFixture(t *testing.T) *Adapter {
	t.Helper()
	if os.Getenv("SLORP_NATIVE_TEST") != "1" {
		t.Skip("set SLORP_NATIVE_TEST=1 for isolated native V2 config parity tests")
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
	if err := a.requireV2(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Shutdown)
	return a
}

func configBaseline(t *testing.T, a *Adapter, dir string) *runtime {
	t.Helper()
	cmd := a.command(context.Background(), "serve", "--hostname", "127.0.0.1", "--port", "0")
	password := opaque()
	replaceEnv(cmd, "OPENCODE_PASSWORD", password)
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
		return &runtime{base: base, password: password, dir: dir, cmd: cmd, client: &http.Client{Timeout: 30 * time.Second}}
	case <-time.After(30 * time.Second):
		t.Fatal("native baseline failed to listen")
		return nil
	}
}

type configPrompt struct{ System, Text string }

func configPromptModel(t *testing.T) (*httptest.Server, <-chan configPrompt) {
	t.Helper()
	calls := make(chan configPrompt, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		var prompt configPrompt
		for _, message := range body.Messages {
			if message.Role == "system" {
				prompt.System += fmt.Sprint(message.Content) + "\n"
			}
			if message.Role == "user" {
				prompt.Text += fmt.Sprint(message.Content) + "\n"
			}
		}
		select {
		case calls <- prompt:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"test\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"test\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	return server, calls
}

func configPromptOnce(t *testing.T, ctx context.Context, r *runtime, calls <-chan configPrompt, command string) configPrompt {
	t.Helper()
	var session nativeSession
	if err := r.request(ctx, http.MethodPost, "/api/session", map[string]any{"location": map[string]string{"directory": r.dir}, "agent": "literal"}, &session); err != nil {
		t.Fatal(err)
	}
	path := "/api/session/" + session.ID + "/prompt"
	body := map[string]string{"text": "Reply ok."}
	if command != "" {
		path = "/api/session/" + session.ID + "/command"
		body = map[string]string{"name": command, "text": ""}
	}
	if err := r.request(ctx, http.MethodPost, path, body, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case prompt := <-calls:
		return prompt
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		return configPrompt{}
	}
}

func TestConfigNativeDiscoveryParity(t *testing.T) {
	a := configNativeFixture(t)
	t.Setenv("CONFIG_SENTINEL", "must-not-expand")
	model, calls := configPromptModel(t)
	configToolProvider(t, a, model.URL, nil)
	project := filepath.Join(a.Home, "project")
	dir := filepath.Join(project, "nested")
	put(t, filepath.Join(dir, "AGENTS.md"), "NESTED_NATIVE_INSTRUCTIONS")
	put(t, filepath.Join(dir, "CLAUDE.md"), "SHADOWED_PROJECT_CLAUDE")
	put(t, filepath.Join(project, "AGENTS.md"), "ANCESTOR_NATIVE_INSTRUCTIONS")
	put(t, filepath.Join(a.Home, "CLAUDE.md"), "HOME_FALLBACK_INSTRUCTIONS")
	put(t, filepath.Join(a.ConfigDir, "AGENTS.md"), "GLOBAL_NATIVE_INSTRUCTIONS")
	put(t, filepath.Join(a.Home, ".claude", "CLAUDE.md"), "SHADOWED_GLOBAL_CLAUDE")
	put(t, filepath.Join(dir, "rules", "deep", "rule.md"), "RECURSIVE_NATIVE_INSTRUCTIONS")
	put(t, filepath.Join(a.Home, "rules", "deep", "rule.md"), "OUTSIDE_PROJECT_PATTERN")
	if out, err := exec.Command("git", "init", "--quiet", project).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v", out, err)
	}
	put(t, filepath.Join(project, "opencode.json"), `{"instructions":["rules/**/*.md"],"commands":{"precedence":{"template":"PROJECT_JSON"}}}`)
	put(t, filepath.Join(project, ".opencode", "agents", "literal.md"), "---\nmode: primary\ndescription: '{env:CONFIG_SENTINEL}'\n---\nLITERAL_NATIVE_PROMPT {file:missing-literal.md}")
	put(t, filepath.Join(project, ".opencode", "commands", "precedence.md"), "---\ndescription: Directory command\n---\nDIRECTORY_COMMAND")
	put(t, filepath.Join(a.ConfigDir, "commands", "precedence.md"), "GLOBAL_COMMAND")
	t.Setenv("OPENCODE_CONFIG_CONTENT", `{"commands":{"precedence":{"description":"Inline wins","template":"INLINE_COMMAND"}}}`)
	for name, root := range map[string]string{
		"global-agents":  filepath.Join(a.Home, ".agents", "skills"),
		"global-claude":  filepath.Join(a.Home, ".claude", "skills"),
		"project-agents": filepath.Join(project, ".agents", "skills"),
		"project-claude": filepath.Join(dir, ".claude", "skills"),
		"config-skill":   filepath.Join(a.ConfigDir, "skills"),
		"project-skill":  filepath.Join(project, ".opencode", "skills"),
	} {
		put(t, filepath.Join(root, name, "SKILL.md"), fmt.Sprintf("---\nname: %s\ndescription: %s\n---\nSkill body", name, name))
	}
	beforeFiles := configSourceHashes(t, a.ConfigDir, project, filepath.Join(a.Home, ".agents"), filepath.Join(a.Home, ".claude"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	managed, err := a.configured(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer a.finish(managed)
	baseline := configBaseline(t, a, dir)
	beforeSkills := configNativeSkills(t, ctx, baseline)
	afterSkills := configNativeSkills(t, ctx, managed)
	if !reflect.DeepEqual(beforeSkills, afterSkills) {
		t.Fatalf("skills differ: native %+v; managed %+v", beforeSkills, afterSkills)
	}
	for _, r := range []*runtime{baseline, managed} {
		c, err := r.catalog(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if agent := c.agent("literal"); agent == nil || agent.Description != "{env:CONFIG_SENTINEL}" {
			t.Fatalf("literal agent changed: %+v", agent)
		}
		prompt := configPromptOnce(t, ctx, r, calls, "precedence")
		for _, want := range []string{"NESTED_NATIVE_INSTRUCTIONS", "ANCESTOR_NATIVE_INSTRUCTIONS", "HOME_FALLBACK_INSTRUCTIONS", "GLOBAL_NATIVE_INSTRUCTIONS", "RECURSIVE_NATIVE_INSTRUCTIONS", "{file:missing-literal.md}"} {
			if !strings.Contains(prompt.System, want) {
				t.Errorf("missing %s in system prompt: %s", want, prompt.System)
			}
		}
		if strings.Contains(prompt.System, "SHADOWED_") || strings.Contains(prompt.System, "must-not-expand") || strings.Contains(prompt.System, "OUTSIDE_PROJECT_PATTERN") {
			t.Fatal("shadowed or expanded instructions reached model")
		}
		if !strings.Contains(prompt.Text, "INLINE_COMMAND") || strings.Contains(prompt.Text, "DIRECTORY_COMMAND") || strings.Contains(prompt.Text, "PROJECT_JSON") {
			t.Fatalf("command precedence: %s", prompt.Text)
		}
	}
	if after := configSourceHashes(t, a.ConfigDir, project, filepath.Join(a.Home, ".agents"), filepath.Join(a.Home, ".claude")); !reflect.DeepEqual(beforeFiles, after) {
		t.Fatal("discovery changed source files")
	}
}

func TestConfigNativeRemoteSlorpRejected(t *testing.T) {
	a := configNativeFixture(t)
	t.Setenv("OPENCODE_CONFIG_CONTENT", `{"plugins":["opencode-slorp"]}`)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := a.configured(ctx, a.Home)
	if err == nil || !strings.Contains(err.Error(), "remote-origin or inline") {
		t.Fatalf("originless Slorp must fail before plugin loading: %v", err)
	}
}

func TestConfigNativeDisableFlags(t *testing.T) {
	a := configNativeFixture(t)
	model, calls := configPromptModel(t)
	configToolProvider(t, a, model.URL, nil)
	put(t, filepath.Join(a.ConfigDir, "agents", "literal.md"), "---\nmode: primary\n---\nFlag parity fixture")
	put(t, filepath.Join(a.ConfigDir, "skills", "native", "SKILL.md"), "---\nname: native\ndescription: native\n---\nbody")
	project := filepath.Join(a.Home, "project")
	put(t, filepath.Join(project, "opencode.json"), `{"agents":{"excluded":{"mode":"primary"}}}`)
	put(t, filepath.Join(project, "AGENTS.md"), "PROJECT_FLAG_INSTRUCTIONS")
	put(t, filepath.Join(a.Home, ".agents", "skills", "external", "SKILL.md"), "---\nname: external\ndescription: external\n---\nbody")
	t.Setenv("OPENCODE_DISABLE_PROJECT_CONFIG", "true")
	t.Setenv("OPENCODE_DISABLE_CLAUDE_CODE_PROMPT", "true")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	baseline := configBaseline(t, a, project)
	managed, err := a.configured(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	defer a.finish(managed)
	var instructionPresence []bool
	for _, r := range []*runtime{baseline, managed} {
		skills := configNativeSkills(t, ctx, r)
		if _, ok := skills["external"]; !ok {
			t.Fatal("project flag lost a global compatibility skill")
		}
		if _, ok := skills["native"]; !ok {
			t.Fatal("native skill lost")
		}
		c, err := r.catalog(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if c.agent("excluded") != nil {
			t.Fatal("disabled project config restored")
		}
		prompt := configPromptOnce(t, ctx, r, calls, "")
		instructionPresence = append(instructionPresence, strings.Contains(prompt.System, "PROJECT_FLAG_INSTRUCTIONS"))
	}
	if instructionPresence[0] != instructionPresence[1] {
		t.Fatalf("project instruction flag parity: native %v, managed %v", instructionPresence[0], instructionPresence[1])
	}
}

func TestConfigNativeModelDefaults(t *testing.T) {
	a := configNativeFixture(t)
	project := filepath.Join(a.Home, "project")
	put(t, filepath.Join(project, "AGENTS.md"), "Model defaults fixture")
	for _, selection := range []string{"a/test", "z/test"} {
		t.Run(selection, func(t *testing.T) {
			config := map[string]any{"model": selection, "default_agent": "review", "agents": map[string]any{"review": map[string]any{"mode": "primary", "model": "a/test#high"}}, "providers": map[string]any{}}
			for _, id := range []string{"a", "z"} {
				config["providers"].(map[string]any)[id] = map[string]any{"package": "aisdk:@ai-sdk/openai-compatible", "settings": map[string]string{"apiKey": "test", "baseURL": "http://127.0.0.1:1/v1"}, "models": map[string]any{"test": map[string]any{"name": "Test", "variants": []map[string]string{{"id": "high"}}}}}
			}
			data, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			put(t, filepath.Join(a.ConfigDir, "opencode.json"), string(data))
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			baseline := configBaseline(t, a, project)
			managed, err := a.configured(ctx, project)
			if err != nil {
				t.Fatal(err)
			}
			defer a.finish(managed)
			before, err := baseline.catalog(ctx)
			if err != nil {
				t.Fatal(err)
			}
			after, err := managed.catalog(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before.LaunchOptions, after.LaunchOptions) || before.baseDefault != after.baseDefault {
				t.Fatalf("catalog mismatch: native %+v; managed %+v", before, after)
			}
			if after.baseDefault != selection || after.Defaults.Model != "a/test" || after.Defaults.Variant != "high" {
				t.Fatal(after)
			}
		})
	}
}

func TestConfigNativePolicyAuthority(t *testing.T) {
	a := configNativeFixture(t)
	project := filepath.Join(a.Home, "project")
	put(t, filepath.Join(project, "opencode.json"), `{"policies":[{"action":"provider.use","resource":"mock","effect":"allow"}]}`)
	configToolProvider(t, a, "http://127.0.0.1:1", nil)
	data, err := os.ReadFile(filepath.Join(a.ConfigDir, "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	config["policies"] = []map[string]string{{"action": "provider.use", "resource": "mock", "effect": "deny"}}
	data, err = json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(a.ConfigDir, "opencode.json"), string(data))
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	baseline := configBaseline(t, a, project)
	managed, err := a.configured(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	defer a.finish(managed)
	for _, r := range []*runtime{baseline, managed} {
		c, err := r.catalog(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range c.Models {
			if strings.HasPrefix(m.ID, "mock/") {
				t.Fatal("project lifted global provider policy")
			}
		}
	}
}
