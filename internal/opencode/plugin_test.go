package opencode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/hub"
	"github.com/jefflinse/slorp/internal/testdb"
)

func put(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func fixture(t *testing.T) *Adapter {
	t.Helper()
	root := t.TempDir()
	st := testdb.Open(t)
	a := &Adapter{Home: root, Data: filepath.Join(root, "slorp"), ConfigDir: filepath.Join(root, "config", "opencode"), URL: "http://127.0.0.1:5858", Bin: filepath.Join(root, "opencode"), Hub: hub.New(testdb.KB(t, st), st)}
	put(t, a.Bin, `#!/bin/sh
test "$1" = plugin && test "$3" = --global || exit 4
mkdir -p "$XDG_CONFIG_HOME/opencode"
printf '{"plugin":["%s"]}' "$2" > "$XDG_CONFIG_HOME/opencode/opencode.json"
printf '{"plugin":["%s"]}' "$2" > "$XDG_CONFIG_HOME/opencode/tui.json"
`)
	if err := os.Chmod(a.Bin, 0o700); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestBundledInstallWithoutCheckout(t *testing.T) {
	a := fixture(t)
	ctx := context.Background()
	if p, err := a.inspect(); err != nil || p.Installed {
		t.Fatalf("before install: %+v, %v", p, err)
	}
	if err := a.Install(ctx); err != nil {
		t.Fatal(err)
	}
	p, err := a.inspect()
	if err != nil || !p.Installed || p.NeedsSetup || p.Source != "managed" || p.UpdateAvailable || p.Revision == "" {
		t.Fatalf("installed: %+v, %v", p, err)
	}
	b, _ := os.ReadFile(filepath.Join(p.Path, "server.ts"))
	if !strings.Contains(string(b), a.URL) {
		t.Fatal("managed plugin did not receive the service address")
	}
	if _, err := os.Stat(filepath.Join(p.Path, "tui.ts")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p.Path, "presence.ts")); err != nil {
		t.Fatal(err)
	}
	manifestBytes, _ := os.ReadFile(filepath.Join(p.Path, "package.json"))
	if strings.Contains(string(manifestBytes), "devDependencies") {
		t.Fatal("installed development dependencies")
	}
	if err := a.Install(ctx); err != nil {
		t.Fatal("idempotent install:", err)
	}
	if err := a.Uninstall(ctx); err != nil {
		t.Fatal(err)
	}
	if p, err := a.inspect(); err != nil || p.Installed {
		t.Fatalf("uninstalled: %+v, %v", p, err)
	}
	if err := a.Install(ctx); err != nil {
		t.Fatal("reinstall:", err)
	}
}

func TestManagedUpdatesFollowBundledContentAndAddress(t *testing.T) {
	a := fixture(t)
	ctx := context.Background()
	if err := a.Install(ctx); err != nil {
		t.Fatal(err)
	}
	before, _ := a.inspect()
	put(t, filepath.Join(a.managedPath(), "slorp.ts"), "old plugin")
	p, err := a.inspect()
	if err != nil || !p.UpdateAvailable || p.Latest == "" {
		t.Fatalf("outdated: %+v %v", p, err)
	}
	if err := a.Update(ctx); err != nil {
		t.Fatal(err)
	}
	p, _ = a.inspect()
	if p.UpdateAvailable || p.Revision != before.Revision {
		t.Fatalf("updated: %+v", p)
	}
	a.URL = "http://127.0.0.1:6000"
	p, _ = a.inspect()
	if !p.UpdateAvailable {
		t.Fatal("an address change did not require updating the wrapper")
	}
	if err := a.Update(ctx); err != nil {
		t.Fatal(err)
	}
	p, _ = a.inspect()
	if p.UpdateAvailable || p.Revision == before.Revision {
		t.Fatalf("address update: %+v", p)
	}
}

func TestBundledPresenceAffectsRevisionAndUpdateDetection(t *testing.T) {
	a := fixture(t)
	if err := a.writeBundle(); err != nil {
		t.Fatal(err)
	}
	files, version, err := bundled(a.URL)
	if err != nil {
		t.Fatal(err)
	}
	if version != "0.5.0" || len(files["presence.ts"]) == 0 {
		t.Fatalf("missing presence runtime or version: %q", version)
	}
	written, err := os.ReadFile(filepath.Join(a.managedPath(), "presence.ts"))
	if err != nil || string(written) != string(files["presence.ts"]) {
		t.Fatalf("installed presence differs from embedded runtime: %v", err)
	}
	before := fingerprint(files)
	files["presence.ts"] = []byte("changed protocol")
	if fingerprint(files) == before {
		t.Fatal("presence changes did not change the runtime revision")
	}
	if err := a.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(a.managedPath(), "presence.ts"), "old protocol")
	p, err := a.inspect()
	if err != nil || !p.UpdateAvailable || p.Revision == before {
		t.Fatalf("presence change was not detected: %+v, %v", p, err)
	}
}

