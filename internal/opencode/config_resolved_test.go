package opencode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestV2ConfigSourceMerge(t *testing.T) {
	decode := func(text string) map[string]any {
		t.Helper()
		var value map[string]any
		if err := json.Unmarshal([]byte(text), &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	global := decode(`{
		"policies":[{"effect":"deny"}],"permissions":[{"effect":"ask"}],
		"instructions":["global"],"skills":["global"],"plugins":["global"],
		"commands":{"review":{"template":"global","agent":"old"}},
		"references":{"docs":{"path":"global","branch":"old"}},
		"mcp":{"servers":{"test":{"type":"local","command":["old"],"environment":{"OLD":"1"}}}},
		"providers":{"mock":{"settings":{"baseURL":"old","keep":true},"models":{"test":{"variants":[{"id":"low"}]}}}}
	}`)
	project := decode(`{
		"policies":[{"effect":"allow"}],"permissions":[{"effect":"allow"}],
		"instructions":["project"],"skills":["project"],"plugins":["project"],
		"commands":{"review":{"template":"project"}},
		"references":{"docs":{"path":"project"}},
		"mcp":{"servers":{"test":{"type":"remote","url":"https://example.invalid/mcp"}}},
		"providers":{"mock":{"settings":{"baseURL":"new"},"models":{"test":{"variants":[{"id":"high"}]}}}}
	}`)
	got := map[string]any{}
	mergeConfigDocument(got, global)
	mergeConfigDocument(got, project)
	want := decode(`{
		"policies":[{"effect":"allow"},{"effect":"deny"}],
		"permissions":[{"effect":"ask"},{"effect":"allow"}],
		"instructions":["global","project"],"skills":["global","project"],"plugins":["global","project"],
		"commands":{"review":{"template":"project"}},
		"references":{"docs":{"path":"project"}},
		"mcp":{"servers":{"test":{"type":"remote","url":"https://example.invalid/mcp"}}},
		"providers":{"mock":{"settings":{"baseURL":"new","keep":true},"models":{"test":{"variants":[{"id":"low"},{"id":"high"}]}}}}
	}`)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merged sources = %#v, want %#v", got, want)
	}
}

func TestV2PrivateCommandSourceOrder(t *testing.T) {
	root := t.TempDir()
	put(t, filepath.Join(root, "commands", "review.md"), "directory wins")
	linked := t.TempDir()
	put(t, filepath.Join(linked, "command.md"), "linked directory command")
	if err := os.Symlink(linked, filepath.Join(root, "commands", "nested")); err != nil {
		t.Fatal(err)
	}
	r := &runtime{private: t.TempDir()}
	config := map[string]any{
		configCommandsKey: []any{
			map[string]any{"commands": map[string]any{"review": map[string]any{"template": "global"}}},
			map[string]any{"path": root},
			map[string]any{"commands": map[string]any{"last": map[string]any{"template": "inline literal {env:SECRET}"}}},
		},
	}
	if err := r.preserveCommands(config); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"review": "directory wins", "last": "inline literal {env:SECRET}", "nested/command": "linked directory command"} {
		data, err := os.ReadFile(filepath.Join(r.private, "commands", name+".md"))
		if err != nil || !strings.Contains(string(data), want) {
			t.Fatalf("%s: %q, %v", name, data, err)
		}
	}
	if _, exists := config[configCommandsKey]; exists {
		t.Fatal("internal source metadata leaked into native configuration")
	}
	config[configCommandsKey] = []any{map[string]any{"commands": map[string]any{"../escape": map[string]any{"template": "unsafe"}}}}
	if err := r.preserveCommands(config); err == nil {
		t.Fatal("command path escaped private command directory")
	}
}
