package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const configDirectoriesKey = "slorp:directories"
const configCommandsKey = "slorp:command-sources"

type nativeConfigEntry struct {
	Type string         `json:"type"`
	Path string         `json:"path"`
	Info map[string]any `json:"info"`
}

// resolvedConfig materializes V2's ordered sources without executing user plugins.
func (r *runtime) resolvedConfig(ctx context.Context) (map[string]any, error) {
	var entries []nativeConfigEntry
	if err := r.request(ctx, http.MethodGet, "/api/config", nil, &entries); err != nil {
		return nil, err
	}
	config := map[string]any{}
	var dirs []any
	var commands []any
	for _, entry := range entries {
		switch entry.Type {
		case "document":
			if entry.Path == "" {
				for _, plugin := range configList(entry.Info, "plugins") {
					if slorpSpec(pluginSpec(plugin)) {
						return nil, fmt.Errorf("managed OpenCode cannot replace a remote-origin or inline Slorp plugin; configure Slorp in a local configuration file")
					}
				}
			}
			base := r.dir
			if entry.Path != "" {
				base = filepath.Dir(entry.Path)
			}
			rebaseConfig(entry.Info, base)
			mergeConfigDocument(config, entry.Info)
			if definitions, ok := entry.Info["commands"].(map[string]any); ok {
				commands = append(commands, map[string]any{"commands": definitions})
			}
		case "directory":
			dirs = append(dirs, entry.Path)
			commands = append(commands, map[string]any{"path": entry.Path})
		default:
			return nil, fmt.Errorf("unknown OpenCode configuration source %q", entry.Type)
		}
	}
	config[configDirectoriesKey] = dirs
	config[configCommandsKey] = commands
	// Model discovery waits for the location's configuration adapters to load.
	if err := r.request(ctx, http.MethodGet, "/api/model", nil, nil); err != nil {
		return nil, err
	}
	var agents []map[string]any
	if err := r.request(ctx, http.MethodGet, "/api/agent", nil, &agents); err != nil {
		return nil, err
	}
	resolved, _ := config["agents"].(map[string]any)
	if resolved == nil {
		resolved = map[string]any{}
	}
	for _, agent := range agents {
		id, _ := agent["id"].(string)
		if _, exists := config["default_agent"]; !exists && agent["hidden"] != true && (agent["mode"] == "primary" || agent["mode"] == "all") {
			config["default_agent"] = id
		}
		delete(agent, "id")
		delete(agent, "name")
		if request, ok := agent["request"].(map[string]any); ok {
			delete(request, "settings")
		}
		if model, ok := agent["model"].(map[string]any); ok {
			model["model"] = model["id"]
			delete(model, "id")
		}
		resolved[id] = agent
	}
	config["agents"] = resolved
	return config, nil
}

func configList(config map[string]any, key string) []any {
	list, _ := config[key].([]any)
	return list
}

func mergeConfigDocument(dst, src map[string]any) {
	for key, value := range src {
		switch key {
		case "policies":
			dst[key] = append(slices.Clone(configList(src, key)), configList(dst, key)...)
		case "permissions", "instructions", "skills", "plugins":
			dst[key] = append(configList(dst, key), configList(src, key)...)
		case "commands", "references":
			previous, _ := dst[key].(map[string]any)
			if previous == nil {
				previous = map[string]any{}
			}
			for name, entry := range value.(map[string]any) {
				previous[name] = entry
			}
			dst[key] = previous
		case "mcp":
			object, _ := value.(map[string]any)
			previous, _ := dst[key].(map[string]any)
			if previous == nil {
				previous = map[string]any{}
			}
			for field, item := range object {
				if field == "servers" {
					servers, _ := previous[field].(map[string]any)
					if servers == nil {
						servers = map[string]any{}
					}
					for name, server := range item.(map[string]any) {
						servers[name] = server
					}
					previous[field] = servers
				} else {
					mergeConfig(previous, map[string]any{field: item})
				}
			}
			dst[key] = previous
		default:
			mergeConfig(dst, map[string]any{key: value})
		}
	}
}

