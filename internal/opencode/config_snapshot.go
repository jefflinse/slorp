package opencode

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// configSnapshot gives native config discovery disposable copies. OpenCode's
// config reader writes schema/dependency files even in pure mode.
type configSnapshot struct {
	ctx   context.Context
	home  string
	paths map[string]string
	bytes int64
}

var fileVariable = regexp.MustCompile(`\{file:([^}]+)\}`)

func rebaseFiles(b []byte, dir, home string) []byte {
	return fileVariable.ReplaceAllFunc(b, func(match []byte) []byte {
		name := string(match[len("{file:") : len(match)-1])
		if filepath.IsAbs(name) {
			return match
		}
		if strings.HasPrefix(name, "~/") {
			name = filepath.Join(home, strings.TrimPrefix(name, "~/"))
		} else {
			name = filepath.Join(dir, name)
		}
		value := "{file:" + name + "}"
		return []byte(value)
	})
}

func (s *configSnapshot) copy(source, target string, depth int) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	info, err := os.Stat(source)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if depth > 32 {
		return fmt.Errorf("configuration directory nesting exceeds limit: %s", source)
	}
	if info.IsDir() {
		if err := os.MkdirAll(target, 0o700); err != nil {
			return err
		}
		entries, err := os.ReadDir(source)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if depth == 0 && !slices.Contains([]string{"opencode.json", "opencode.jsonc", "agent", "agents", "command", "commands", "plugin", "plugins"}, e.Name()) {
				continue
			}
			if e.Name() == "node_modules" || e.Name() == ".git" || e.Name() == "bin" {
				continue
			}
			if depth == 0 && (e.Name() == "plugin" || e.Name() == "plugins") {
				if err := os.Symlink(filepath.Join(source, e.Name()), filepath.Join(target, e.Name())); err != nil {
					return err
				}
				continue
			}
			if err := s.copy(filepath.Join(source, e.Name()), filepath.Join(target, e.Name()), depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	if depth > 1 && filepath.Ext(source) != ".md" {
		return nil
	}
	s.bytes += info.Size()
	if info.Size() > 16<<20 || s.bytes > 64<<20 {
		return fmt.Errorf("OpenCode configuration exceeds snapshot size limit")
	}
	b, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if filepath.Ext(source) == ".json" || filepath.Ext(source) == ".jsonc" {
		b = rebaseFiles(b, filepath.Dir(source), s.home)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	return os.WriteFile(target, b, 0o600)
}

func (s *configSnapshot) tree(source, target string) error {
	s.paths[target] = source
	return s.copy(source, target, 0)
}

func (a *Adapter) snapshotConfig(ctx context.Context, r *runtime) (*configSnapshot, error) {
	private, err := filepath.EvalSymlinks(r.private)
	if err != nil {
		return nil, fmt.Errorf("resolving private configuration directory: %w", err)
	}
	r.private = private
	s := &configSnapshot{ctx: ctx, home: a.Home, paths: map[string]string{}}
	root := filepath.Join(r.private, "sources")
	config := filepath.Join(root, "config", "opencode")
	if err := s.tree(a.serverConfigDir(), config); err != nil {
		return nil, err
	}
	home := filepath.Join(root, "home")
	sourceHome := runtimeEnv(r, "OPENCODE_TEST_HOME")
	if sourceHome == "" {
		sourceHome = a.Home
	}
	s.paths[home] = sourceHome
	project := filepath.Join(root, "project")
	dir, err := filepath.Abs(r.dir)
	if err != nil {
		return nil, err
	}
	shadow := filepath.Join(project, strings.TrimPrefix(dir, string(filepath.Separator)))
	if err := os.MkdirAll(shadow, 0o700); err != nil {
		return nil, err
	}
	for current := dir; ; current = filepath.Dir(current) {
		dest := filepath.Join(project, strings.TrimPrefix(current, string(filepath.Separator)))
		s.paths[dest] = current
		if !runtimeFlag(r, "OPENCODE_DISABLE_PROJECT_CONFIG") {
			for _, name := range []string{"opencode.json", "opencode.jsonc", ".opencode"} {
				if err := s.tree(filepath.Join(current, name), filepath.Join(dest, name)); err != nil {
					return nil, err
				}
			}
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	replaceEnv(r.cmd, "XDG_CONFIG_HOME", filepath.Dir(config))
	replaceEnv(r.cmd, "OPENCODE_TEST_HOME", home)
	replaceEnv(r.cmd, "OPENCODE_CONFIG_DIR", config)
	replaceEnv(r.cmd, "OPENCODE_CONFIG", "")
	if a.ConfigFile != "" {
		file := filepath.Join(root, "custom", filepath.Base(a.ConfigFile))
		s.paths[filepath.Dir(file)] = filepath.Dir(a.ConfigFile)
		if err := s.tree(a.ConfigFile, file); err != nil {
			return nil, err
		}
		replaceEnv(r.cmd, "OPENCODE_CONFIG", file)
	}
	if inline := os.Getenv("OPENCODE_CONFIG_CONTENT"); inline != "" {
		replaceEnv(r.cmd, "OPENCODE_CONFIG_CONTENT", string(rebaseFiles([]byte(inline), dir, a.Home)))
	}
	r.dir = shadow
	return s, nil
}

func (s *configSnapshot) original(value any) any {
	value = s.originalPaths(value)
	return value
}

func (s *configSnapshot) originalPaths(value any) any {
	switch v := value.(type) {
	case string:
		if strings.HasPrefix(v, "file://") {
			if u, err := url.Parse(v); err == nil {
				u.Path = s.originalPaths(u.Path).(string)
				u.RawPath = ""
				return u.String()
			}
		}
		best := ""
		for target := range s.paths {
			if strings.Contains(v, target) && len(target) > len(best) {
				best = target
			}
		}
		if best != "" {
			return strings.ReplaceAll(v, best, s.paths[best])
		}
		return v
	case []any:
		for i := range v {
			v[i] = s.originalPaths(v[i])
		}
		return v
	case map[string]any:
		for k, x := range v {
			v[k] = s.originalPaths(x)
		}
		return v
	}
	return value
}
