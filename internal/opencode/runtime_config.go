package opencode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

func slorpSpec(spec string) bool {
	if spec == "opencode-slorp" || strings.HasPrefix(spec, "opencode-slorp@") {
		return true
	}
	p := localPath(spec, "")
	if p == "" {
		return false
	}
	if _, ok := pluginAt(p); ok {
		return true
	}
	_, ok := pluginAt(filepath.Dir(p))
	return ok
}

func (a *Adapter) runtimeConfig(r *runtime, config map[string]any) error {
	if runtimeFlag(r, "OPENCODE_PURE") {
		return fmt.Errorf("managed OpenCode requires the Slorp bridge plugin; OPENCODE_PURE is unsupported")
	}
	private, err := filepath.EvalSymlinks(r.private)
	if err != nil {
		return fmt.Errorf("resolving private configuration directory: %w", err)
	}
	r.private = private
	if err := a.runtimeDiscovery(r, config); err != nil {
		return fmt.Errorf("preserving native OpenCode discovery: %w", err)
	}
	files, _, err := bundled(a.URL)
	if err != nil {
		return err
	}
	bundle := filepath.Join(r.private, "slorp-bundle")
	if err := os.Mkdir(bundle, 0o700); err != nil {
		return err
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(bundle, name), data, 0o600); err != nil {
			return err
		}
	}
	plugins := []any{}
	if list, ok := config["plugins"].([]any); ok {
		for _, entry := range list {
			spec := pluginSpec(entry)
			if strings.HasPrefix(spec, ".") {
				spec = filepath.Join(r.dir, spec)
				if object, ok := entry.(map[string]any); ok {
					object["package"] = spec
				} else {
					entry = spec
				}
			}
			if !slorpSpec(spec) {
				if source := localPath(spec, filepath.Join(r.dir, "opencode.json")); source != "" && !withinToolScope(r.private, source) {
					info, err := os.Stat(source)
					if err != nil {
						return fmt.Errorf("reading OpenCode plugin: %w", err)
					}
					if !info.IsDir() {
						return fmt.Errorf("OpenCode V2 plugins must be package directories: %s", source)
					}
					source, err = filepath.EvalSymlinks(source)
					if err != nil {
						return err
					}
					target := filepath.Join(r.private, "plugin-sources", fmt.Sprint(len(plugins)), "root", strings.TrimPrefix(source, string(filepath.Separator)))
					if err := privateToolScope(source, target); err != nil {
						return fmt.Errorf("copying OpenCode plugin privately: %w", err)
					}
					spec = (&url.URL{Scheme: "file", Path: target}).String()
					if object, ok := entry.(map[string]any); ok {
						object["package"] = spec
					} else {
						entry = spec
					}
				}
				plugins = append(plugins, entry)
			}
		}
	}
	config["plugins"] = append(plugins, (&url.URL{Scheme: "file", Path: bundle}).String())
	// An idle directory's services are otherwise unloaded, the bridge with them.
	experimental, _ := config["experimental"].(map[string]any)
	if experimental == nil {
		experimental = map[string]any{}
	}
	experimental["location_idle_timeout"] = false
	config["experimental"] = experimental
	config["$schema"] = "https://opencode.ai/config.json"
	data, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("encoding private OpenCode configuration: %w", err)
	}
	// Substitution precedes JSON parsing in native ConfigVariable. Escape only
	// template openers in the serialized, already-resolved configuration.
	data = bytes.ReplaceAll(data, []byte("{env:"), []byte(`\u007benv:`))
	data = bytes.ReplaceAll(data, []byte("{file:"), []byte(`\u007bfile:`))
	path := filepath.Join(r.private, "opencode.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	// Only configuration discovery changes; native data/state/cache stay put.
	replaceEnv(r.cmd, "XDG_CONFIG_HOME", filepath.Join(r.private, "config"))
	replaceEnv(r.cmd, "OPENCODE_TEST_HOME", filepath.Join(r.private, "home"))
	replaceEnv(r.cmd, "OPENCODE_CONFIG_DIR", r.private)
	replaceEnv(r.cmd, "OPENCODE_CONFIG", "")
	replaceEnv(r.cmd, "OPENCODE_CONFIG_CONTENT", "")
	replaceEnv(r.cmd, "OPENCODE_DISABLE_PROJECT_CONFIG", "true")
	replaceEnv(r.cmd, "OPENCODE_CONFIG_PROJECT_DISABLE", "true")
	return nil
}

// marshalConfigOrder preserves order-sensitive package export conditions.
func marshalConfigOrder(value any, original json.RawMessage) ([]byte, error) {
	switch value := value.(type) {
	case map[string]any:
		var keys []string
		fields := map[string]json.RawMessage{}
		if len(original) > 0 {
			decoder := json.NewDecoder(bytes.NewReader(original))
			if token, err := decoder.Token(); err == nil && token == json.Delim('{') {
				for decoder.More() {
					token, err := decoder.Token()
					if err != nil {
						return nil, err
					}
					key, ok := token.(string)
					if !ok {
						return nil, fmt.Errorf("invalid configuration property")
					}
					var raw json.RawMessage
					if err := decoder.Decode(&raw); err != nil {
						return nil, err
					}
					fields[key] = raw
					if _, exists := value[key]; exists {
						keys = append(keys, key)
					}
				}
			}
		}
		var added []string
		for key := range value {
			if _, exists := fields[key]; !exists {
				added = append(added, key)
			}
		}
		slices.Sort(added)
		keys = append(keys, added...)
		var out bytes.Buffer
		out.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				out.WriteByte(',')
			}
			name, err := json.Marshal(key)
			if err != nil {
				return nil, err
			}
			data, err := marshalConfigOrder(value[key], fields[key])
			if err != nil {
				return nil, err
			}
			out.Write(name)
			out.WriteByte(':')
			out.Write(data)
		}
		out.WriteByte('}')
		return out.Bytes(), nil
	case []any:
		var prior []json.RawMessage
		if len(original) > 0 && original[0] == '[' {
			if err := json.Unmarshal(original, &prior); err != nil {
				return nil, err
			}
		}
		items := make([]json.RawMessage, len(value))
		for i, item := range value {
			var reference json.RawMessage
			if i < len(prior) {
				reference = prior[i]
			}
			data, err := marshalConfigOrder(item, reference)
			if err != nil {
				return nil, err
			}
			items[i] = data
		}
		return json.Marshal(items)
	default:
		return json.Marshal(value)
	}
}

func pluginSpec(entry any) string {
	if object, ok := entry.(map[string]any); ok {
		entry = object["package"]
	}
	spec, _ := entry.(string)
	return spec
}

func runtimeEnv(r *runtime, key string) string {
	if r.cmd != nil {
		for _, entry := range r.cmd.Env {
			if value, ok := strings.CutPrefix(entry, key+"="); ok {
				return value
			}
		}
	}
	return ""
}

func runtimeFlag(r *runtime, key string) bool {
	if key == "OPENCODE_DISABLE_PROJECT_CONFIG" && r.cmd != nil {
		for _, entry := range r.cmd.Env {
			if value, ok := strings.CutPrefix(entry, "OPENCODE_CONFIG_PROJECT_DISABLE="); ok {
				return value == "1" || strings.EqualFold(value, "true")
			}
		}
	}
	v := strings.ToLower(runtimeEnv(r, key))
	return v == "true" || v == "1"
}
