package release

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/hub"
)

// world is a stand-in for the Go command beside the directory it installs
// commands into, where a Slorp already is. The command says no newer
// release is out until offer is called, and records what it was asked.
type world struct {
	t   *testing.T
	dir string
	// reported is what the checker told its hub, the latest last.
	reported []hub.Release
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, dir: t.TempDir()}
	script := `#!/bin/sh
dir=$(dirname "$0")
{ printf '%s' "$1"; shift; for a in "$@"; do printf '|%s' "$a"; done; echo; } >> "$dir/calls"
case "$(tail -n 1 "$dir/calls")" in
list*) cat "$dir/listed" ;;
env*) cat "$dir/gobin"; echo; echo "$dir/gopath:/elsewhere" ;;
install*) if [ -f "$dir/broken" ]; then cat "$dir/broken" >&2; exit 1; fi; echo upgraded > "$dir/bin/slorp" ;;
esac`
	w.write("go", script, 0o755)
	w.write("listed", `{"Path":"github.com/jefflinse/slorp","Version":"v0.2.0"}`, 0o600)
	w.write("gobin", filepath.Join(w.dir, "bin"), 0o600)
	w.write("bin/slorp", "running", 0o755)
	return w
}

func (w *world) write(name, body string, perm os.FileMode) {
	w.t.Helper()
	path := filepath.Join(w.dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), perm); err != nil {
		w.t.Fatal(err)
	}
}

// offer has the Go command say version is out.
func (w *world) offer(version string) {
	w.write("listed", `{"Path":"github.com/jefflinse/slorp","Version":"v0.2.0","Update":{"Path":"github.com/jefflinse/slorp","Version":"`+version+`"}}`, 0o600)
}

// checker returns a checker for the Slorp in the directory commands are
// installed into, running as v0.2.0.
func (w *world) checker() *Checker {
	return &Checker{Version: "v0.2.0", Exe: filepath.Join(w.dir, "bin", "slorp"), Go: filepath.Join(w.dir, "go"), Every: time.Hour,
		Report: func(r hub.Release) { w.reported = append(w.reported, r) }}
}

// ran lists what the Go command was asked to do.
func (w *world) ran() string {
	b, _ := os.ReadFile(filepath.Join(w.dir, "calls"))
	return strings.TrimSpace(string(b))
}

// installed is what the executable in the directory commands are installed
// into holds.
func (w *world) installed() string {
	b, _ := os.ReadFile(filepath.Join(w.dir, "bin", "slorp"))
	return strings.TrimSpace(string(b))
}

