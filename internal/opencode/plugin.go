package opencode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	opencodeplugin "github.com/jefflinse/slorp/integrations/opencode"
	"github.com/jefflinse/slorp/internal/hub"
)

type manifest struct {
	Name    string            `json:"name"`
	Version string            `json:"version"`
	Type    string            `json:"type,omitempty"`
	Main    string            `json:"main"`
	Exports map[string]string `json:"exports"`
	Managed bool              `json:"slorpManaged,omitempty"`
}

var bundleFiles = []string{"package.json", "slorp.ts", "tui.ts", "presence.ts", "quotas.ts", "native.ts", "forms.ts", "service.js", "server.ts"}

func bundled(address string) (map[string][]byte, string, error) {
	files := map[string][]byte{}
	for _, name := range bundleFiles {
		if name == "server.ts" {
			continue
		}
		b, err := opencodeplugin.Files.ReadFile(name)
		if err != nil {
			return nil, "", err
		}
		files[name] = b
	}
	var m manifest
	if err := json.Unmarshal(files["package.json"], &m); err != nil {
		return nil, "", err
	}
	m.Main, m.Type, m.Managed = "server.ts", "module", true
	if m.Exports == nil {
		m.Exports = map[string]string{}
	}
	m.Exports["."], m.Exports["./tui"] = "./server.ts", "./tui.ts"
	files["package.json"], _ = json.MarshalIndent(m, "", "  ")
	url, _ := json.Marshal(address)
	files["server.ts"] = []byte("import { Slorp } from './slorp.ts'\nimport { revision } from './revision.ts'\nexport default { id: 'slorp', setup: ctx => Slorp(ctx, { url: " + string(url) + ", ...ctx.options, revision }) }\n")
	files["revision.ts"] = []byte("export const revision = '" + fingerprint(files) + "'\n")
	return files, m.Version, nil
}

func (a *Adapter) managedPath() string { return filepath.Join(a.Data, "plugins", "opencode") }

func (a *Adapter) configDirs() []string {
	dir := a.ConfigDir
	if dir == "" {
		dir = filepath.Join(a.Home, ".config", "opencode")
	}
	dirs := []string{dir}
	if a.ExtraConfigDir != "" && !samePath(dir, a.ExtraConfigDir) {
		dirs = append(dirs, a.ExtraConfigDir)
	}
	return dirs
}

func (a *Adapter) configs(tui bool) []string {
	if tui {
		return []string{filepath.Join(a.configDirs()[0], "cli.json")}
	}
	dir := a.serverConfigDir()
	paths := []string{filepath.Join(dir, "opencode.json"), filepath.Join(dir, "opencode.jsonc")}
	if a.ConfigFile != "" && !slices.Contains(paths, a.ConfigFile) {
		paths = append(paths, a.ConfigFile)
	}
	return paths
}

func (a *Adapter) serverConfigDir() string {
	if a.ExtraConfigDir != "" {
		return a.ExtraConfigDir
	}
	return a.configDirs()[0]
}

func pluginAt(path string) (manifest, bool) {
	var m manifest
	b, err := os.ReadFile(filepath.Join(path, "package.json"))
	if err == nil && json.Unmarshal(b, &m) == nil && m.Name == "opencode-slorp" {
		return m, true
	}
	return m, false
}

