package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/hub"
	"github.com/jefflinse/slorp/internal/testdb"
)

// pluginClaude is a stand-in for Claude Code that keeps its plugin list in a
// file, which installing, enabling, updating and uninstalling the Slorp
// plugin rewrite. Its one marketplace is the directory "market" beside it,
// which lists nothing until offer is called.
func pluginClaude(t *testing.T, listed string) (bin string, calls func() []string) {
	t.Helper()
	bin, calls = fakeClaude(t, `[]`, `
dir=$(dirname "$0")
case "$1 $2" in
"plugin list") cat "$dir/plugins" ;;
"plugin install") echo '[{"id":"other@slorp","enabled":true},{"id":"slorp@slorp","version":"0.1.4","enabled":true}]' > "$dir/plugins" ;;
"plugin enable") echo '[{"id":"slorp@slorp","version":"0.1.3","enabled":true}]' > "$dir/plugins" ;;
"plugin update") echo '[{"id":"slorp@slorp","version":"0.1.5","enabled":true}]' > "$dir/plugins" ;;
"plugin marketplace") if [ "$3" = list ]; then echo '[{"name":"other","installLocation":"/nowhere"},{"name":"slorp","installLocation":"'"$dir"'/market"}]'; fi ;;
"plugin uninstall") echo '[{"id":"other@slorp","enabled":true}]' > "$dir/plugins" ;;
esac`)
	if err := os.WriteFile(filepath.Join(filepath.Dir(bin), "plugins"), []byte(listed), 0o600); err != nil {
		t.Fatal(err)
	}
	return bin, calls
}

