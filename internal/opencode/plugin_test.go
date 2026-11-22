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
	unmanaged(t)
	root := t.TempDir()
	st := testdb.Open(t)
	a := &Adapter{Home: root, Data: filepath.Join(root, "slorp"), ConfigDir: filepath.Join(root, "config", "opencode"), URL: "http://127.0.0.1:5858", Bin: filepath.Join(root, "opencode"), Hub: hub.New(testdb.KB(t, st), st)}
	put(t, a.Bin, `#!/bin/sh
test "$1" = --version && test $# = 1 || exit 4
echo 'opencode v2.3.2'
`)
	if err := os.Chmod(a.Bin, 0o700); err != nil {
		t.Fatal(err)
	}
	return a
}

// unsetEnv removes each variable for the rest of the test.
func unsetEnv(t *testing.T, keys ...string) {
	t.Helper()
	for _, key := range keys {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
}

// unmanaged removes the OpenCode and Slorp overrides a test inherits when it
// runs inside a Slorp-managed session, leaving what an ordinary shell has.
// SLORP_NATIVE_TEST stays, since it chooses which tests run.
func unmanaged(t *testing.T) {
	t.Helper()
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if (strings.HasPrefix(key, "OPENCODE_") || strings.HasPrefix(key, "SLORP_")) && key != "SLORP_NATIVE_TEST" {
			unsetEnv(t, key)
		}
	}
}