func TestDetectLocalPackageAndFinishCompanionSetup(t *testing.T) {
	a := fixture(t)
	path := filepath.Join(a.Home, "checkout", "integrations", "opencode")
	put(t, filepath.Join(path, "package.json"), `{"name":"opencode-slorp","version":"dev","exports":{"./tui":"./tui.ts"}}`)
	put(t, filepath.Join(path, "tui.ts"), "export default {}")
	put(t, filepath.Join(a.ConfigDir, "opencode.jsonc"), `{
  // Keep this comment and the custom options.
  "plugin": [["../../checkout/integrations/opencode", {"url":"http://127.0.0.1:5858"}]],
}`)
	p, err := a.inspect()
	if err != nil || p.Source != "local" || !p.Installed || !p.NeedsSetup {
		t.Fatalf("local: %+v, %v", p, err)
	}
	if err := a.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	p, _ = a.inspect()
	if p.NeedsSetup || p.Source != "local" {
		t.Fatalf("completed setup: %+v", p)
	}
	if err := a.Update(context.Background()); err == nil {
		t.Fatal("updated a development checkout")
	}
	if err := a.Uninstall(context.Background()); err == nil {
		t.Fatal("removed an external installation")
	}
	if _, err := os.Stat(a.managedPath()); !os.IsNotExist(err) {
		t.Fatal("created an unnecessary managed copy")
	}
}

func TestDetectionHandlesAutoloadAndFileURLs(t *testing.T) {
	a := fixture(t)
	path := filepath.Join(a.ConfigDir, "plugins", "slorp.ts")
	put(t, path, "const AGENT = 'opencode'; fetch('/api/bridge/poll')")
	p, err := a.inspect()
	if err != nil || p.Source != "file" || !p.Installed {
		t.Fatalf("autoload: %+v, %v", p, err)
	}
	if err := a.Install(context.Background()); err == nil {
		t.Fatal("silently duplicated an old single-file bridge")
	}
	put(t, filepath.Join(a.ConfigDir, "opencode.json"), `{"plugin":["file://`+path+`"]}`)
	if _, err := a.inspect(); err != nil {
		t.Fatal("same file listed twice should be deduplicated:", err)
	}
}

func TestInstallErrorsAreReported(t *testing.T) {
	for _, which := range []string{"missing executable", "malformed config", "failed command", "unowned directory"} {
		t.Run(which, func(t *testing.T) {
			a := fixture(t)
			switch which {
			case "missing executable":
				a.Bin += "-missing"
			case "malformed config":
				put(t, filepath.Join(a.ConfigDir, "opencode.jsonc"), "{broken")
			case "failed command":
				put(t, a.Bin, "#!/bin/sh\necho rejected >&2\nexit 1\n")
				os.Chmod(a.Bin, 0o700)
			case "unowned directory":
				put(t, filepath.Join(a.managedPath(), "package.json"), `{"name":"someone-else"}`)
			}
			if err := a.Install(context.Background()); err == nil {
				t.Fatal("install unexpectedly succeeded")
			}
		})
	}
}

func TestInstallWithPluginAddSubcommand(t *testing.T) {
	a := fixture(t)
	put(t, a.Bin, `#!/bin/sh
if test "$1" != plugin || test "$2" != add || test $# != 3; then
  echo "Unknown subcommand \"$2\" for \"opencode plugin\"" >&2
  exit 1
fi
mkdir -p "$XDG_CONFIG_HOME/opencode"
printf '{"plugin":["%s"]}' "$3" > "$XDG_CONFIG_HOME/opencode/opencode.json"
printf '{"plugin":["%s"]}' "$3" > "$XDG_CONFIG_HOME/opencode/tui.json"
`)
	os.Chmod(a.Bin, 0o700)
	if err := a.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p, err := a.inspect(); err != nil || !p.Installed || p.NeedsSetup {
		t.Fatalf("installed: %+v, %v", p, err)
	}
	put(t, a.Bin, "#!/bin/sh\ntest \"$2\" = add && echo refused >&2 || echo 'Unknown subcommand' >&2\nexit 1\n")
	os.Chmod(a.Bin, 0o700)
	if err := a.install(context.Background(), a.managedPath()); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("failed add: %v", err)
	}
}

