// Package release tells which release of Slorp is running, and has the Go
// toolchain say whether a newer one is out and install it.
package release

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/jefflinse/slorp/internal/hub"
)

// Module is the Go module Slorp is released as.
const Module = "github.com/jefflinse/slorp"

const (
	// retry is how long after the toolchain could not say whether a newer
	// release is out it is asked again.
	retry = time.Hour
	// askTimeout bounds a question put to the toolchain.
	askTimeout = time.Minute
	// installTimeout bounds an install, which may fetch and compile every
	// dependency.
	installTimeout = 10 * time.Minute
)

// Installed returns the release this executable was installed as by
// `go install`, and empty for one built any other way.
func Installed() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Path != Module || info.Main.Sum == "" {
		return ""
	}
	return info.Main.Version
}

// Checker looks for releases of Slorp newer than the one running, and
// installs the newest in its place.
type Checker struct {
	// Version is the release running. With none, or with no Go, nothing is
	// looked for and nothing can be installed.
	Version string
	// Exe is the executable running, which an upgrade replaces.
	Exe string
	// Go is the Go command.
	Go string
	// Every is how often a newer release is looked for.
	Every time.Duration
	// Report is told the release running and the newer one when one is out.
	Report func(hub.Release)
	// Relaunch starts Slorp again as the executable then at Exe.
	Relaunch func()

	// upgrading is held while a release is being installed.
	upgrading sync.Mutex
}

// Run looks for a newer release now and every Every after, until ctx is done.
func (c *Checker) Run(ctx context.Context) {
	if c.Version == "" || c.Go == "" || c.Every <= 0 {
		return
	}
	for {
		wait := c.Every
		if _, err := c.check(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("release: %v", err)
			wait = retry
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// check asks the toolchain for a release newer than the one running, reports
// what it said, and returns the same.
func (c *Checker) check(ctx context.Context) (hub.Release, error) {
	out, err := c.run(ctx, askTimeout, "list", "-m", "-u", "-json", Module+"@"+c.Version)
	if err != nil {
		return hub.Release{}, err
	}
	var listed struct {
		Update *struct{ Version string }
	}
	if err := json.Unmarshal([]byte(out), &listed); err != nil {
		return hub.Release{}, err
	}
	r := hub.Release{Version: c.Version}
	if listed.Update != nil && listed.Update.Version != "" {
		r.Latest, r.Manual = listed.Update.Version, c.manual(ctx)
	}
	c.Report(r)
	return r, nil
}

// manual says why `go install` would not replace the executable running,
// and returns empty when it would.
func (c *Checker) manual(ctx context.Context) string {
	if runtime.GOOS == "windows" {
		return "Slorp cannot replace itself while it runs on Windows"
	}
	out, err := c.run(ctx, askTimeout, "env", "GOBIN", "GOPATH")
	if err != nil {
		return "Go could not say where it installs commands: " + err.Error()
	}
	bin, gopath, _ := strings.Cut(out, "\n")
	if bin = strings.TrimSpace(bin); bin == "" {
		first, _, _ := strings.Cut(strings.TrimSpace(gopath), string(os.PathListSeparator))
		bin = filepath.Join(first, "bin")
	}
	if installed, err := os.Stat(filepath.Join(bin, path.Base(Module))); err == nil {
		if running, err := os.Stat(c.Exe); err == nil && os.SameFile(installed, running) {
			return ""
		}
	}
	return fmt.Sprintf("Slorp runs from %s, which is not where Go installs commands (%s)", filepath.Dir(c.Exe), bin)
}

// Upgrade installs the newest release over the executable running. Slorp
// goes on running the old one until Restart. It fails when no newer release
// is out, or when `go install` would not replace the executable.
func (c *Checker) Upgrade(ctx context.Context) error {
	if c.Version == "" || c.Go == "" {
		return errors.New("Slorp was not installed with `go install`, or was started without Go, so it cannot upgrade itself")
	}
	if !c.upgrading.TryLock() {
		return errors.New("an upgrade is already under way")
	}
	defer c.upgrading.Unlock()
	r, err := c.check(ctx)
	if err != nil {
		return err
	}
	if r.Latest == "" {
		return fmt.Errorf("Slorp %s is the newest release", c.Version)
	}
	if r.Manual != "" {
		return errors.New(r.Manual)
	}
	_, err = c.run(ctx, installTimeout, "install", Module+"@"+r.Latest)
	return err
}

// Restart starts Slorp again, as the release Upgrade installed.
func (c *Checker) Restart() { c.Relaunch() }

// run has Go do something outside any module and returns what it printed,
// or as an error the last of what it complained of.
func (c *Checker) run(ctx context.Context, within time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Go, args...)
	cmd.Dir = os.TempDir()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		said := []rune(cmp.Or(strings.TrimSpace(stderr.String()), strings.TrimSpace(string(out))))
		if len(said) == 0 {
			return "", fmt.Errorf("go %s: %w", args[0], err)
		}
		return "", errors.New(string(said[max(0, len(said)-300):]))
	}
	return string(out), nil
}