// clearEnv removes every variable but PATH and TMPDIR for the rest of the
// test.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, entry := range os.Environ() {
		if key, _, _ := strings.Cut(entry, "="); key != "PATH" && key != "TMPDIR" {
			unsetEnv(t, key)
		}
	}
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
	if version != "0.7.0" || len(files["presence.ts"]) == 0 {
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

func TestBundledQuotasRuntimePackaging(t *testing.T) {
	a := fixture(t)
	if err := a.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	files, version, err := bundled(a.URL)
	if err != nil {
		t.Fatal(err)
	}
	if version != "0.7.0" || len(files["quotas.ts"]) == 0 || !strings.Contains(string(files["slorp.ts"]), "'./quotas.ts'") {
		t.Fatalf("missing quotas runtime/import or version: %q", version)
	}
	written, err := os.ReadFile(filepath.Join(a.managedPath(), "quotas.ts"))
	if err != nil || string(written) != string(files["quotas.ts"]) {
		t.Fatalf("installed quotas differ from embedded runtime: %v", err)
	}
	before := fingerprint(files)
	if !strings.Contains(string(files["revision.ts"]), before) {
		t.Fatal("wrapper revision does not identify the full runtime")
	}
	files["quotas.ts"] = []byte("changed quota collector")
	if fingerprint(files) == before {
		t.Fatal("quota changes did not change the runtime revision")
	}
	put(t, filepath.Join(a.managedPath(), "quotas.ts"), "old collector")
	p, err := a.inspect()
	if err != nil || !p.UpdateAvailable || p.Revision == before {
		t.Fatalf("quota change was not detected: %+v, %v", p, err)
	}
	if err := a.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	p, err = a.inspect()
	if err != nil || p.UpdateAvailable || p.Revision != before {
		t.Fatalf("quota runtime was not restored: %+v, %v", p, err)
	}
	for _, name := range []string{"quotas.test.ts", "slorp.test.ts"} {
		if _, err := os.Stat(filepath.Join(a.managedPath(), name)); !os.IsNotExist(err) {
			t.Fatalf("development file %s installed: %v", name, err)
		}
	}
}

func TestDetectLocalPackageLoadsBothParts(t *testing.T) {
	a := fixture(t)
	path := filepath.Join(a.Home, "checkout", "integrations", "opencode")
	put(t, filepath.Join(path, "package.json"), `{"name":"opencode-slorp","version":"dev","exports":{"./tui":"./tui.ts"}}`)
	put(t, filepath.Join(path, "tui.ts"), "export default {}")
	put(t, filepath.Join(a.ConfigDir, "opencode.jsonc"), `{
  // Keep this comment and the custom options.
  "plugins": [{"package":"../../checkout/integrations/opencode", "options":{"url":"http://127.0.0.1:5858"}}],
}`)
	p, err := a.inspect()
	if err != nil || p.Source != "local" || !p.Installed || p.NeedsSetup {
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

func TestInstallRegistersLocalPackageWithoutPluginAdd(t *testing.T) {
	a := fixture(t)
	put(t, a.Bin, `#!/bin/sh
if test "$1" = --version; then echo 'opencode v2.3.2'; exit 0; fi
echo "Plugin target must be an npm registry package or Git package specifier" >&2
exit 1
`)
	os.Chmod(a.Bin, 0o700)
	if err := a.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p, err := a.inspect(); err != nil || !p.Installed || p.NeedsSetup {
		t.Fatalf("installed: %+v, %v", p, err)
	}
	b, err := os.ReadFile(filepath.Join(a.ConfigDir, "opencode.json"))
	if err != nil || !strings.Contains(string(b), `"plugins"`) {
		t.Fatalf("native registration = %s, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(a.ConfigDir, "cli.json")); !os.IsNotExist(err) {
		t.Fatalf("created unnecessary terminal configuration: %v", err)
	}
}

func TestRemoveReferencePreservesOtherConfiguration(t *testing.T) {
	for _, input := range []string{
		`{"plugins":["/managed"]}`,
		`{"plugins":["/managed", "other"]}`,
		`{"plugins":["other", "/managed"]}`,
		`{"plugins":["first", {"package":"/managed", "options":{"url":"http://127.0.0.1:4747"}}, "last"]}`,
		"{\n // before\n \"plugins\": [\"other\", /* keep */ \"/managed\",],\n \"model\": \"provider/model\", // after\n}",
		`{"plugins":["/managed","/managed","other"]}`,
		`{"plugins":["/managed",]}`,
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
	put(t, filepath.Join(a.ConfigDir, "cli.json"), `{"plugins":["`+a.managedPath()+`"]}`)
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

func TestDisabledPluginIsReported(t *testing.T) {
	a := fixture(t)
	if err := a.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"plugins": []string{a.managedPath(), "-slorp"}})
	put(t, filepath.Join(a.ConfigDir, "opencode.json"), string(b))
	p, err := a.inspect()
	if err != nil || p.Enabled || p.NeedsSetup {
		t.Fatalf("disabled companion = %+v %v", p, err)
	}
	if err := a.Uninstall(context.Background()); err != nil {
		t.Fatal("could not uninstall disabled plugin:", err)
	}
}

func TestDisabledTerminalCompanionIsReported(t *testing.T) {
	a := fixture(t)
	if err := a.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(a.ConfigDir, "cli.json"), `{"plugins":["*","-slorp"]}`)
	p, err := a.inspect()
	if err != nil || !p.Installed || p.Enabled || p.NeedsSetup {
		t.Fatalf("disabled terminal: %+v, %v", p, err)
	}
	if err := a.Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(a.ConfigDir, "cli.json"))
	if !strings.Contains(string(b), `"-slorp"`) {
		t.Fatal("removed the user's terminal control")
	}
}

func TestUninstallPreservesConfigSymlinks(t *testing.T) {
	dir := t.TempDir()
	path, link := filepath.Join(dir, "real.json"), filepath.Join(dir, "opencode.json")
	put(t, path, `{"plugins":["/managed","other"]}`)
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

func TestAddReferencePreservesConfiguration(t *testing.T) {
	for _, input := range []string{
		`{}`,
		`{"model":"provider/model"}`,
		`{"model":"provider/model",}`,
		"{\"model\":\"provider/model\", // keep\n}",
		`{"plugins":[]}`,
		`{"plugins":["other"]}`,
		`{"plugins":["other",]}`,
		"{\n // before\n \"plugins\": [\"other\", /* keep */],\n \"model\": \"provider/model\", // after\n}",
		`{"plugins":[{"package":"other","options":{"nested":[1,2]}}]}`,
		`{"plugins":["other",{"package":"local-package","options":{"url":"http://localhost","nested":[1,2]}}]}`,
		`{"plugins":[{"package":"local-package", /* keep */ "options":{"url":"http://localhost","nested":[1,2]}}]}`,
		`{"plu\u0067ins":["other"]}`,
	} {
		t.Run(input, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "opencode.jsonc")
			target := filepath.Join(t.TempDir(), "space and \"quotes", "plugin")
			put(t, path, input)
			if err := addReference(path, target); err != nil {
				t.Fatal(err)
			}
			b, _ := os.ReadFile(path)
			var doc map[string]json.RawMessage
			if err := json.Unmarshal(jsonConfig(b), &doc); err != nil {
				t.Fatal(err)
			}
			var plugins []any
			if err := json.Unmarshal(doc["plugins"], &plugins); err != nil {
				t.Fatal(err)
			}
			if plugins[len(plugins)-1] != target {
				t.Fatalf("missing registered path: %s", b)
			}
			for _, text := range []string{"// before", "/* keep */", "// after", `"model": "provider/model"`} {
				if strings.Contains(input, text) && !strings.Contains(string(b), text) {
					t.Errorf("lost %s", text)
				}
			}
			for _, text := range []string{`"options":{"nested":[1,2]}`, `"options":{"url":"http://localhost","nested":[1,2]}`} {
				if strings.Contains(input, text) && !strings.Contains(string(b), text) {
					t.Errorf("lost native options %s: %s", text, b)
				}
			}
			if err := addReference(path, target); err != nil {
				t.Fatal(err)
			}
			again, _ := os.ReadFile(path)
			if string(again) != string(b) {
				t.Fatalf("non-idempotent registration:\n%s\n%s", b, again)
			}
			if err := removeReference(path, target); err != nil {
				t.Fatal(err)
			}
			b, _ = os.ReadFile(path)
			if !json.Valid(jsonConfig(b)) {
				t.Fatalf("invalid after removal: %s", b)
			}
		})
	}
}

func TestAddReferencePreservesSymlinkAndMode(t *testing.T) {
	dir := t.TempDir()
	target, link := filepath.Join(dir, "real.jsonc"), filepath.Join(dir, "opencode.jsonc")
	put(t, target, `{"plugins":["other"]}`)
	if err := os.Chmod(target, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := addReference(link, "/managed"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(link)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("replaced symlink: %v", err)
	}
	info, err = os.Stat(target)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("changed permissions: %v", err)
	}
}

func TestInstallRejectsUnsupportedVersionBeforeWriting(t *testing.T) {
	a := fixture(t)
	put(t, a.Bin, "#!/bin/sh\necho 1.19.6\n")
	if err := a.Install(context.Background()); err == nil || !strings.Contains(err.Error(), "requires OpenCode 2.x") {
		t.Fatalf("version error = %v", err)
	}
	if _, err := os.Stat(a.managedPath()); !os.IsNotExist(err) {
		t.Fatal("wrote plugin before rejecting incompatible host")
	}
}

func TestInstallRepairsInterruptedUpgrade(t *testing.T) {
	a := fixture(t)
	if err := a.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(a.managedPath(), "server.ts"), "incomplete")
	p, err := a.inspect()
	if err != nil || !p.UpdateAvailable {
		t.Fatalf("incomplete = %+v, %v", p, err)
	}
	if err := a.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	p, err = a.inspect()
	if err != nil || p.UpdateAvailable {
		t.Fatalf("retry = %+v, %v", p, err)
	}
}

