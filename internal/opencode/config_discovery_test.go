package opencode

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigPureRejectsEmptyConfig(t *testing.T) {
	for _, value := range []string{"true", "1", "TRUE"} {
		t.Run(value, func(t *testing.T) {
			a := fixture(t)
			r := &runtime{dir: a.Home, private: t.TempDir(), cmd: a.command(context.Background())}
			replaceEnv(r.cmd, "OPENCODE_PURE", value)
			if err := a.runtimeConfig(r, map[string]any{}); err == nil || !strings.Contains(err.Error(), "Slorp bridge") || !strings.Contains(err.Error(), "OPENCODE_PURE") {
				t.Fatalf("pure mode could disable the bridge without rejecting launch: %v", err)
			}
			entries, err := os.ReadDir(r.private)
			if err != nil || len(entries) != 0 {
				t.Fatalf("pure rejection wrote runtime files: %v %v", entries, err)
			}
		})
	}
}

func TestConfigToolsRejectAncestorDependencyScopes(t *testing.T) {
	for _, field := range []string{"dependencies", "devDependencies", "optionalDependencies", "peerDependencies", "installed-only"} {
		t.Run(field, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(root, "project", ".opencode")
			put(t, filepath.Join(source, "tools", "check.ts"), `import 'project-dep'; export {}`)
			if field == "installed-only" {
				put(t, filepath.Join(root, "project", "node_modules", "project-dep", "index.js"), "export {}")
			} else {
				put(t, filepath.Join(root, "project", "package.json"), fmt.Sprintf(`{%q:{"project-dep":"1.0.0"}}`, field))
			}
			target := filepath.Join(root, "private", "root", strings.TrimPrefix(source, string(filepath.Separator)))
			if err := privateToolScope(source, target); err == nil || !strings.Contains(err.Error(), "ancestor dependency scope") {
				t.Fatalf("ancestor package resolution was silently changed: %v", err)
			}
			if _, err := os.Stat(target); !os.IsNotExist(err) {
				t.Fatalf("unsupported scope was partially installed: %v", err)
			}
		})
	}
}

func TestConfigSkillsKeepNativeDiscoveryRoots(t *testing.T) {
	root := t.TempDir()
	hidden := filepath.Join(root, "skills", ".internal", "review")
	visible := filepath.Join(root, "skills", "visible")
	outside := filepath.Join(root, "shared")
	put(t, filepath.Join(hidden, "SKILL.md"), "hidden")
	put(t, filepath.Join(visible, "SKILL.md"), "visible")
	put(t, filepath.Join(outside, "SKILL.md"), "linked")
	linked := filepath.Join(root, "skills", ".linked")
	if err := os.Symlink(outside, linked); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "skills"), filepath.Join(hidden, "cycle")); err != nil {
		t.Fatal(err)
	}
	r := &runtime{}
	config := map[string]any{"skills": []any{filepath.Join(root, "skills")}}
	if err := r.runtimeSkills(config, root, root, nil, nil); err != nil {
		t.Fatal(err)
	}
	paths, _ := config["skills"].([]any)
	if len(paths) != 1 || paths[0] != filepath.Join(root, "skills") {
		t.Fatalf("skill roots were expanded instead of deferring to V2 discovery: %v", paths)
	}
}
