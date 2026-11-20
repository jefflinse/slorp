package opencode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type toolScope struct {
	source, target string
	localDeps      []string
	bytes          int64
	entries        int
}

func privateToolScope(source, target string) error {
	if !filepath.IsAbs(source) || !filepath.IsAbs(target) || !strings.HasSuffix(target, source) || withinToolScope(source, target) {
		return fmt.Errorf("invalid private custom tool mirror: %s", target)
	}
	if err := rejectToolAncestorScopes(source); err != nil {
		return err
	}
	s := toolScope{source: source, target: target}
	manifest, original, err := s.manifest(filepath.Join(source, "package.json"))
	if err != nil {
		return err
	}
	manifest["private"] = true
	data, err := marshalConfigOrder(manifest, original)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(target, "package.json"), data, 0o600); err != nil {
		return err
	}
	if err := s.copyModules(source, target, 0); err != nil {
		return err
	}
	for _, dependency := range s.localDeps {
		real, err := filepath.EvalSymlinks(dependency)
		if err != nil {
			return fmt.Errorf("resolving private tool dependency: %w", err)
		}
		if !withinToolScope(target, real) {
			return fmt.Errorf("unsupported local tool dependency through a source link: %s", dependency)
		}
	}
	return mirrorToolAncestors(source, target)
}

// Ancestor packages are outside the private install graph. Until that graph can
// preserve native ancestor resolution, reject these scopes rather than hide or
// substitute their packages with dependencies hoisted from another tool scope.
func rejectToolAncestorScopes(source string) error {
	for parent := filepath.Dir(source); ; parent = filepath.Dir(parent) {
		path := filepath.Join(parent, "package.json")
		info, err := os.Stat(path)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil {
			if info.Size() > 16<<20 {
				return fmt.Errorf("custom tool ancestor manifest exceeds snapshot size limit: %s", path)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			var manifest struct {
				Workspaces           json.RawMessage            `json:"workspaces"`
				Dependencies         map[string]json.RawMessage `json:"dependencies"`
				DevDependencies      map[string]json.RawMessage `json:"devDependencies"`
				OptionalDependencies map[string]json.RawMessage `json:"optionalDependencies"`
				PeerDependencies     map[string]json.RawMessage `json:"peerDependencies"`
			}
			if err := json.Unmarshal(data, &manifest); err != nil {
				return fmt.Errorf("reading custom tool ancestor manifest %s: %w", path, err)
			}
			if len(manifest.Workspaces) > 0 {
				return fmt.Errorf("unsupported custom tool workspace in ancestor %s: managed dependency installation requires a standalone config package", path)
			}
			if len(manifest.Dependencies)+len(manifest.DevDependencies)+len(manifest.OptionalDependencies)+len(manifest.PeerDependencies) > 0 {
				return toolAncestorDependencyError(path)
			}
		}
		modules := filepath.Join(parent, "node_modules")
		entries, err := os.ReadDir(modules)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), ".") {
				return toolAncestorDependencyError(modules)
			}
		}
		if filepath.Dir(parent) == parent {
			return nil
		}
	}
}

func toolAncestorDependencyError(path string) error {
	return fmt.Errorf("unsupported custom tool ancestor dependency scope at %s: managed tools require a standalone config location without ancestor packages; source node_modules will not be installed into or silently ignored", path)
}

// Only shallow sibling links are created, never copies of ancestor codebases.
// Package-manager inputs are excluded so reification cannot write through them.
func mirrorToolAncestors(source, target string) error {
	if !filepath.IsAbs(source) || !filepath.IsAbs(target) || withinToolScope(source, target) || !strings.HasSuffix(target, source) {
		return fmt.Errorf("invalid private custom tool mirror: %s", target)
	}
	links := 0
	for filepath.Dir(source) != source {
		parent, dest := filepath.Dir(source), filepath.Dir(target)
		entries, err := os.ReadDir(parent)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			name := entry.Name()
			if name == filepath.Base(source) || slices.Contains([]string{"package.json", "node_modules", ".git", "package-lock.json", "npm-shrinkwrap.json", "bun.lock", "bun.lockb"}, name) {
				continue
			}
			links++
			if links > 16384 {
				return fmt.Errorf("custom tool ancestor links exceed snapshot limit")
			}
			if err := os.Symlink(filepath.Join(parent, name), filepath.Join(dest, name)); err != nil {
				return err
			}
		}
		source, target = parent, dest
	}
	return nil
}

