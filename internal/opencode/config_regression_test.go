package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
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
	put(t, filepath.Join(a.Home, ".opencode", "opencode.jsonc"), `{"agents":{"json":{"system":"{file:prompt.txt}"}}}`)
	r := &runtime{private: t.TempDir(), dir: a.Home, cmd: a.command(context.Background())}
	s, err := a.snapshotConfig(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if s.bytes > 1024 {
		t.Fatalf("copied unrelated files: %d bytes", s.bytes)
	}
	base := filepath.Join(r.dir, ".opencode")
	data, err := os.ReadFile(filepath.Join(base, "agents", "literal.md"))
	if err != nil || string(data) != md {
		t.Fatalf("markdown changed: %q, %v", data, err)
	}
	data, err = os.ReadFile(filepath.Join(base, "opencode.jsonc"))
	if err != nil || !strings.Contains(string(data), filepath.Join(a.Home, ".opencode", "prompt.txt")) {
		t.Fatalf("JSON file reference was not rebased: %q, %v", data, err)
	}
}

func TestConfigSnapshotIgnoresReplacedGlobalDirectory(t *testing.T) {
	a := fixture(t)
	put(t, filepath.Join(a.ConfigDir, "opencode.json"), strings.Repeat("x", 17<<20))
	a.ExtraConfigDir = filepath.Join(a.Home, "replacement")
	put(t, filepath.Join(a.ExtraConfigDir, "opencode.json"), `{"default_agent":"build"}`)
	r := &runtime{private: t.TempDir(), dir: a.Home, cmd: a.command(context.Background())}
	snapshot, err := a.snapshotConfig(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.bytes > 1024 {
		t.Fatalf("inactive global configuration was copied: %d bytes", snapshot.bytes)
	}
}

func TestConfigResolvedTemplatesAndRemoteOrigin(t *testing.T) {
	a := fixture(t)
	t.Setenv("OPENCODE_TEST_HOME", "")
	t.Setenv("OPENCODE_AUTH_CONTENT", `{"retired":{"type":"wellknown"}}`)
	r := &runtime{private: t.TempDir(), dir: a.Home, cmd: a.command(context.Background())}
	literal := `{env:CONFIG_SENTINEL} {file:missing-literal.md} \u007benv:already-escaped}`
	config := map[string]any{
		"agents":  map[string]any{"literal": map[string]any{"system": literal}},
		"plugins": []any{"opencode-slorp"},
	}
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
		Agents map[string]struct{ System string }
	}
	if err := json.Unmarshal(data, &got); err != nil || got.Agents["literal"].System != literal {
		t.Fatalf("literal did not round trip: %+v %v", got, err)
	}
	for _, spec := range []any{"opencode-slorp@2.0.0", map[string]any{"package": "opencode-slorp", "options": map[string]any{}}} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if err := json.NewEncoder(w).Encode([]nativeConfigEntry{{Type: "document", Info: map[string]any{"plugins": []any{spec}}}}); err != nil {
				t.Error(err)
			}
		}))
		r := &runtime{base: server.URL, dir: a.Home, client: server.Client()}
		_, err := r.resolvedConfig(context.Background())
		server.Close()
		if err == nil || !strings.Contains(err.Error(), "remote-origin or inline Slorp") {
			t.Fatalf("remote Slorp should fail explicitly: %v", err)
		}
	}
}

func TestConfigPreservesPackageExportOrder(t *testing.T) {
	original := json.RawMessage(`{"exports":{".":{"import":"./import.js","default":"./default.js"}}}`)
	var config map[string]any
	if err := json.Unmarshal(original, &config); err != nil {
		t.Fatal(err)
	}
	config["private"] = true
	data, err := marshalConfigOrder(config, original)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"import":"./import.js","default":"./default.js"`) {
		t.Fatalf("native insertion order lost: %s", data)
	}
}

func TestConfigCatalogNativeDefaults(t *testing.T) {
	for _, tc := range []struct {
		name, model, defaultAgent, want string
		invalid                         bool
	}{
		{name: "native default overrides alphabetical order", want: "z/m"},
		{name: "explicit wins", model: "a/m", want: "a/m"},
		{name: "explicit missing fails", model: "missing/m", want: "missing/m", invalid: true},
		{name: "agent override", model: "z/m", defaultAgent: "review", want: "a/m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/agent":
					if tc.defaultAgent == "review" {
						fmt.Fprint(w, `{"data":[{"id":"review","mode":"primary","model":{"providerID":"a","id":"m"}},{"id":"build","mode":"primary"}]}`)
					} else {
						fmt.Fprint(w, `{"data":[{"id":"build","mode":"primary"},{"id":"review","mode":"primary","model":{"providerID":"a","id":"m"}}]}`)
					}
				case "/api/provider":
					fmt.Fprint(w, `{"data":[{"id":"a"},{"id":"z"}]}`)
				case "/api/model":
					fmt.Fprint(w, `{"data":[{"providerID":"a","id":"m","enabled":true,"capabilities":{"input":["text"],"output":["text"]}},{"providerID":"z","id":"m","enabled":true,"capabilities":{"input":["text"],"output":["text"]}}]}`)
				case "/api/model/default":
					model := tc.model
					if model == "" {
						model = "z/m"
					}
					provider, id, _ := strings.Cut(model, "/")
					fmt.Fprintf(w, `{"data":{"providerID":%q,"id":%q}}`, provider, id)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			r := &runtime{base: server.URL, dir: t.TempDir(), client: server.Client()}
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
