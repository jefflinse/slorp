package opencode

import (
	"fmt"
	"os"
	"path/filepath"
)

// Native external discovery follows symlinks and includes dot directories.
// Explicit containing directories preserve those matches when native rescans
// skills.paths with dot:false, without changing the reported source locations.
func externalSkillDirs(root string) ([]string, error) {
	info, err := os.Stat(root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("discovering external skills at %s: %w", root, err)
	}
	if !info.IsDir() {
		return nil, nil
	}
	var dirs []string
	ancestors := map[string]bool{}
	visited := 0
	var walk func(string, int) error
	walk = func(dir string, depth int) error {
		if depth > 64 {
			return fmt.Errorf("external skill directory nesting exceeds limit: %s", dir)
		}
		real, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return err
		}
		if ancestors[real] {
			return nil
		}
		ancestors[real] = true
		defer delete(ancestors, real)
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			visited++
			if visited > 100000 {
				return fmt.Errorf("external skill discovery exceeds entry limit: %s", root)
			}
			path := filepath.Join(dir, entry.Name())
			info, err := os.Stat(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			if info.IsDir() {
				if err := walk(path, depth+1); err != nil {
					return err
				}
			} else if entry.Name() == "SKILL.md" && info.Mode().IsRegular() {
				dirs = append(dirs, dir)
			}
		}
		return nil
	}
	if err := walk(root, 0); err != nil {
		return nil, fmt.Errorf("discovering external skills at %s: %w", root, err)
	}
	return dirs, nil
}