func (a *Adapter) inspect() (hub.Plugin, error) {
	if _, err := exec.LookPath(a.Bin); err != nil {
		return hub.Plugin{}, fmt.Errorf("OpenCode executable not found: %s", a.Bin)
	}
	var paths []string
	for _, cfg := range a.configs(false) {
		b, err := os.ReadFile(cfg)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return hub.Plugin{}, err
		}
		list, err := entries(b)
		if err != nil {
			return hub.Plugin{}, fmt.Errorf("%s: %w", cfg, err)
		}
		for _, entry := range list {
			if path := localPath(entry.spec, cfg); path != "" {
				paths = append(paths, path)
			}
		}
	}
	dir := filepath.Join(a.serverConfigDir(), "plugins")
	children, _ := os.ReadDir(dir)
	for _, child := range children {
		paths = append(paths, filepath.Join(dir, child.Name()))
	}
	serverPaths := slices.Clone(paths)
	var terminalPaths []string
	for _, cfg := range a.configs(true) {
		b, err := os.ReadFile(cfg)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return hub.Plugin{}, err
		}
		list, err := entries(b)
		if err != nil {
			return hub.Plugin{}, fmt.Errorf("%s: %w", cfg, err)
		}
		for _, e := range list {
			if path := localPath(e.spec, cfg); path != "" {
				paths = append(paths, path)
				terminalPaths = append(terminalPaths, path)
			}
		}
	}
	var p hub.Plugin
	for _, path := range paths {
		m, ok := pluginAt(path)
		if !ok {
			continue
		}
		if p.Installed && !samePath(path, p.Path) {
			return p, errors.New("multiple Slorp plugins are configured; remove the duplicate before changing setup")
		}
		p = hub.Plugin{Installed: true, Enabled: true, Path: path, Version: m.Version, Source: "local", NeedsSetup: true}
		if m.Managed && samePath(path, a.managedPath()) {
			p.Source = "managed"
			files, version, err := bundled(a.URL)
			if err != nil {
				return p, err
			}
			installed := map[string][]byte{}
			for name, want := range files {
				got, err := os.ReadFile(filepath.Join(path, name))
				installed[name] = got
				if err != nil || string(got) != string(want) {
					p.UpdateAvailable = true
				}
			}
			p.Revision = fingerprint(installed)
			if p.UpdateAvailable {
				p.Latest = version
			}
		}
	}
	if !p.Installed {
		return p, nil
	}
	p.Enabled = slices.ContainsFunc(serverPaths, func(path string) bool { return samePath(path, p.Path) })
	p.NeedsSetup = !p.Enabled ||
		slices.ContainsFunc(terminalPaths, func(path string) bool { return samePath(path, p.Path) })
	for _, configs := range [][]string{a.configs(false), a.configs(true)} {
		enabled := true
		for _, cfg := range configs {
			b, err := os.ReadFile(cfg)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return p, err
			}
			list, err := entries(b)
			if err != nil {
				return p, err
			}
			for _, e := range list {
				switch e.spec {
				case "-slorp", "-*":
					enabled = false
				case "slorp", "*":
					enabled = true
				}
			}
		}
		if !enabled {
			p.Enabled = false
		}
	}
	return p, nil
}

func (a *Adapter) publish(p hub.Plugin, err error) {
	if err != nil {
		p.Error = err.Error()
	}
	a.Hub.SetPlugin(Agent, p)
}

// CheckPlugin reports the globally configured plugin and its setup state.
func (a *Adapter) CheckPlugin() {
	if !a.pluginMu.TryLock() {
		return
	}
	defer a.pluginMu.Unlock()
	a.publish(a.inspect())
}

// install registers the local package in OpenCode's global server configuration.
func (a *Adapter) install(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	registered := false
	for _, cfg := range a.configs(false) {
		b, err := os.ReadFile(cfg)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		list, err := entries(b)
		if err != nil {
			return fmt.Errorf("%s: %w", cfg, err)
		}
		for _, entry := range list {
			if samePath(localPath(entry.spec, cfg), path) {
				registered = true
			}
		}
	}
	if !registered {
		cfg := filepath.Join(a.serverConfigDir(), "opencode.json")
		if _, err := os.Stat(cfg + "c"); err == nil {
			cfg += "c"
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := addReference(cfg, path); err != nil {
			return err
		}
	}
	for _, cfg := range a.configs(true) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := removeReference(cfg, path); err != nil {
			return err
		}
	}
	p, err := a.inspect()
	if err != nil {
		return err
	}
	if !p.Installed || p.NeedsSetup || !samePath(p.Path, path) {
		return errors.New("OpenCode did not register the Slorp plugin package")
	}
	return nil
}