func TestRemoveReferencePreservesOtherConfiguration(t *testing.T) {
	for _, input := range []string{
		`{"plugin":["/managed"]}`,
		`{"plugin":["/managed", "other"]}`,
		`{"plugin":["other", "/managed"]}`,
		`{"plugin":["first", ["/managed", {"url":"http://127.0.0.1:4747"}], "last"]}`,
		"{\n // before\n \"plugin\": [\"other\", /* keep */ \"/managed\",],\n \"model\": \"provider/model\", // after\n}",
		`{"plugin":["/managed","/managed","other"]}`,
		`{"plugin":["/managed",]}`,
	} {
		t.Run(input, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "opencode.jsonc")
			put(t, path, input)
			if err := removeReference(path, "/managed"); err != nil {
				t.Fatal(err)
			}
			b, _ := os.ReadFile(path)
			list, err := entries(b)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range list {
				if e.spec == "/managed" {
					t.Fatal("reference left behind")
				}
			}
			for _, text := range []string{"// before", "/* keep */", "// after", `"model": "provider/model"`} {
				if strings.Contains(input, text) && !strings.Contains(string(b), text) {
					t.Errorf("lost %s", text)
				}
			}
			var doc map[string]any
			if err := json.Unmarshal(jsonConfig(b), &doc); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type interruptedUninstall struct {
	context.Context
	serverConfig, target string
}

func (c interruptedUninstall) Err() error {
	b, _ := os.ReadFile(c.serverConfig)
	if !strings.Contains(string(b), c.target) {
		return context.Canceled
	}
	return nil
}

func TestInterruptedUninstallCanBeRetried(t *testing.T) {
	a := fixture(t)
	if err := a.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := interruptedUninstall{context.Background(), filepath.Join(a.ConfigDir, "opencode.json"), a.managedPath()}
	if err := a.Uninstall(ctx); err != context.Canceled {
		t.Fatalf("uninstall = %v", err)
	}
	p, err := a.inspect()
	if err != nil || !p.Installed || p.Enabled || !p.NeedsSetup || p.Source != "managed" {
		t.Fatalf("partial setup hidden: %+v %v", p, err)
	}
	if err := a.Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p, err := a.inspect(); err != nil || p.Installed {
		t.Fatalf("retry left a companion: %+v %v", p, err)
	}
}

func TestPluginManagementDoesNotBlockRetirement(t *testing.T) {
	a := fixture(t)
	a.Interval, a.Silence = 5*time.Millisecond, 10*time.Millisecond
	a.Hub.Apply(Agent, "silent", nil)
	a.pluginMu.Lock()
	defer a.pluginMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan struct{})
	go func() { a.Run(ctx); close(stopped) }()
	deadline := time.After(time.Second)
	for len(a.Hub.Sessions()) != 0 {
		select {
		case <-deadline:
			t.Fatal("plugin management blocked session retirement")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("adapter did not stop")
	}
}

func TestDisabledCompanionIsReported(t *testing.T) {
	a := fixture(t)
	if err := a.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"plugin": []string{a.managedPath()}, "plugin_enabled": map[string]bool{"slorp": false}})
	put(t, filepath.Join(a.ConfigDir, "tui.json"), string(b))
	p, err := a.inspect()
	if err == nil || !strings.Contains(err.Error(), "disabled") || !p.NeedsSetup {
		t.Fatalf("disabled companion = %+v %v", p, err)
	}
}

func TestUninstallPreservesConfigSymlinks(t *testing.T) {
	dir := t.TempDir()
	path, link := filepath.Join(dir, "real.json"), filepath.Join(dir, "opencode.json")
	put(t, path, `{"plugin":["/managed","other"]}`)
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := removeReference(link, "/managed"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("replaced configuration symlink")
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "/managed") || !strings.Contains(string(b), "other") {
		t.Fatalf("target = %s", b)
	}
}
