package opencode

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
)

func configAncestors(dir string) []string {
	var dirs []string
	for current := dir; ; current = filepath.Dir(current) {
		dirs = append(dirs, current)
		if filepath.Dir(current) == current {
			return dirs
		}
	}
}

// runtimeDiscovery preserves filesystem inputs from V2's ordered config sources.
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
	if real, err := filepath.EvalSymlinks(home); err == nil {
		home = real
	}
	ancestors := configAncestors(dir)
	instructionRoots := ancestors
	for i, root := range ancestors {
		if samePath(root, home) {
			instructionRoots = ancestors[:i+1]
			break
		}
	}
	if !withinToolScope(home, dir) {
		for i, root := range ancestors {
			if _, err := os.Stat(filepath.Join(root, ".git")); err == nil {
				instructionRoots = ancestors[:i+1]
				break
			}
		}
	}
	global := a.serverConfigDir()
	if err := r.runtimeInstructions(config, home, global, instructionRoots); err != nil {
		return err
	}
	dirs := []string{a.serverConfigDir()}
	if !runtimeFlag(r, "OPENCODE_DISABLE_PROJECT_CONFIG") {
		for _, parent := range ancestors {
			dirs = append(dirs, filepath.Join(parent, ".opencode"))
		}
	}
	if sources, ok := config[configDirectoriesKey].([]any); ok {
		dirs = nil
		for _, source := range sources {
			if path, ok := source.(string); ok {
				dirs = append(dirs, path)
			}
		}
	}
	delete(config, configDirectoriesKey)
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
	if err := r.preserveDefinitions(config, unique); err != nil {
		return err
	}
	return nil
}

func (r *runtime) runtimeInstructions(config map[string]any, home, global string, ancestors []string) error {
	project := !runtimeFlag(r, "OPENCODE_DISABLE_PROJECT_CONFIG")
	claude := !runtimeFlag(r, "OPENCODE_DISABLE_CLAUDE_CODE") && !runtimeFlag(r, "OPENCODE_DISABLE_CLAUDE_CODE_PROMPT")
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
		for _, parent := range ancestors {
			for _, name := range names {
				file := filepath.Join(parent, name)
				if _, err := os.Stat(file); err == nil {
					instructions = append(instructions, file)
					break
				}
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
				for i, root := range ancestors {
					if _, err := os.Stat(filepath.Join(root, ".git")); err == nil {
						roots = ancestors[:i+1]
						break
					}
					if _, err := os.Stat(filepath.Join(root, ".hg")); err == nil {
						roots = ancestors[:i+1]
						break
					}
				}
				if !project {
					roots = []string{global}
				}
				for _, root := range roots {
					instructions = append(instructions, filepath.Join(root, pattern))
				}
			}
		}
	}
	config["instructions"] = instructions
	return nil
}

func (r *runtime) runtimeSkills(config map[string]any, home, dir string, ancestors, dirs []string) error {
	var roots []string
	{
		external := []string{".agents"}
		if !runtimeFlag(r, "OPENCODE_DISABLE_CLAUDE_CODE") && !runtimeFlag(r, "OPENCODE_DISABLE_CLAUDE_CODE_SKILLS") {
			external = append([]string{".claude"}, external...)
		}
		bases := []string{home}
		if !runtimeFlag(r, "OPENCODE_DISABLE_PROJECT_CONFIG") {
			projectRoots := slices.Clone(ancestors)
			slices.Reverse(projectRoots)
			bases = append(bases, projectRoots...)
		}
		for _, name := range external {
			for _, base := range bases {
				root := filepath.Join(base, name, "skills")
				if !slices.Contains(roots, root) {
					roots = append(roots, root)
				}
			}
		}
	}
	for _, source := range dirs {
		for _, name := range []string{"skill", "skills"} {
			roots = append(roots, filepath.Join(source, name))
		}
	}
	var urls []any
	if list, ok := config["skills"].([]any); ok {
		for _, item := range list {
			if p, ok := item.(string); ok {
				if strings.HasPrefix(p, "https://") || strings.HasPrefix(p, "http://") {
					urls = append(urls, p)
					continue
				}
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
	config["skills"] = append(paths, urls...)
	return nil
}
