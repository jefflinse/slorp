package opencode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigToolsRejectUnsupportedPackages(t *testing.T) {
	for _, tc := range []struct{ name, manifest, want string }{
		{"workspace packages", `{"workspaces":["packages/*"]}`, "unsupported custom tool workspace"},
		{"workspace protocol", `{"dependencies":{"shared":"workspace:*"}}`, "workspace/link dependency"},
		{"link protocol", `{"dependencies":{"shared":"link:../shared"}}`, "workspace/link dependency"},
		{"external file dependency", `{"dependencies":{"shared":"file:../shared"}}`, "outside config root"},
		{"external plain path", `{"dependencies":{"shared":"../shared"}}`, "outside config root"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := fixture(t)
			put(t, filepath.Join(a.ConfigDir, "tools", "test.ts"), `export default {args:{}, description:'test', execute:async()=>''}`)
			put(t, filepath.Join(a.ConfigDir, "package.json"), tc.manifest)
			outside := filepath.Join(filepath.Dir(a.ConfigDir), "shared", "package.json")
			put(t, outside, `{"name":"shared","version":"1.0.0"}`)
			r := &runtime{dir: a.Home, private: t.TempDir(), cmd: a.command(context.Background())}
			if err := r.runtimeTools(map[string]any{}, []string{a.ConfigDir}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q error, got %v", tc.want, err)
			}
			for path, want := range map[string]string{filepath.Join(a.ConfigDir, "package.json"): tc.manifest, outside: `{"name":"shared","version":"1.0.0"}`} {
				if data, err := os.ReadFile(path); err != nil || string(data) != want {
					t.Fatalf("source changed: %s %s %v", path, data, err)
				}
			}
		})
	}
}

func TestConfigToolsMirrorAncestorsWithoutCopying(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "project", "nested", "deep", ".opencode")
	put(t, filepath.Join(source, "tools", "test.ts"), "export {}")
	shared := filepath.Join(root, "project", "shared.ts")
	put(t, shared, "export const shared = true")
	put(t, filepath.Join(root, "project", "node_modules", "must-not-touch", "index.js"), "source")
	put(t, filepath.Join(source, "resources", "binary"), strings.Repeat("x", 17<<20))
	tree := filepath.Join(root, "private", "root")
	target := filepath.Join(tree, strings.TrimPrefix(source, string(filepath.Separator)))
	scope := toolScope{source: source, target: target}
	if err := scope.copyModules(source, target, 0); err != nil {
		t.Fatal(err)
	}
	if err := mirrorToolAncestors(source, target); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"shared.ts", "node_modules"} {
		path := filepath.Join(target, "..", "..", "..", name)
		info, err := os.Lstat(path)
		if name == "node_modules" {
			if !os.IsNotExist(err) {
				t.Fatalf("ancestor install target was exposed: %s %v", path, err)
			}
			continue
		}
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("ancestor must be linked, not copied: %s %v", path, err)
		}
		if real, err := filepath.EvalSymlinks(path); err != nil || real != shared {
			t.Fatalf("ancestor link: %s %v", real, err)
		}
	}
	if info, err := os.Lstat(filepath.Join(target, "resources")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("resources were copied: %v", err)
	}
}

func TestConfigToolsRejectSourceLinkedDependency(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, ".opencode")
	put(t, filepath.Join(source, "package.json"), `{"dependencies":{"dep":"file:./resources/dep"}}`)
	put(t, filepath.Join(source, "resources", "dep", "package.json"), `{"name":"dep","version":"1.0.0"}`)
	target := filepath.Join(root, "private", "root", strings.TrimPrefix(source, string(filepath.Separator)))
	if err := privateToolScope(source, target, 0); err == nil || !strings.Contains(err.Error(), "source link") {
		t.Fatalf("installer could reach source: %v", err)
	}
}

func TestConfigToolsPureModeFailsExplicitly(t *testing.T) {
	a := fixture(t)
	put(t, filepath.Join(a.ConfigDir, "tools", "test.ts"), "export {}")
	r := &runtime{dir: a.Home, private: t.TempDir(), cmd: a.command(context.Background())}
	replaceEnv(r.cmd, "OPENCODE_PURE", "true")
	if err := a.runtimeConfig(r, map[string]any{}); err == nil || !strings.Contains(err.Error(), "OPENCODE_PURE") {
		t.Fatalf("pure mode silently lost custom tools: %v", err)
	}
}

func TestConfigToolsRejectNestedWorkspaceManifestLink(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, ".opencode")
	put(t, filepath.Join(source, "package.json"), `{"dependencies":{"dep":"file:./dep"}}`)
	outside := filepath.Join(root, "outside.json")
	put(t, outside, `{"name":"dep","version":"1.0.0","workspaces":["packages/*"]}`)
	if err := os.MkdirAll(filepath.Join(source, "dep"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(source, "dep", "package.json")); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "private", "root", strings.TrimPrefix(source, string(filepath.Separator)))
	if err := privateToolScope(source, target, 0); err == nil || !strings.Contains(err.Error(), "unsupported custom tool workspace") {
		t.Fatalf("linked workspace manifest escaped validation: %v", err)
	}
}

func TestConfigToolsRejectSourceAsInstallTarget(t *testing.T) {
	source, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manifest := `{"name":"untouched"}`
	put(t, filepath.Join(source, "package.json"), manifest)
	if err := privateToolScope(source, source, 0); err == nil {
		t.Fatal("source was accepted as the install target")
	}
	if data, err := os.ReadFile(filepath.Join(source, "package.json")); err != nil || string(data) != manifest {
		t.Fatalf("source package changed: %s %v", data, err)
	}
}
