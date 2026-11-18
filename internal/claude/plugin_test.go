package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/hub"
	"github.com/jefflinse/slorp/internal/testdb"
)

// pluginClaude is a stand-in for Claude Code that keeps its plugin list in a
// file, which installing, enabling and uninstalling the Slorp plugin rewrite.
func pluginClaude(t *testing.T, listed string) (bin string, calls func() []string) {
	t.Helper()
	bin, calls = fakeClaude(t, `[]`, `
dir=$(dirname "$0")
case "$1 $2" in
"plugin list") cat "$dir/plugins" ;;
"plugin install") echo '[{"id":"other@slorp","enabled":true},{"id":"slorp@slorp","version":"0.1.4","enabled":true}]' > "$dir/plugins" ;;
"plugin enable") echo '[{"id":"slorp@slorp","version":"0.1.3","enabled":true}]' > "$dir/plugins" ;;
"plugin uninstall") echo '[{"id":"other@slorp","enabled":true}]' > "$dir/plugins" ;;
esac`)
	if err := os.WriteFile(filepath.Join(filepath.Dir(bin), "plugins"), []byte(listed), 0o600); err != nil {
		t.Fatal(err)
	}
	return bin, calls
}

// ran lists what Claude Code was asked to do, leaving out the directory it
// was asked from and its listing of plugins.
func ran(calls func() []string) string {
	var out []string
	for _, c := range calls() {
		if _, args, _ := strings.Cut(c, "|"); args != "" && args != "plugin|list|--json" {
			out = append(out, args)
		}
	}
	return strings.Join(out, "\n")
}

