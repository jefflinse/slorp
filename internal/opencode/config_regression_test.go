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
	"strings"
	"testing"

	"github.com/jefflinse/slorp/internal/hub"
)

func TestConfigSnapshotDiscoveryOnly(t *testing.T) {
	a := fixture(t)
	put(t, filepath.Join(a.Home, ".opencode", "bin", "opencode"), strings.Repeat("x", 17<<20))
	put(t, filepath.Join(a.Home, ".opencode", "resources", "large"), strings.Repeat("x", 17<<20))
	md := "---\nmode: primary\ndescription: '{env:CONFIG_SENTINEL}'\n---\n{file:missing-literal.md}"
	put(t, filepath.Join(a.Home, ".opencode", "agents", "literal.md"), md)
	put(t, filepath.Join(a.Home, ".opencode", "opencode.jsonc"), `{"agent":{"json":{"prompt":"{file:prompt.txt}"}}}`)
	r := &runtime{private: t.TempDir(), dir: a.Home, cmd: a.command(context.Background())}
	s, err := a.snapshotConfig(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if s.bytes > 1024 {
		t.Fatalf("copied unrelated files: %d bytes", s.bytes)
	}
	base := filepath.Join(r.private, "sources", "home", ".opencode")
	data, err := os.ReadFile(filepath.Join(base, "agents", "literal.md"))
	if err != nil || string(data) != md {
		t.Fatalf("markdown changed: %q, %v", data, err)
	}
	data, err = os.ReadFile(filepath.Join(base, "opencode.jsonc"))
	if err != nil || !strings.Contains(string(data), filepath.Join(a.Home, ".opencode", "prompt.txt")) {
		t.Fatalf("JSON file reference was not rebased: %q, %v", data, err)
	}
}

func TestConfigResolvedTemplatesAndRemoteOrigin(t *testing.T) {
	a := fixture(t)
	t.Setenv("OPENCODE_TEST_HOME", "")
	r := &runtime{private: t.TempDir(), dir: a.Home, cmd: a.command(context.Background())}
	literal := `{env:CONFIG_SENTINEL} {file:missing-literal.md} \u007benv:already-escaped}`
	config := map[string]any{"agent": map[string]any{"literal": map[string]any{"prompt": literal}}}
	if err := a.runtimeConfig(r, config); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(r.private, "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "{env:") || strings.Contains(string(data), "{file:") {
		t.Fatalf("resolved template left expandable: %s", data)
	}
	var got struct {
		Agent map[string]struct{ Prompt string }
	}
	if err := json.Unmarshal(data, &got); err != nil || got.Agent["literal"].Prompt != literal {
		t.Fatalf("literal did not round trip: %+v %v", got, err)
	}
	for _, spec := range []any{"opencode-slorp@1.0.0", []any{"opencode-slorp", map[string]any{}}} {
		r := &runtime{private: t.TempDir(), dir: a.Home, cmd: a.command(context.Background())}
		cfg := map[string]any{"plugin_origins": []any{map[string]any{"source": "http://127.0.0.1/.well-known/opencode", "spec": spec}}}
		if err := a.runtimeConfig(r, cfg); err == nil || !strings.Contains(err.Error(), "remote-origin Slorp") {
			t.Fatalf("remote Slorp should fail explicitly: %v", err)
		}
	}
}

func TestConfigPreservesNativeObjectOrder(t *testing.T) {
	original := json.RawMessage(`{"provider":{"z":{},"a":{}},"permission":{"bash":{"*":"deny","git *":"allow"}},"agent":{"z":{"mode":"primary"},"a":{"mode":"primary"}}}`)
	var config map[string]any
	if err := json.Unmarshal(original, &config); err != nil {
		t.Fatal(err)
	}
	config["model"] = "z/test"
	data, err := marshalConfigOrder(config, original)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"provider":{"z":{},"a":{}}`) || !strings.Contains(string(data), `"bash":{"*":"deny","git *":"allow"}`) || !strings.Contains(string(data), `"agent":{"z":{"mode":"primary"},"a":{"mode":"primary"}}`) {
		t.Fatalf("native insertion order lost: %s", data)
	}
}

func TestConfigCatalogNativeDefaults(t *testing.T) {
	for _, tc := range []struct {
		name, model, recent, configured, defaultAgent, want string
		invalid                                             bool
	}{
		{name: "recent later provider", recent: `{"recent":[null,{"providerID":"gone","modelID":"m"},{"providerID":"z","modelID":"m"}]}`, want: "z/m"},
		{name: "configured provider fallback", configured: `{"z":{}}`, want: "z/m"},
		{name: "native connection order", want: "z/m"},
		{name: "explicit wins", model: "a/m", recent: `{"recent":[{"providerID":"z","modelID":"m"}]}`, want: "a/m"},
		{name: "explicit missing fails", model: "missing/m", want: "missing/m", invalid: true},
		{name: "agent override", model: "z/m", defaultAgent: "review", want: "a/m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := t.TempDir()
			if tc.recent != "" {
				put(t, filepath.Join(state, "opencode", "model.json"), tc.recent)
			}
			if tc.configured == "" {
				tc.configured = `{}`
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/agent":
					fmt.Fprint(w, `[{"name":"build","mode":"primary"},{"name":"review","mode":"primary","model":{"providerID":"a","modelID":"m"}}]`)
				case "/config":
					fmt.Fprintf(w, `{"model":%q,"default_agent":%q,"provider":%s}`, tc.model, tc.defaultAgent, tc.configured)
				case "/provider":
					fmt.Fprint(w, `{"connected":["z","a"],"default":{"z":"m","a":"m"},"all":[{"id":"a","models":{"m":{"capabilities":{"input":{"text":true},"output":{"text":true}}}}},{"id":"z","models":{"m":{"capabilities":{"input":{"text":true},"output":{"text":true}}}}}]}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			r := &runtime{base: server.URL, dir: t.TempDir(), client: server.Client(), cmd: &exec.Cmd{Env: []string{"XDG_STATE_HOME=" + state}}}
			c, err := r.catalog(context.Background())
			if err != nil || c.Defaults.Model != tc.want {
				t.Fatalf("default = %+v, err %v; want %s", c.Defaults, err, tc.want)
			}
			l := hub.Launch{}
			err = c.choose(&l)
			if tc.invalid {
				if !errors.Is(err, hub.ErrLaunchInvalid) {
					t.Fatalf("missing explicit model silently fell back: %+v %v", l, err)
				}
			} else if err != nil || l.Model != tc.want {
				t.Fatalf("choose: %+v %v", l, err)
			}
			if tc.defaultAgent == "review" {
				other := hub.Launch{AgentName: "build"}
				if err := c.choose(&other); err != nil || other.Model != "z/m" {
					t.Fatalf("default agent's model leaked to build: %+v %v", other, err)
				}
			}
		})
	}
}