func TestInstallUsesReplacementServerConfig(t *testing.T) {
	a := fixture(t)
	a.ExtraConfigDir = filepath.Join(a.Home, "replacement")
	inactive := filepath.Join(a.ConfigDir, "opencode.json")
	put(t, inactive, "{inactive configuration}")
	put(t, filepath.Join(a.ConfigDir, "plugins", "other-slorp", "package.json"), `{"name":"opencode-slorp"}`)
	if err := a.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	p, err := a.inspect()
	if err != nil || !p.Installed || !p.Enabled || p.NeedsSetup {
		t.Fatalf("replacement registration = %+v, %v", p, err)
	}
	b, err := os.ReadFile(filepath.Join(a.ExtraConfigDir, "opencode.json"))
	if err != nil || !strings.Contains(string(b), a.managedPath()) {
		t.Fatalf("active registration = %s, %v", b, err)
	}
	b, _ = os.ReadFile(inactive)
	if string(b) != "{inactive configuration}" {
		t.Fatalf("inactive configuration changed: %s", b)
	}
	put(t, filepath.Join(a.ConfigDir, "cli.json"), `{"plugins":["-slorp"]}`)
	p, err = a.inspect()
	if err != nil || p.Enabled {
		t.Fatalf("default terminal controls ignored: %+v, %v", p, err)
	}
}

