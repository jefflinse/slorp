// Package terminal opens windows of the terminal emulators on this machine.
package terminal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// App is a terminal emulator Slorp knows how to open a window of.
type App struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// preset is how one terminal emulator is found and opened on macOS.
type preset struct {
	App
	// bundle is the application's name in an Applications folder.
	bundle string
	// runs reports whether the environment is that of a shell it runs.
	runs func(env func(string) string) bool
	// argv is the command that opens a new window of it running a shell
	// command, given where the application is and the user's shell.
	argv func(app, shell, command string) []string
}

// script is the osascript command that runs lines as the body of a handler
// whose argv holds the shell command.
func script(lines ...string) func(app, shell, command string) []string {
	return func(_, _, command string) []string {
		args := []string{"osascript", "-e", "on run argv"}
		for _, l := range lines {
			args = append(args, "-e", l)
		}
		return append(args, "-e", "end run", command)
	}
}

// launched is the command that starts a new instance of the application
// with flags, running the shell command in the user's shell.
func launched(flags ...string) func(app, shell, command string) []string {
	return func(app, shell, command string) []string {
		return append(append([]string{"open", "-na", app, "--args"}, flags...), inShell(shell, command)...)
	}
}

// inShell is the user's shell running a command as it would at a prompt, and
// staying open once the command ends.
func inShell(shell, command string) []string {
	return []string{shell, "-lic", command + "; exec " + quote(shell) + " -l"}
}

func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func termProgram(name string) func(func(string) string) bool {
	return func(env func(string) string) bool { return env("TERM_PROGRAM") == name }
}

func set(name string) func(func(string) string) bool {
	return func(env func(string) string) bool { return env(name) != "" }
}

var presets = map[string][]preset{
	"darwin": {
		{App{"iterm", "iTerm2"}, "iTerm", termProgram("iTerm.app"), script(
			`tell application "iTerm"`,
			`set w to (create window with default profile)`,
			`tell current session of w to write text (item 1 of argv)`,
			`activate`,
			`end tell`)},
		{App{"ghostty", "Ghostty"}, "Ghostty", termProgram("ghostty"), launched("-e")},
		{App{"kitty", "kitty"}, "kitty", set("KITTY_WINDOW_ID"), launched()},
		{App{"terminal", "Terminal"}, "Terminal", termProgram("Apple_Terminal"), script(
			`tell application "Terminal"`,
			`do script (item 1 of argv)`,
			`activate`,
			`end tell`)},
	},
}

// Opener opens terminal windows on the machine Slorp runs on.
type Opener struct {
	// OS is the system, as runtime.GOOS names it.
	OS string
	// Env reads the environment Slorp was started in, and Exists whether a
	// path is there.
	Env    func(string) string
	Exists func(path string) bool
	// Run runs a command and returns what it printed.
	Run func(ctx context.Context, argv []string) ([]byte, error)
}

// New returns an Opener for this machine.
func New() *Opener {
	return &Opener{
		OS:  runtime.GOOS,
		Env: os.Getenv,
		Exists: func(path string) bool {
			_, err := os.Stat(path)
			return err == nil
		},
		Run: func(ctx context.Context, argv []string) ([]byte, error) {
			return exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
		},
	}
}

// found is a preset whose application is installed, and where.
type found struct {
	preset
	path string
}

func (o *Opener) installed() []found {
	var out []found
	for _, p := range presets[o.OS] {
		for _, dir := range []string{"/Applications", "/System/Applications/Utilities", filepath.Join(o.Env("HOME"), "Applications")} {
			if path := filepath.Join(dir, p.bundle+".app"); o.Exists(path) {
				out = append(out, found{p, path})
				break
			}
		}
	}
	return out
}

// Apps lists the terminal emulators installed here that Slorp can open a
// window of. There are none on a system it has no presets for.
func (o *Opener) Apps() []App {
	out := []App{}
	for _, p := range o.installed() {
		out = append(out, p.App)
	}
	return out
}

// Default names the one of Apps to use when the user has chosen none: the
// one Slorp was started from, or else the first. It is empty when there are
// none.
func (o *Opener) Default() string {
	here := o.installed()
	for _, p := range here {
		if p.runs(o.Env) {
			return p.ID
		}
	}
	if len(here) > 0 {
		return here[0].ID
	}
	return ""
}

// ErrUnknown is returned for a terminal emulator that is not one of Apps.
var ErrUnknown = errors.New("no such terminal emulator is installed")

// Open opens a new window of the terminal emulator named app, one of Apps,
// and runs a shell command in it.
func (o *Opener) Open(ctx context.Context, app, command string) error {
	for _, p := range o.installed() {
		if p.ID != app {
			continue
		}
		shell := o.Env("SHELL")
		if shell == "" {
			shell = "/bin/zsh"
		}
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		out, err := o.Run(ctx, p.argv(p.path, shell, command))
		if err == nil {
			return nil
		}
		said := strings.TrimSpace(string(out))
		if strings.Contains(said, "-1743") {
			return fmt.Errorf("macOS has not allowed Slorp to control %s; allow it under System Settings, Privacy & Security, Automation", p.Name)
		}
		if said == "" {
			said = err.Error()
		}
		return fmt.Errorf("opening %s: %s", p.Name, said)
	}
	return ErrUnknown
}