func TestInstall(t *testing.T) {
	off := `[{"id":"slorp@slorp","version":"0.1.3","enabled":false}]`
	tests := []struct {
		name, listed, want string
		now                hub.Plugin
	}{
		{"not installed", `[{"id":"slorp@elsewhere","enabled":true}]`, "plugin|install|slorp|--marketplace|https://github.com/jefflinse/slorp.git|--config|url=http://127.0.0.1:4747", hub.Plugin{Installed: true, Enabled: true, Version: "0.1.4"}},
		{"installed and turned off", off, "plugin|enable|slorp@slorp", hub.Plugin{Installed: true, Enabled: true, Version: "0.1.3"}},
		{"installed already", `[{"id":"slorp@slorp","version":"0.1.2","enabled":true}]`, "", hub.Plugin{Installed: true, Enabled: true, Version: "0.1.2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin, calls := pluginClaude(t, tt.listed)
			st := testdb.Open(t)
			h := hub.New(testdb.KB(t, st), st)
			a := &Adapter{Hub: h, Bin: bin, URL: "http://127.0.0.1:4747"}
			if err := a.Install(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := ran(calls); got != tt.want {
				t.Errorf("ran %q, want %q", got, tt.want)
			}
			if got := h.State().Plugins[Agent]; got != tt.now {
				t.Errorf("the hub was told %+v, want %+v", got, tt.now)
			}
		})
	}

	t.Run("no address to give it", func(t *testing.T) {
		bin, calls := pluginClaude(t, `[]`)
		st := testdb.Open(t)
		if err := (&Adapter{Hub: hub.New(testdb.KB(t, st), st), Bin: bin}).Install(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := ran(calls); got != "plugin|install|slorp|--marketplace|https://github.com/jefflinse/slorp.git" {
			t.Errorf("ran %q, want the plugin left to its own address", got)
		}
	})

	t.Run("Claude Code refuses", func(t *testing.T) {
		bin, _ := fakeClaude(t, `[]`, `[ "$2" = list ] && { echo '[]'; exit 0; }; echo "Failed to add marketplace: blocked by policy" >&2; exit 1`)
		st := testdb.Open(t)
		h := hub.New(testdb.KB(t, st), st)
		err := (&Adapter{Hub: h, Bin: bin}).Install(context.Background())
		if err == nil || !strings.Contains(err.Error(), "blocked by policy") {
			t.Errorf("err = %v, want what Claude Code said", err)
		}
		if _, told := h.State().Plugins[Agent]; told {
			t.Error("the hub was told of a plugin after an install that failed")
		}
	})
}

func TestUninstall(t *testing.T) {
	tests := []struct {
		name, listed, want string
	}{
		{"installed", `[{"id":"slorp@slorp","version":"0.1.4","enabled":true}]`, "plugin|uninstall|slorp@slorp"},
		{"installed and turned off", `[{"id":"slorp@slorp","version":"0.1.4","enabled":false}]`, "plugin|uninstall|slorp@slorp"},
		{"not installed", `[{"id":"slorp@elsewhere","enabled":true}]`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin, calls := pluginClaude(t, tt.listed)
			st := testdb.Open(t)
			h := hub.New(testdb.KB(t, st), st)
			if err := (&Adapter{Hub: h, Bin: bin}).Uninstall(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := ran(calls); got != tt.want {
				t.Errorf("ran %q, want %q", got, tt.want)
			}
			if got, told := h.State().Plugins[Agent]; !told || got != (hub.Plugin{}) {
				t.Errorf("the hub was told %+v (%v), want that the plugin is gone", got, told)
			}
		})
	}

	t.Run("Claude Code refuses", func(t *testing.T) {
		listed := `[{"id":"slorp@slorp","version":"0.1.4","enabled":true}]`
		bin, _ := fakeClaude(t, `[]`, `[ "$2" = list ] && { echo '`+listed+`'; exit 0; }; echo "Failed to uninstall: managed by policy" >&2; exit 1`)
		st := testdb.Open(t)
		h := hub.New(testdb.KB(t, st), st)
		err := (&Adapter{Hub: h, Bin: bin}).Uninstall(context.Background())
		if err == nil || !strings.Contains(err.Error(), "managed by policy") {
			t.Errorf("err = %v, want what Claude Code said", err)
		}
		if _, told := h.State().Plugins[Agent]; told {
			t.Error("the hub was told of a plugin after an uninstall that failed")
		}
	})

	t.Run("handed to sessions from a directory", func(t *testing.T) {
		bin, calls := pluginClaude(t, `[{"id":"slorp@slorp","version":"0.1.4","enabled":true}]`)
		st := testdb.Open(t)
		err := (&Adapter{Hub: hub.New(testdb.KB(t, st), st), Bin: bin, Plugin: t.TempDir()}).Uninstall(context.Background())
		if err == nil || ran(calls) != "" {
			t.Errorf("err = %v after %q, want it refused and nothing run", err, ran(calls))
		}
	})
}

func TestCheckPlugin(t *testing.T) {
	bin, calls := pluginClaude(t, `[{"id":"slorp@slorp","version":"0.1.3","enabled":false}]`)
	st := testdb.Open(t)
	h := hub.New(testdb.KB(t, st), st)
	a := &Adapter{Hub: h, Bin: bin}
	a.checkPlugin(context.Background())
	a.checkPlugin(context.Background())
	if got := h.State().Plugins[Agent]; got != (hub.Plugin{Installed: true, Version: "0.1.3"}) || len(calls()) != 1 {
		t.Errorf("the hub was told %+v after %d questions; want the plugin as listed, asked for once", got, len(calls()))
	}
	a.pluginAt = time.Now().Add(-2 * pluginEvery)
	a.checkPlugin(context.Background())
	if len(calls()) != 2 {
		t.Errorf("Claude Code was asked %d times, want it asked again once the last answer is old", len(calls()))
	}

	// Sessions handed the plugin from a directory have it without Claude Code installing it.
	h = hub.New(testdb.KB(t, st), st)
	a = &Adapter{Hub: h, Bin: filepath.Join(t.TempDir(), "no-claude"), Plugin: "/plugin"}
	a.checkPlugin(context.Background())
	if got := h.State().Plugins[Agent]; got != (hub.Plugin{Installed: true, Enabled: true}) {
		t.Errorf("with a plugin directory the hub was told %+v", got)
	}

	// An answer that cannot be had says nothing either way.
	h = hub.New(testdb.KB(t, st), st)
	(&Adapter{Hub: h, Bin: filepath.Join(t.TempDir(), "no-claude")}).checkPlugin(context.Background())
	bin, _ = fakeClaude(t, `[]`, `echo 'not json'`)
	(&Adapter{Hub: h, Bin: bin}).checkPlugin(context.Background())
	if got := h.State().Plugins; len(got) != 0 {
		t.Errorf("the hub was told %+v by a Claude Code that could not say", got)
	}
}
