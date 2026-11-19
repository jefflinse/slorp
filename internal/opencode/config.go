package opencode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// jsonComments removes JSONC comments without moving offsets.
func jsonComments(b []byte) []byte {
	out := bytes.Clone(b)
	quoted, escaped := false, false
	for i := 0; i < len(out); i++ {
		c := out[i]
		if quoted {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				quoted = false
			}
			continue
		}
		if c == '"' {
			quoted = true
			continue
		}
		if c != '/' || i+1 == len(out) {
			continue
		}
		switch out[i+1] {
		case '/':
			for ; i < len(out) && out[i] != '\n'; i++ {
				out[i] = ' '
			}
		case '*':
			out[i], out[i+1] = ' ', ' '
			i += 2
			for ; i < len(out); i++ {
				if out[i] == '*' && i+1 < len(out) && out[i+1] == '/' {
					out[i], out[i+1] = ' ', ' '
					i++
					break
				}
				if out[i] != '\n' && out[i] != '\r' {
					out[i] = ' '
				}
			}
		}
	}
	return out
}

func jsonConfig(b []byte) []byte {
	out := jsonComments(b)
	quoted, escaped := false, false
	for i, c := range out {
		if quoted {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				quoted = false
			}
			continue
		}
		if c == '"' {
			quoted = true
			continue
		}
		if c == ',' {
			j := i + 1
			for j < len(out) && bytes.ContainsRune([]byte(" \r\n\t"), rune(out[j])) {
				j++
			}
			if j < len(out) && (out[j] == ']' || out[j] == '}') {
				out[i] = ' '
			}
		}
	}
	return out
}

type configEntry struct {
	spec       string
	start, end int
}

func entries(b []byte) ([]configEntry, error) {
	clean := jsonConfig(b)
	if !json.Valid(clean) {
		return nil, fmt.Errorf("invalid OpenCode configuration")
	}
	d := json.NewDecoder(bytes.NewReader(clean))
	token, _ := d.Token()
	if token != json.Delim('{') {
		return nil, fmt.Errorf("OpenCode configuration must be an object")
	}
	var out []configEntry
	for d.More() {
		key, _ := d.Token()
		var raw json.RawMessage
		if err := d.Decode(&raw); err != nil {
			return nil, err
		}
		if key != "plugin" {
			continue
		}
		base := int(d.InputOffset()) - len(raw)
		a := json.NewDecoder(bytes.NewReader(raw))
		if token, _ := a.Token(); token != json.Delim('[') {
			return nil, fmt.Errorf("plugin must be an array")
		}
		for a.More() {
			var value json.RawMessage
			if err := a.Decode(&value); err != nil {
				return nil, err
			}
			end := base + int(a.InputOffset())
			var spec string
			if json.Unmarshal(value, &spec) != nil {
				var pair []json.RawMessage
				if json.Unmarshal(value, &pair) == nil && len(pair) > 0 {
					_ = json.Unmarshal(pair[0], &spec)
				}
			}
			out = append(out, configEntry{spec, end - len(value), end})
		}
	}
	return out, nil
}

func localPath(spec, config string) string {
	if strings.HasPrefix(spec, "file:") {
		u, err := url.Parse(spec)
		if err != nil || (u.Host != "" && u.Host != "localhost") {
			return ""
		}
		spec = u.Path
	}
	if !filepath.IsAbs(spec) {
		if !strings.HasPrefix(spec, ".") {
			return ""
		}
		spec = filepath.Join(filepath.Dir(config), spec)
	}
	return filepath.Clean(spec)
}

func samePath(a, b string) bool {
	if x, err := filepath.EvalSymlinks(a); err == nil {
		a = x
	}
	if x, err := filepath.EvalSymlinks(b); err == nil {
		b = x
	}
	return a != "" && b != "" && filepath.Clean(a) == filepath.Clean(b)
}

// removeReference removes only matching plugin entries, keeping other config text.
func removeReference(path, target string) error {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	all, err := entries(b)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	clean := jsonComments(b)
	changed := false
	for _, e := range all {
		if !samePath(localPath(e.spec, path), target) {
			continue
		}
		changed = true
		for i := e.start; i < e.end; i++ {
			if b[i] != '\n' && b[i] != '\r' {
				b[i] = ' '
			}
			clean[i] = ' '
		}
		j := e.end
		for j < len(clean) && bytes.ContainsRune([]byte(" \r\n\t"), rune(clean[j])) {
			j++
		}
		if j < len(clean) && clean[j] == ',' {
			b[j], clean[j] = ' ', ' '
			continue
		}
		j = e.start - 1
		for j >= 0 && bytes.ContainsRune([]byte(" \r\n\t"), rune(clean[j])) {
			j--
		}
		if j >= 0 && clean[j] == ',' {
			b[j], clean[j] = ' ', ' '
		}
	}
	if !changed {
		return nil
	}
	if !json.Valid(jsonConfig(b)) {
		return fmt.Errorf("could not remove plugin from %s", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if target, err := filepath.EvalSymlinks(path); err == nil {
		path = target
	}
	return atomicFile(path, b, info.Mode().Perm())
}

func atomicFile(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".slorp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	closed := f.Close()
	if err != nil {
		return err
	}
	if closed != nil {
		return closed
	}
	return os.Rename(f.Name(), path)
}