func TestCheckReportsANewerRelease(t *testing.T) {
	w := newWorld(t)
	c := w.checker()
	if _, err := c.check(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, want := w.reported, (hub.Release{Version: "v0.2.0"}); len(got) != 1 || got[0] != want {
		t.Errorf("reported %+v with nothing newer out, want %+v", got, want)
	}
	if got, want := w.ran(), "list|-m|-u|-json|github.com/jefflinse/slorp@v0.2.0"; got != want {
		t.Errorf("go was asked\n%s\nwant\n%s", got, want)
	}

	w.offer("v0.3.0")
	if _, err := c.check(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, want := w.reported[len(w.reported)-1], (hub.Release{Version: "v0.2.0", Latest: "v0.3.0"}); got != want {
		t.Errorf("reported %+v, want %+v", got, want)
	}
}

func TestCheckSaysWhyGoWouldNotReplaceTheExecutable(t *testing.T) {
	w := newWorld(t)
	w.offer("v0.3.0")
	w.write("copied/slorp", "running", 0o755)
	c := w.checker()
	c.Exe = filepath.Join(w.dir, "copied", "slorp")
	if _, err := c.check(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := w.reported[len(w.reported)-1]
	if got.Latest != "v0.3.0" || !strings.Contains(got.Manual, filepath.Join(w.dir, "copied")) || !strings.Contains(got.Manual, filepath.Join(w.dir, "bin")) {
		t.Errorf("reported %+v, want v0.3.0 with where Slorp runs from and where Go installs commands", got)
	}
}

func TestCheckFindsCommandsUnderTheFirstGopathWithNoGobin(t *testing.T) {
	w := newWorld(t)
	w.offer("v0.3.0")
	w.write("gobin", "", 0o600)
	c := w.checker()
	if _, err := c.check(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := w.reported[len(w.reported)-1]; !strings.Contains(got.Manual, filepath.Join(w.dir, "gopath", "bin")) {
		t.Errorf("reported %+v with Slorp outside GOPATH's bin, want that named as where Go installs commands", got)
	}
	w.write("gopath/bin/slorp", "running", 0o755)
	c.Exe = filepath.Join(w.dir, "gopath", "bin", "slorp")
	if _, err := c.check(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, want := w.reported[len(w.reported)-1], (hub.Release{Version: "v0.2.0", Latest: "v0.3.0"}); got != want {
		t.Errorf("reported %+v with Slorp in GOPATH's bin, want %+v", got, want)
	}
}

func TestUpgradeInstallsTheNewestRelease(t *testing.T) {
	w := newWorld(t)
	w.offer("v0.3.0")
	if err := w.checker().Upgrade(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := w.installed(); got != "upgraded" {
		t.Errorf("the executable holds %q, want it replaced", got)
	}
	if got, want := w.ran(), "list|-m|-u|-json|github.com/jefflinse/slorp@v0.2.0\nenv|GOBIN|GOPATH\ninstall|github.com/jefflinse/slorp@v0.3.0"; got != want {
		t.Errorf("go was asked\n%s\nwant\n%s", got, want)
	}
}

func TestUpgradeInstallsNothingItShouldNot(t *testing.T) {
	t.Run("nothing newer", func(t *testing.T) {
		w := newWorld(t)
		if err := w.checker().Upgrade(t.Context()); err == nil || !strings.Contains(err.Error(), "v0.2.0 is the newest") {
			t.Errorf("upgrade = %v, want it to say the release running is the newest", err)
		}
		if strings.Contains(w.ran(), "install") {
			t.Errorf("go was asked\n%s\nwant nothing installed", w.ran())
		}
	})
	t.Run("running from elsewhere", func(t *testing.T) {
		w := newWorld(t)
		w.offer("v0.3.0")
		w.write("copied/slorp", "running", 0o755)
		c := w.checker()
		c.Exe = filepath.Join(w.dir, "copied", "slorp")
		if err := c.Upgrade(t.Context()); err == nil || !strings.Contains(err.Error(), "not where Go installs commands") {
			t.Errorf("upgrade = %v, want it to say Go would not replace the executable", err)
		}
		if strings.Contains(w.ran(), "install") || w.installed() != "running" {
			t.Errorf("go was asked\n%s\nwant nothing installed", w.ran())
		}
	})
	t.Run("not a release", func(t *testing.T) {
		w := newWorld(t)
		w.offer("v0.3.0")
		c := w.checker()
		c.Version = ""
		if err := c.Upgrade(t.Context()); err == nil || w.ran() != "" {
			t.Errorf("upgrade = %v after go was asked %q, want an error and Go never asked", err, w.ran())
		}
	})
}

func TestUpgradeSaysWhatGoComplainedOf(t *testing.T) {
	w := newWorld(t)
	w.offer("v0.3.0")
	w.write("broken", "build failed: no space left on device", 0o600)
	if err := w.checker().Upgrade(t.Context()); err == nil || err.Error() != "build failed: no space left on device" {
		t.Errorf("upgrade = %v, want what Go complained of", err)
	}
}

func TestRunLooksForANewerRelease(t *testing.T) {
	w := newWorld(t)
	w.offer("v0.3.0")
	ctx, cancel := context.WithCancel(t.Context())
	c := w.checker()
	c.Report = func(r hub.Release) { w.reported = append(w.reported, r); cancel() }
	c.Run(ctx)
	if got, want := w.reported, (hub.Release{Version: "v0.2.0", Latest: "v0.3.0"}); len(got) != 1 || got[0] != want {
		t.Errorf("reported %+v, want %+v once", got, want)
	}
}

func TestRunLooksForNothingWithoutAReleaseOrGo(t *testing.T) {
	for name, set := range map[string]func(*Checker){
		"not a release": func(c *Checker) { c.Version = "" },
		"no go":         func(c *Checker) { c.Go = "" },
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			c := w.checker()
			go_ := c.Go
			set(c)
			c.Run(t.Context())
			if len(w.reported) != 0 || w.ran() != "" {
				t.Errorf("reported %+v after %s was asked %q, want nothing looked for", w.reported, go_, w.ran())
			}
		})
	}
}