// offer has the marketplace beside bin list the Slorp plugin at version.
func offer(t *testing.T, bin, version string) {
	t.Helper()
	dir := filepath.Join(filepath.Dir(bin), "market", ".claude-plugin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	listing := `{"name":"slorp","plugins":[{"name":"other","version":"9.0.0"},{"name":"slorp","source":"./integrations/claude-code","version":"` + version + `"}]}`
	if err := os.WriteFile(filepath.Join(dir, "marketplace.json"), []byte(listing), 0o600); err != nil {
		t.Fatal(err)
	}
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

func TestNewer(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"0.1.5", "0.1.4", true},
		{"0.1.4", "0.1.5", false},
		{"0.1.5", "0.1.5", false},
		{"0.1.10", "0.1.9", true},
		{"0.2.0", "0.1.9", true},
		{"1.0", "0.9.9", true},
		{"0.1.5.1", "0.1.5", true},
		{"0.1", "0.1.0", false},
		{"", "0.1.5", false},
		{"0.1.5", "", false},
		{"0.2.0-beta", "0.1.5", false},
	}
	for _, tt := range tests {
		if got := newer(tt.a, tt.b); got != tt.want {
			t.Errorf("newer(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestCheckOffer(t *testing.T) {
	const fetched = "plugin|marketplace|update|slorp\nplugin|marketplace|list|--json"
	tests := []struct {
		name, offered string
		want          hub.Plugin
	}{
		{"a newer version", "0.1.5", hub.Plugin{Installed: true, Enabled: true, Version: "0.1.3", Latest: "0.1.5"}},
		{"the version installed", "0.1.3", hub.Plugin{Installed: true, Enabled: true, Version: "0.1.3"}},
		{"an older version", "0.1.2", hub.Plugin{Installed: true, Enabled: true, Version: "0.1.3"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin, calls := pluginClaude(t, `[{"id":"slorp@slorp","version":"0.1.3","enabled":true}]`)
			offer(t, bin, tt.offered)
			st := testdb.Open(t)
			h := hub.New(testdb.KB(t, st), st)
			a := &Adapter{Hub: h, Bin: bin, Updates: time.Hour}
			if err := a.checkOffer(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := ran(calls); got != fetched {
				t.Errorf("ran %q, want the marketplace fetched and then read", got)
			}
			if got := h.State().Plugins[Agent]; got != tt.want {
				t.Errorf("the hub was told %+v, want %+v", got, tt.want)
			}
			if wait := time.Until(a.offer.again); wait < 59*time.Minute || wait > time.Hour {
				t.Errorf("the marketplace is next asked in %v, want Updates from now", wait)
			}
		})
	}

	t.Run("a marketplace without the plugin", func(t *testing.T) {
		bin, _ := pluginClaude(t, `[{"id":"slorp@slorp","version":"0.1.3","enabled":true}]`)
		st := testdb.Open(t)
		h := hub.New(testdb.KB(t, st), st)
		a := &Adapter{Hub: h, Bin: bin, Updates: time.Hour}
		if err := a.checkOffer(context.Background()); err == nil {
			t.Error("no error from a marketplace that could not be read")
		}
		if _, told := h.State().Plugins[Agent]; told || !a.offer.again.IsZero() {
			t.Errorf("the hub was told (%v) or the next asking put off to %v by an answer that could not be had", told, a.offer.again)
		}
	})

	t.Run("Claude Code refuses", func(t *testing.T) {
		bin, _ := fakeClaude(t, `[]`, `echo "Failed to update marketplace: offline" >&2; exit 1`)
		st := testdb.Open(t)
		err := (&Adapter{Hub: hub.New(testdb.KB(t, st), st), Bin: bin}).checkOffer(context.Background())
		if err == nil || !strings.Contains(err.Error(), "offline") {
			t.Errorf("err = %v, want what Claude Code said", err)
		}
	})
}

// asked counts the times Claude Code was told to fetch the marketplace.
func asked(calls func() []string) (n int) {
	for _, c := range calls() {
		if strings.HasSuffix(c, "|plugin|marketplace|update|slorp") {
			n++
		}
	}
	return n
}

// eventually waits for what checkPlugin started on the side.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("never happened: %s", what)
		}
	}
}

func TestCheckPluginLooksForANewerVersion(t *testing.T) {
	bin, calls := pluginClaude(t, `[{"id":"slorp@slorp","version":"0.1.3","enabled":true}]`)
	offer(t, bin, "0.1.5")
	st := testdb.Open(t)
	h := hub.New(testdb.KB(t, st), st)
	a := &Adapter{Hub: h, Bin: bin, Updates: 24 * time.Hour}
	again := func() {
		a.pluginAt = time.Time{}
		a.checkPlugin(context.Background())
	}
	due := func() time.Time {
		a.offer.Lock()
		defer a.offer.Unlock()
		return a.offer.again
	}

	again()
	eventually(t, "the hub told of the newer version", func() bool { return h.State().Plugins[Agent].Latest == "0.1.5" })
	eventually(t, "the next asking put off by Updates", func() bool { return time.Until(due()) > 23*time.Hour })
	again()
	if n := asked(calls); n != 1 {
		t.Errorf("the marketplace was fetched %d times, want once until Updates has passed", n)
	}

	a.offer.Lock()
	a.offer.again = time.Now().Add(-time.Second)
	a.offer.Unlock()
	offer(t, bin, "0.1.6")
	again()
	eventually(t, "the hub told of the version after", func() bool { return h.State().Plugins[Agent].Latest == "0.1.6" })
	if n := asked(calls); n != 2 {
		t.Errorf("the marketplace was fetched %d times, want once more when due", n)
	}

	// Nothing is asked of the marketplace of a plugin that is not installed,
	// or one handed to sessions from a directory.
	bin, calls = pluginClaude(t, `[]`)
	a = &Adapter{Hub: hub.New(testdb.KB(t, st), st), Bin: bin, Updates: time.Hour}
	a.checkPlugin(context.Background())
	if !a.offer.again.IsZero() || asked(calls) != 0 {
		t.Error("the marketplace was asked about a plugin that is not installed")
	}
	a = &Adapter{Hub: hub.New(testdb.KB(t, st), st), Bin: bin, Updates: time.Hour, Plugin: "/plugin"}
	a.checkPlugin(context.Background())
	if !a.offer.again.IsZero() || asked(calls) != 0 {
		t.Error("the marketplace was asked about a plugin handed over from a directory")
	}
}

// TestThisMarketplaceOffersThisPlugin reads this repository's marketplace as
// an install of 0.1.5 would: it offers the plugin as its manifest has it.
func TestThisMarketplaceOffersThisPlugin(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "integrations", "claude-code", ".claude-plugin", "plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct{ Version string }
	if err := json.Unmarshal(b, &manifest); err != nil {
		t.Fatal(err)
	}
	listing, err := os.ReadFile(filepath.Join("..", "..", ".claude-plugin", "marketplace.json"))
	if err != nil {
		t.Fatal(err)
	}
	bin, _ := pluginClaude(t, `[{"id":"slorp@slorp","version":"0.1.5","enabled":true}]`)
	market := filepath.Join(filepath.Dir(bin), "market", ".claude-plugin")
	if err := os.MkdirAll(market, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(market, "marketplace.json"), listing, 0o600); err != nil {
		t.Fatal(err)
	}
	st := testdb.Open(t)
	h := hub.New(testdb.KB(t, st), st)
	(&Adapter{Hub: h, Bin: bin, Updates: time.Hour}).checkPlugin(context.Background())
	eventually(t, "an install of 0.1.5 offered "+manifest.Version, func() bool { return h.State().Plugins[Agent].Latest == manifest.Version })
}

func TestCheckPluginAsksAgainSoonerAfterAFailure(t *testing.T) {
	bin, calls := pluginClaude(t, `[{"id":"slorp@slorp","version":"0.1.3","enabled":true}]`)
	st := testdb.Open(t)
	a := &Adapter{Hub: hub.New(testdb.KB(t, st), st), Bin: bin, Updates: 24 * time.Hour}
	a.checkPlugin(context.Background())
	eventually(t, "the marketplace fetched", func() bool { return asked(calls) == 1 })
	a.offer.Lock()
	wait := time.Until(a.offer.again)
	a.offer.Unlock()
	if wait < offerRetry-time.Minute || wait > offerRetry {
		t.Errorf("the marketplace is next asked in %v, want offerRetry after it could not say", wait)
	}
}

func TestUpdate(t *testing.T) {
	listed := `[{"id":"slorp@slorp","version":"0.1.3","enabled":true}]`
	t.Run("a newer version", func(t *testing.T) {
		bin, calls := pluginClaude(t, listed)
		offer(t, bin, "0.1.5")
		st := testdb.Open(t)
		h := hub.New(testdb.KB(t, st), st)
		a := &Adapter{Hub: h, Bin: bin}
		if err := a.checkOffer(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := a.Update(context.Background()); err != nil {
			t.Fatal(err)
		}
		want := "plugin|marketplace|update|slorp\nplugin|marketplace|list|--json"
		if got := ran(calls); got != want+"\n"+want+"\nplugin|update|slorp@slorp" {
			t.Errorf("ran %q, want the marketplace fetched before the update", got)
		}
		if got := h.State().Plugins[Agent]; got != (hub.Plugin{Installed: true, Enabled: true, Version: "0.1.5"}) {
			t.Errorf("the hub was told %+v, want the new version with none newer", got)
		}
	})

	t.Run("Claude Code refuses", func(t *testing.T) {
		bin, _ := fakeClaude(t, `[]`, `[ "$2" = list ] && { echo '`+listed+`'; exit 0; }; echo "Failed to update: managed by policy" >&2; exit 1`)
		st := testdb.Open(t)
		h := hub.New(testdb.KB(t, st), st)
		err := (&Adapter{Hub: h, Bin: bin}).Update(context.Background())
		if err == nil || !strings.Contains(err.Error(), "managed by policy") {
			t.Errorf("err = %v, want what Claude Code said", err)
		}
		if _, told := h.State().Plugins[Agent]; told {
			t.Error("the hub was told of a plugin after an update that failed")
		}
	})

	t.Run("handed to sessions from a directory", func(t *testing.T) {
		bin, calls := pluginClaude(t, listed)
		st := testdb.Open(t)
		err := (&Adapter{Hub: hub.New(testdb.KB(t, st), st), Bin: bin, Plugin: t.TempDir()}).Update(context.Background())
		if err == nil || ran(calls) != "" {
			t.Errorf("err = %v after %q, want it refused and nothing run", err, ran(calls))
		}
	})
}
