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
	if runtimeEffectFlag(r, "OPENCODE_PURE") {
		return fmt.Errorf("managed OpenCode requires the Slorp bridge plugin; OPENCODE_PURE is unsupported")
	}
	private, err := filepath.EvalSymlinks(r.private)
	if err != nil {
		return fmt.Errorf("resolving private configuration directory: %w", err)
	}
	r.private = private
	if err := validateRemoteSlorp(r, config); err != nil {
		return err
	}
	if origins, ok := config["plugin_origins"].([]any); ok {
		for _, item := range origins {
			origin, _ := item.(map[string]any)
			source, _ := origin["source"].(string)
			if (strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://")) && slorpSpec(pluginSpec(origin["spec"])) {
				return fmt.Errorf("managed OpenCode cannot replace a remote-origin Slorp plugin; remove Slorp from the remote configuration and configure it locally")
			}
		}
	}
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
	if list, ok := config["plugin"].([]any); ok {
		for _, entry := range list {
			spec, _ := entry.(string)
			if pair, ok := entry.([]any); ok && len(pair) > 0 {
				spec, _ = pair[0].(string)
			}
			if strings.HasPrefix(spec, ".") {
				spec = filepath.Join(r.dir, spec)
				if pair, ok := entry.([]any); ok {
					pair[0] = spec
				} else {
					entry = spec
				}
			}
			if !slorpSpec(spec) {
				plugins = append(plugins, entry)
			}
		}
	}
	config["plugin"] = append(plugins, (&url.URL{Scheme: "file", Path: bundle}).String())
	delete(config, "plugin_origins")
	config["$schema"] = "https://opencode.ai/config.json"
	order, _ := config[configOrderKey].(json.RawMessage)
	delete(config, configOrderKey)
	data, err := marshalConfigOrder(config, order)
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
	// v1.19.6 merges local plugin origins by file URL, not package name. A
	// resolved snapshot avoids loading both the bundled and installed Slorp.
	// Only config discovery changes; HOME and native data/state/cache stay put.
	replaceEnv(r.cmd, "XDG_CONFIG_HOME", filepath.Join(r.private, "config"))
	replaceEnv(r.cmd, "OPENCODE_TEST_HOME", filepath.Join(r.private, "home"))
	replaceEnv(r.cmd, "OPENCODE_CONFIG_DIR", r.private)
	replaceEnv(r.cmd, "OPENCODE_CONFIG", path)
	replaceEnv(r.cmd, "OPENCODE_CONFIG_CONTENT", "")
	replaceEnv(r.cmd, "OPENCODE_DISABLE_PROJECT_CONFIG", "true")
	return nil
}

// Native uses object insertion order for provider/agent defaults and permission
// precedence. Keep the probe's order while replacing values and adding private inputs.
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

func validateRemoteSlorp(r *runtime, config map[string]any) error {
	plugins, _ := config["plugin"].([]any)
	if !slices.ContainsFunc(plugins, func(entry any) bool { return slorpSpec(pluginSpec(entry)) }) {
		return nil
	}
	// The v1.19.6 HTTP config schema can omit plugin_origins. In that case a
	// well-known source cannot be safely distinguished from an overridden local
	// Slorp entry: native will merge the remote plugin again on the final launch.
	data := []byte(runtimeEnv(r, "OPENCODE_AUTH_CONTENT"))
	if !json.Valid(data) {
		root := runtimeEnv(r, "XDG_DATA_HOME")
		if root == "" {
			root = filepath.Join(runtimeEnv(r, "HOME"), ".local", "share")
		}
		var err error
		data, err = os.ReadFile(filepath.Join(root, "opencode", "auth.json"))
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("checking remote OpenCode configuration: %w", err)
		}
	}
	var auth map[string]struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &auth); err != nil {
		return fmt.Errorf("checking remote OpenCode configuration: invalid authentication data")
	}
	for _, entry := range auth {
		if entry.Type == "wellknown" {
			return fmt.Errorf("managed OpenCode cannot safely replace a remote-origin Slorp plugin while well-known configuration is active; configure Slorp locally without a remote configuration source")
		}
	}
	return nil
}

func pluginSpec(entry any) string {
	if pair, ok := entry.([]any); ok && len(pair) > 0 {
		entry = pair[0]
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
	v := strings.ToLower(runtimeEnv(r, key))
	return v == "true" || v == "1"
}

// RuntimeFlags uses Effect's boolean codec, unlike the legacy Flag getters.
func runtimeEffectFlag(r *runtime, key string) bool {
	switch runtimeEnv(r, key) {
	case "true", "1", "yes", "on", "y":
		return true
	default:
		return false
	}
}