func withinToolScope(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && !filepath.IsAbs(relative) && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// User workspaces and links outside the config root need a separate install
// graph. Reject them instead of letting private npm installs reach source trees.
func (s *toolScope) manifest(path string) (map[string]any, []byte, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return map[string]any{}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if info.Size() > 16<<20 {
		return nil, nil, fmt.Errorf("custom tool manifest exceeds snapshot size limit")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, nil, fmt.Errorf("reading custom tool package %s: %w", path, err)
	}
	if manifest == nil {
		return nil, nil, fmt.Errorf("custom tool package %s must be a JSON object", path)
	}
	if _, ok := manifest["workspaces"]; ok {
		return nil, nil, fmt.Errorf("unsupported custom tool workspace in %s: managed dependency installation requires a standalone config package", path)
	}
	for _, key := range []string{"dependencies", "devDependencies", "optionalDependencies", "peerDependencies"} {
		deps, _ := manifest[key].(map[string]any)
		for name, value := range deps {
			spec, ok := value.(string)
			if !ok {
				continue
			}
			if strings.HasPrefix(spec, "workspace:") || strings.HasPrefix(spec, "link:") {
				return nil, nil, fmt.Errorf("unsupported custom tool workspace/link dependency %q in %s", name, path)
			}
			if !strings.HasPrefix(spec, "file:") && !strings.HasPrefix(spec, ".") && !filepath.IsAbs(spec) {
				continue
			}
			local := strings.TrimPrefix(spec, "file:")
			if !filepath.IsAbs(local) {
				local = filepath.Join(filepath.Dir(path), local)
			}
			real, err := filepath.EvalSymlinks(local)
			if err != nil {
				return nil, nil, fmt.Errorf("resolving custom tool dependency %q: %w", name, err)
			}
			if !withinToolScope(s.source, real) {
				return nil, nil, fmt.Errorf("unsupported custom tool dependency %q outside config root: %s", name, path)
			}
			relative, err := filepath.Rel(s.source, real)
			if err != nil {
				return nil, nil, err
			}
			private := filepath.Join(s.target, relative)
			s.localDeps = append(s.localDeps, private)
			deps[name] = "file:" + private
		}
	}
	return manifest, data, nil
}

func (s *toolScope) copyModules(source, target string, depth int) error {
	if depth > 32 {
		return fmt.Errorf("custom tool module nesting exceeds limit")
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == "node_modules" || name == ".git" || depth == 0 && slices.Contains([]string{"package.json", "package-lock.json", "npm-shrinkwrap.json", "bun.lock", "bun.lockb"}, name) {
			continue
		}
		s.entries++
		if s.entries > 16384 {
			return fmt.Errorf("custom tool modules exceed snapshot entry limit")
		}
		from, to := filepath.Join(source, name), filepath.Join(target, name)
		info, err := os.Stat(from)
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			real, err := filepath.EvalSymlinks(from)
			if err != nil {
				return err
			}
			if !withinToolScope(s.source, real) && name != "package.json" {
				if err := os.Symlink(from, to); err != nil {
					return err
				}
				continue
			}
		}
		if info.IsDir() && !slices.Contains([]string{"bin", "resources", "skill", "skills", "agent", "agents", "command", "commands", "plugin", "plugins"}, name) {
			if err := s.copyModules(from, to, depth+1); err != nil {
				return err
			}
		} else if info.Mode().IsRegular() && slices.Contains([]string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".json", ".jsonc"}, filepath.Ext(name)) {
			s.bytes += info.Size()
			if info.Size() > 16<<20 || s.bytes > 64<<20 {
				return fmt.Errorf("custom tool modules exceed snapshot size limit")
			}
			var data []byte
			if name == "package.json" {
				manifest, original, err := s.manifest(from)
				if err != nil {
					return err
				}
				data, err = marshalConfigOrder(manifest, original)
				if err != nil {
					return err
				}
			} else {
				data, err = os.ReadFile(from)
				if err != nil {
					return err
				}
			}
			if err := os.WriteFile(to, data, 0o600); err != nil {
				return err
			}
		} else if err := os.Symlink(from, to); err != nil {
			return err
		}
	}
	return nil
}
