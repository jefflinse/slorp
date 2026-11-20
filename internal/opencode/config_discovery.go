package opencode

import (
	"os"
	"path/filepath"
	"strings"
)

func configAncestors(dir string) []string {
	var dirs []string
	for current := dir; ; current = filepath.Dir(current) {
		dirs = append(dirs, current)
		if _, err := os.Stat(filepath.Join(current, ".git")); err == nil || filepath.Dir(current) == current {
			return dirs
		}
	}
}

// runtimeDiscovery preserves the inputs that Config.get does not serialize.
func (a *Adapter) runtimeDiscovery(r *runtime, config map[string]any) error {
	dir, err := filepath.Abs(r.dir)
	if err != nil {
		return err
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	home := runtimeEnv(r, "OPENCODE_TEST_HOME")
	if home == "" {
		home = a.Home
	}
	ancestors := configAncestors(dir)
	global := a.configDirs()[0]
	if a.ExtraConfigDir != "" {
		global = a.ExtraConfigDir
	}
	if err := r.runtimeInstructions(config, home, global, ancestors); err != nil {
		return err
	}
	dirs := []string{a.configDirs()[0]}
	if !runtimeFlag(r, "OPENCODE_DISABLE_PROJECT_CONFIG") {
		for _, parent := range ancestors {
			dirs = append(dirs, filepath.Join(parent, ".opencode"))
		}
	}
	dirs = append(dirs, filepath.Join(home, ".opencode"))
	if a.ExtraConfigDir != "" {
		dirs = append(dirs, a.ExtraConfigDir)
	}
	seen := map[string]bool{}
	unique := make([]string, 0, len(dirs))
	for _, source := range dirs {
		if !seen[source] {
			seen[source] = true
			unique = append(unique, source)
		}
	}
	if err := r.runtimeSkills(config, home, dir, ancestors, unique); err != nil {
		return err
	}
	return r.runtimeTools(config, unique)
}

func (r *runtime) runtimeInstructions(config map[string]any, home, global string, ancestors []string) error {
	project := !runtimeFlag(r, "OPENCODE_DISABLE_PROJECT_CONFIG")
	claude := !runtimeEffectFlag(r, "OPENCODE_DISABLE_CLAUDE_CODE") && !runtimeEffectFlag(r, "OPENCODE_DISABLE_CLAUDE_CODE_PROMPT")
	instructions := []any{}
	globalFiles := []string{filepath.Join(global, "AGENTS.md")}
	if claude {
		globalFiles = append(globalFiles, filepath.Join(home, ".claude", "CLAUDE.md"))
	}
	for _, file := range globalFiles {
		if _, err := os.Stat(file); err == nil {
			instructions = append(instructions, file)
			break
		}
	}
	if project {
		names := []string{"AGENTS.md"}
		if claude {
			names = append(names, "CLAUDE.md")
		}
		names = append(names, "CONTEXT.md")
		for _, name := range names {
			found := false
			for _, parent := range ancestors {
				file := filepath.Join(parent, name)
				if _, err := os.Stat(file); err == nil {
					instructions = append(instructions, file)
					found = true
				}
			}
			if found {
				break
			}
		}
	}
	if list, ok := config["instructions"].([]any); ok {
		for _, item := range list {
			pattern, ok := item.(string)
			if !ok {
				continue
			}
			if strings.HasPrefix(pattern, "http://") || strings.HasPrefix(pattern, "https://") || filepath.IsAbs(pattern) {
				instructions = append(instructions, pattern)
			} else if strings.HasPrefix(pattern, "~/") {
				instructions = append(instructions, filepath.Join(home, pattern[2:]))
			} else {
				roots := ancestors
				if !project {
					roots = []string{global}
				}
				for _, root := range roots {
					// Native's absolute-pattern branch only globs the basename.
					relative, err := filepath.Rel(r.private, filepath.Join(root, pattern))
					if err != nil {
						return err
					}
					instructions = append(instructions, relative)
				}
			}
		}
	}
	config["instructions"] = instructions
	return nil
}

func (r *runtime) runtimeSkills(config map[string]any, home, dir string, ancestors, dirs []string) error {
	var roots []string
	if !runtimeEffectFlag(r, "OPENCODE_DISABLE_EXTERNAL_SKILLS") {
		external := []string{".agents"}
		if !runtimeEffectFlag(r, "OPENCODE_DISABLE_CLAUDE_CODE") && !runtimeEffectFlag(r, "OPENCODE_DISABLE_CLAUDE_CODE_SKILLS") {
			external = append([]string{".claude"}, external...)
		}
		for _, base := range append([]string{home}, ancestors...) {
			for _, name := range external {
				found, err := externalSkillDirs(filepath.Join(base, name, "skills"))
				if err != nil {
					return err
				}
				roots = append(roots, found...)
			}
		}
	}
	for _, source := range dirs {
		for _, name := range []string{"skill", "skills"} {
			roots = append(roots, filepath.Join(source, name))
		}
	}
	skills, _ := config["skills"].(map[string]any)
	if skills == nil {
		skills = map[string]any{}
	}
	if list, ok := skills["paths"].([]any); ok {
		for _, item := range list {
			if p, ok := item.(string); ok {
				if strings.HasPrefix(p, "~/") {
					p = filepath.Join(home, p[2:])
				} else if !filepath.IsAbs(p) {
					p = filepath.Join(dir, p)
				}
				roots = append(roots, p)
			}
		}
	}
	paths := []any{}
	for _, root := range roots {
		if info, err := os.Stat(root); err == nil && info.IsDir() {
			paths = append(paths, root)
		}
	}
	skills["paths"] = paths
	config["skills"] = skills
	return nil
}