func mergeConfig(dst, src map[string]any) {
	for key, value := range src {
		if object, ok := value.(map[string]any); ok {
			existing, _ := dst[key].(map[string]any)
			if existing == nil {
				existing = map[string]any{}
			}
			mergeConfig(existing, object)
			dst[key] = existing
		} else if key == "variants" {
			dst[key] = append(configList(dst, key), configList(src, key)...)
		} else {
			dst[key] = value
		}
	}
}

func rebaseConfig(config map[string]any, base string) {
	// Skill and instruction paths resolve against the location, not the document.
	plugins, _ := config["plugins"].([]any)
	for i, entry := range plugins {
		spec := pluginSpec(entry)
		if strings.HasPrefix(spec, ".") {
			spec = filepath.Join(base, spec)
			if object, ok := entry.(map[string]any); ok {
				object["package"] = spec
			} else {
				plugins[i] = spec
			}
		}
	}
	references, _ := config["references"].(map[string]any)
	for name, entry := range references {
		if value, ok := entry.(string); ok && strings.HasPrefix(value, ".") {
			references[name] = filepath.Join(base, value)
		} else if object, ok := entry.(map[string]any); ok {
			if value, ok := object["path"].(string); ok && !filepath.IsAbs(value) && !strings.HasPrefix(value, "~/") {
				object["path"] = filepath.Join(base, value)
			}
		}
	}
}

func (r *runtime) preserveDefinitions(config map[string]any, dirs []string) error {
	if err := r.preserveCommands(config); err != nil {
		return err
	}
	var discovered []any
	for _, dir := range dirs {
		for _, name := range []string{"plugin", "plugins"} {
			entries, err := os.ReadDir(filepath.Join(dir, name))
			if err != nil && !os.IsNotExist(err) {
				return err
			}
			for _, entry := range entries {
				path := filepath.Join(dir, name, entry.Name())
				info, err := os.Stat(path)
				if err != nil {
					return fmt.Errorf("reading OpenCode plugin source: %w", err)
				}
				if info.IsDir() && !slorpSpec(path) {
					discovered = append(discovered, path)
				}
			}
		}
	}
	config["plugins"] = append(discovered, configList(config, "plugins")...)
	return nil
}

// Command definitions replace by name in source order. JSON frontmatter is valid
// YAML, so native retains responsibility for validation and template execution.
func (r *runtime) preserveCommands(config map[string]any) error {
	commands := map[string][]byte{}
	scratch := filepath.Join(r.private, ".command-sources")
	defer removePrivate(scratch)
	snapshot := &configSnapshot{ctx: context.Background()}
	for i, source := range configList(config, configCommandsKey) {
		entry, _ := source.(map[string]any)
		if dir, ok := entry["path"].(string); ok {
			for _, name := range []string{"command", "commands"} {
				root := filepath.Join(scratch, fmt.Sprint(i), name)
				if err := snapshot.copy(filepath.Join(dir, name), root, 1); err != nil {
					return fmt.Errorf("copying OpenCode command sources: %w", err)
				}
				err := filepath.WalkDir(root, func(path string, e os.DirEntry, err error) error {
					if os.IsNotExist(err) {
						return nil
					}
					if err != nil {
						return err
					}
					if e.IsDir() || filepath.Ext(path) != ".md" {
						return nil
					}
					data, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					relative, err := filepath.Rel(root, path)
					if err != nil {
						return err
					}
					commands[strings.TrimSuffix(relative, ".md")] = data
					return nil
				})
				if err != nil {
					return err
				}
			}
		}
		definitions, _ := entry["commands"].(map[string]any)
		for name, definition := range definitions {
			object, _ := definition.(map[string]any)
			fields := make(map[string]any, len(object))
			for key, value := range object {
				if key != "template" {
					fields[key] = value
				}
			}
			frontmatter, err := json.Marshal(fields)
			if err != nil {
				return err
			}
			template, _ := object["template"].(string)
			commands[name] = []byte("---\n" + string(frontmatter) + "\n---\n" + template)
		}
	}
	delete(config, configCommandsKey)
	delete(config, "commands")
	root := filepath.Join(r.private, "commands")
	for name, data := range commands {
		path := filepath.Join(root, name+".md")
		if !withinToolScope(root, path) {
			return fmt.Errorf("OpenCode command name cannot be represented privately: %q", name)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return err
		}
	}
	return nil
}