func (a *Adapter) requireV2(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := a.command(ctx, "--version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("checking OpenCode version: %w: %s", err, strings.TrimSpace(string(out)))
	}
	version := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(string(out)), "opencode "), "v")
	if !strings.HasPrefix(version, "2.") {
		return fmt.Errorf("Slorp requires OpenCode 2.x; found %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func (a *Adapter) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, a.Bin, args...)
	cmd.Dir = a.Home
	cmd.Env = os.Environ()
	for key, value := range map[string]string{"HOME": a.Home, "XDG_CONFIG_HOME": filepath.Dir(a.configDirs()[0]), "OPENCODE_CONFIG_DIR": a.ExtraConfigDir, "OPENCODE_CONFIG": a.ConfigFile} {
		cmd.Env = slices.DeleteFunc(cmd.Env, func(s string) bool { return strings.HasPrefix(s, key+"=") })
		if value != "" {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	return cmd
}

func (a *Adapter) writeBundle() error {
	path := a.managedPath()
	if _, err := os.Stat(path); err == nil {
		m, ok := pluginAt(path)
		if !ok || !m.Managed {
			return fmt.Errorf("%s is not a Slorp-managed plugin directory", path)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	files, _, err := bundled(a.URL)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	// The manifest marks a newly created directory before an interrupted write.
	for _, name := range append(slices.Clone(bundleFiles), "revision.ts") {
		if err := atomicFile(filepath.Join(path, name), files[name], 0o600); err != nil {
			return err
		}
	}
	return nil
}

// Install installs the bundled plugin, or registers an existing local package's TUI companion.
func (a *Adapter) Install(ctx context.Context) error {
	a.pluginMu.Lock()
	defer a.pluginMu.Unlock()
	if err := a.requireV2(ctx); err != nil {
		return err
	}
	p, err := a.inspect()
	if err != nil {
		return err
	}
	if p.Installed && !p.NeedsSetup && !(p.Source == "managed" && p.UpdateAvailable) {
		return a.install(ctx, p.Path)
	}
	path := p.Path
	if !p.Installed || p.Source == "managed" {
		if err := a.writeBundle(); err != nil {
			return err
		}
		path = a.managedPath()
	} else {
		m, _ := pluginAt(path)
		if m.Exports["./tui"] == "" {
			return errors.New("update this local Slorp plugin package before completing setup")
		}
	}
	err = a.install(ctx, path)
	a.publish(a.inspect())
	return err
}

// Update replaces a managed plugin with the copy shipped in this Slorp executable.
func (a *Adapter) Update(ctx context.Context) error {
	a.pluginMu.Lock()
	defer a.pluginMu.Unlock()
	if err := a.requireV2(ctx); err != nil {
		return err
	}
	p, err := a.inspect()
	if err != nil {
		return err
	}
	if p.Source != "managed" {
		return errors.New("only a Slorp-managed plugin can be updated here")
	}
	if err := a.writeBundle(); err != nil {
		return err
	}
	err = a.install(ctx, p.Path)
	a.publish(a.inspect())
	return err
}

// Uninstall removes references to the managed plugin, retaining its inert files.
func (a *Adapter) Uninstall(ctx context.Context) error {
	a.pluginMu.Lock()
	defer a.pluginMu.Unlock()
	p, err := a.inspect()
	if err != nil {
		return err
	}
	if !p.Installed {
		return nil
	}
	if p.Source != "managed" {
		return errors.New("manage this external plugin in OpenCode's configuration")
	}
	for _, cfg := range append(a.configs(false), a.configs(true)...) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := removeReference(cfg, p.Path); err != nil {
			a.publish(a.inspect())
			return err
		}
	}
	a.publish(a.inspect())
	return nil
}

// fingerprint identifies the bundled runtime, including its configured address.
func fingerprint(files map[string][]byte) string {
	h := sha256.New()
	for _, name := range bundleFiles {
		h.Write(files[name])
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}