func TestUpdatePreservesRegistrationAcrossConfigFiles(t *testing.T) {
	a := fixture(t)
	if err := a.writeBundle(); err != nil {
		t.Fatal(err)
	}
	path, _ := json.Marshal(a.managedPath())
	original := `{"plugins":[{"package":` + string(path) + `,"options":{"url":"http://localhost:5858","nested":{"keep":true}}}]}`
	jsonPath := filepath.Join(a.ConfigDir, "opencode.json")
	jsoncPath := jsonPath + "c"
	put(t, jsonPath, original)
	put(t, jsoncPath, "{ // unrelated\n \"model\":\"provider/model\"\n}")
	companion := filepath.Join(a.ConfigDir, "cli.json")
	put(t, companion, `{"plugins":[`+string(path)+`,"other"]}`)
	if err := a.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(jsonPath)
	if string(b) != original {
		t.Fatalf("changed custom options: %s", b)
	}
	b, _ = os.ReadFile(jsoncPath)
	list, err := entries(b)
	if err != nil || len(list) != 0 {
		t.Fatalf("added overriding registration: %s, %v", b, err)
	}
	b, _ = os.ReadFile(companion)
	if strings.Contains(string(b), a.managedPath()) || !strings.Contains(string(b), "other") {
		t.Fatalf("duplicate companion cleanup = %s", b)
	}
}

type interruptedInstall struct {
	context.Context
	config string
}

func (c interruptedInstall) Err() error {
	if _, err := os.Stat(c.config); err == nil {
		return context.Canceled
	}
	return nil
}

func TestInterruptedInstallRetainsCleanupState(t *testing.T) {
	a := fixture(t)
	if err := a.writeBundle(); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"plugins": []string{a.managedPath()}})
	put(t, filepath.Join(a.ConfigDir, "cli.json"), string(b))
	ctx := interruptedInstall{context.Background(), filepath.Join(a.ConfigDir, "opencode.json")}
	if err := a.Install(ctx); err != context.Canceled {
		t.Fatalf("interrupted install = %v", err)
	}
	p, err := a.inspect()
	if err != nil || !p.Installed || !p.Enabled || !p.NeedsSetup {
		t.Fatalf("partial setup hidden: %+v, %v", p, err)
	}
	if err := a.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	p, err = a.inspect()
	if err != nil || p.NeedsSetup {
		t.Fatalf("retry incomplete: %+v, %v", p, err)
	}
	b, _ = os.ReadFile(filepath.Join(a.ConfigDir, "cli.json"))
	list, err := entries(b)
	if err != nil || len(list) != 0 {
		t.Fatalf("duplicate companion left behind: %s, %v", b, err)
	}
}
