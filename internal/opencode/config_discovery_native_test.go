package opencode

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/hub"
)

func TestConfigNativePureEmptyConfigRejectsStart(t *testing.T) {
	a := configNativeFixture(t)
	for _, value := range []string{"true", "1", "TRUE"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("OPENCODE_PURE", value)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			id, err := a.Start(ctx, hub.Launch{Dir: a.Home})
			if err == nil || id != "" || !strings.Contains(err.Error(), "OPENCODE_PURE") || !strings.Contains(err.Error(), "Slorp bridge") {
				t.Fatalf("managed start accepted pure mode: id %q, err %v", id, err)
			}
			if sessions := a.Hub.Sessions(); len(sessions) != 0 {
				t.Fatalf("pure rejection created managed sessions: %+v", sessions)
			}
		})
	}
}

func configSourceHashes(t *testing.T, paths ...string) map[string]string {
	t.Helper()
	result := map[string]string{}
	for _, root := range paths {
		if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			value := fmt.Sprintf("%s:%d:%d", info.Mode(), info.Size(), info.ModTime().UnixNano())
			if entry.Type()&os.ModeSymlink != 0 {
				target, err := os.Readlink(path)
				if err != nil {
					return err
				}
				value += ":" + target
			} else if info.Mode().IsRegular() {
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				value += fmt.Sprintf(":%x", sha256.Sum256(data))
			}
			result[path] = value
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	return result
}

func TestConfigNativeDirectAncestorBareImportRejectsSafely(t *testing.T) {
	a := configNativeFixture(t)
	model, observations := configToolModel(t, []string{"check"})
	configToolProvider(t, a, model.URL, nil)
	project := filepath.Join(a.Home, "javascript-project")
	manifest := filepath.Join(project, "package.json")
	lock := filepath.Join(project, "package-lock.json")
	modules := filepath.Join(project, "node_modules")
	put(t, manifest, `{"name":"javascript-project","type":"module","dependencies":{"project-dep":"1.0.0"}}`)
	put(t, lock, `{"name":"javascript-project","lockfileVersion":3,"packages":{"":{"dependencies":{"project-dep":"1.0.0"}},"node_modules/project-dep":{"version":"1.0.0"}}}`)
	put(t, filepath.Join(modules, "project-dep", "package.json"), `{"name":"project-dep","version":"1.0.0","type":"module","main":"index.js"}`)
	put(t, filepath.Join(modules, "project-dep", "index.js"), `export const value = 'direct-ancestor-bare-import'`)
	put(t, filepath.Join(modules, ".package-lock.json"), `{"lockfileVersion":3,"packages":{"node_modules/project-dep":{"version":"1.0.0"}}}`)
	configToolPlugin(t, filepath.Join(project, ".opencode", "plugins", "check"), "fixture.check",
		`const {value}=await import("project-dep"); await ctx.tool.transform(editor=>editor.add({name:"check",description:"direct bare import",options:{codemode:false},input:{type:"object",properties:{}},execute:async()=>({content:value})}))`)
	if out, err := exec.Command("git", "init", "--quiet", project).CombinedOutput(); err != nil {
		t.Fatalf("isolating project: %s %v", out, err)
	}
	before := configSourceHashes(t, manifest, lock, modules)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	baseline := configBaseline(t, a, project)
	observed := configExecuteTools(t, ctx, baseline, observations)
	if observed.Outputs["check"] != "direct-ancestor-bare-import" {
		t.Fatalf("native direct bare import failed: %+v", observed)
	}
	if after := configSourceHashes(t, manifest, lock, modules); !reflect.DeepEqual(before, after) {
		t.Fatal("baseline changed ancestor dependency files")
	}
	source := filepath.Join(project, ".opencode")
	beforeManaged := configSourceHashes(t, manifest, lock, modules, source)
	_, err := a.configured(ctx, project)
	if err == nil || !strings.Contains(err.Error(), "ancestor dependency scope") {
		t.Fatalf("ancestor bare import was silently lost: %v", err)
	}
	if after := configSourceHashes(t, manifest, lock, modules, source); !reflect.DeepEqual(beforeManaged, after) {
		t.Fatal("managed rejection changed source file paths, modes, sizes, mtimes, symlink targets, or content hashes")
	}
}

type configSkillInfo struct{ ID, Name, Description, Path, Content string }

func configNativeSkills(t *testing.T, ctx context.Context, r *runtime) map[string]configSkillInfo {
	t.Helper()
	var list []configSkillInfo
	if err := r.request(ctx, http.MethodGet, "/api/model", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.request(ctx, http.MethodGet, "/api/skill", nil, &list); err != nil {
		t.Fatal(err)
	}
	result := map[string]configSkillInfo{}
	for _, skill := range list {
		if strings.HasPrefix(skill.Path, "skill://") {
			skill.Content = "" // Built-in diagnostics include the active plugin list.
		}
		result[skill.Name] = skill
	}
	return result
}

func TestConfigNativeHiddenExternalSkillsAndFlags(t *testing.T) {
	a := configNativeFixture(t)
	project := filepath.Join(a.Home, "project")
	put(t, filepath.Join(project, "AGENTS.md"), "Isolated skill fixture.")
	if out, err := exec.Command("git", "init", "--quiet", project).CombinedOutput(); err != nil {
		t.Fatalf("isolating project: %s %v", out, err)
	}
	homeAgents := filepath.Join(a.Home, ".agents", "skills", ".internal", "home-agents")
	homeClaude := filepath.Join(a.Home, ".claude", "skills", ".internal", "home-claude")
	for name, dir := range map[string]string{
		"home-agents-hidden":     homeAgents,
		"home-claude-hidden":     homeClaude,
		"project-agents-hidden":  filepath.Join(project, ".agents", "skills", ".internal", "project-agents"),
		"project-claude-hidden":  filepath.Join(project, ".claude", "skills", ".internal", "project-claude"),
		"config-visible":         filepath.Join(a.ConfigDir, "skills", "visible"),
		"config-hidden-excluded": filepath.Join(a.ConfigDir, "skills", ".hidden", "review"),
	} {
		put(t, filepath.Join(dir, "SKILL.md"), fmt.Sprintf("---\nname: %s\ndescription: Skill %s\n---\nLiteral {file:missing} %s", name, name, name))
		if strings.HasPrefix(name, "home-") || strings.HasPrefix(name, "project-") {
			visible := strings.TrimSuffix(name, "-hidden") + "-visible"
			put(t, filepath.Join(filepath.Dir(filepath.Dir(dir)), visible, "SKILL.md"), fmt.Sprintf("---\nname: %s\ndescription: Skill %s\n---\nVisible body", visible, visible))
		}
	}
	for _, tc := range []struct {
		name           string
		flags          map[string]string
		explicit       string
		agents, claude bool
	}{
		{name: "enabled", agents: true, claude: true},
		{name: "claude skills disabled", flags: map[string]string{"OPENCODE_DISABLE_CLAUDE_CODE_SKILLS": "true"}, agents: true},
		{name: "claude skills nonboolean ignored", flags: map[string]string{"OPENCODE_DISABLE_CLAUDE_CODE_SKILLS": "yes"}, agents: true, claude: true},
		{name: "broad claude flag wins", flags: map[string]string{"OPENCODE_DISABLE_CLAUDE_CODE": "true", "OPENCODE_DISABLE_CLAUDE_CODE_SKILLS": "false"}, agents: true},
		{name: "project flag preserves global skills", flags: map[string]string{"OPENCODE_DISABLE_PROJECT_CONFIG": "true"}, agents: true, claude: true},
		{name: "explicit hidden path", explicit: homeAgents, agents: true, claude: true},
		{name: "explicit path survives claude disable", flags: map[string]string{"OPENCODE_DISABLE_CLAUDE_CODE": "true"}, explicit: homeClaude, agents: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for key, value := range tc.flags {
				t.Setenv(key, value)
			}
			config := map[string]any{"$schema": "https://opencode.ai/config.json"}
			if tc.explicit != "" {
				config["skills"] = []string{tc.explicit}
			}
			data, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			put(t, filepath.Join(a.ConfigDir, "opencode.json"), string(data))
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			baseline := configBaseline(t, a, project)
			before := configNativeSkills(t, ctx, baseline)
			managed, err := a.configured(ctx, project)
			if err != nil {
				t.Fatal(err)
			}
			defer a.finish(managed)
			after := configNativeSkills(t, ctx, managed)
			if !reflect.DeepEqual(before, after) {
				for name, skill := range before {
					if after[name] != skill {
						t.Errorf("skill %s differs: native %+v, managed %+v", name, skill, after[name])
					}
				}
				for name := range after {
					if _, ok := before[name]; !ok {
						t.Errorf("unexpected managed skill %s", name)
					}
				}
			}
			for name, want := range map[string]bool{
				"home-agents-hidden":     tc.explicit == homeAgents,
				"home-claude-hidden":     tc.explicit == homeClaude,
				"project-agents-hidden":  false,
				"project-claude-hidden":  false,
				"home-agents-visible":    tc.agents,
				"home-claude-visible":    tc.claude,
				"project-agents-visible": tc.agents && tc.flags["OPENCODE_DISABLE_PROJECT_CONFIG"] != "true",
				"project-claude-visible": tc.claude && tc.flags["OPENCODE_DISABLE_PROJECT_CONFIG"] != "true",
				"config-visible":         true,
				"config-hidden-excluded": false,
			} {
				if _, exists := after[name]; exists != want {
					t.Errorf("skill %s present=%v, want %v", name, exists, want)
				}
			}
		})
	}
}
