package terminal

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// machine is an Opener for a made-up Mac with the named applications
// installed, which records the commands it is asked to run.
func machine(env map[string]string, apps ...string) (*Opener, *[][]string) {
	ran := &[][]string{}
	return &Opener{
		OS:     "darwin",
		Env:    func(name string) string { return env[name] },
		Exists: func(path string) bool { return slices.Contains(apps, path) },
		Run: func(_ context.Context, argv []string) ([]byte, error) {
			*ran = append(*ran, argv)
			return nil, nil
		},
	}, ran
}

func ids(apps []App) []string {
	out := []string{}
	for _, a := range apps {
		out = append(out, a.ID)
	}
	return out
}

func TestAppsAreThoseInstalled(t *testing.T) {
	o, _ := machine(map[string]string{"HOME": "/Users/me"}, "/System/Applications/Utilities/Terminal.app", "/Users/me/Applications/kitty.app", "/Applications/Unknown.app")
	if got, want := ids(o.Apps()), []string{"kitty", "terminal"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Apps = %v, want %v: those found in any Applications folder", got, want)
	}
	if got := o.Default(); got != "kitty" {
		t.Errorf("Default = %q, want the first installed when Slorp was started from none of them", got)
	}

	o.OS = "plan9"
	if got := o.Apps(); got == nil || len(got) != 0 {
		t.Errorf("Apps = %v on a system with no presets, want an empty list", got)
	}
	if got := o.Default(); got != "" {
		t.Errorf("Default = %q on a system with no presets, want none", got)
	}
	if err := o.Open(context.Background(), "terminal", "true"); !errors.Is(err, ErrUnknown) {
		t.Errorf("Open on a system with no presets = %v, want ErrUnknown", err)
	}
}

func TestDefaultIsTheTerminalSlorpWasStartedFrom(t *testing.T) {
	all := []string{"/Applications/iTerm.app", "/Applications/Ghostty.app", "/Applications/kitty.app", "/Applications/WezTerm.app", "/Applications/Alacritty.app", "/System/Applications/Utilities/Terminal.app"}
	for want, env := range map[string]map[string]string{
		"iterm":     {"TERM_PROGRAM": "iTerm.app"},
		"ghostty":   {"TERM_PROGRAM": "ghostty"},
		"kitty":     {"KITTY_WINDOW_ID": "1"},
		"wezterm":   {"WEZTERM_PANE": "0"},
		"alacritty": {"ALACRITTY_WINDOW_ID": "7"},
		"terminal":  {"TERM_PROGRAM": "Apple_Terminal"},
	} {
		o, _ := machine(env, all...)
		if got := o.Default(); got != want {
			t.Errorf("Default with %v = %q, want %q", env, got, want)
		}
	}
	o, _ := machine(map[string]string{"TERM_PROGRAM": "iTerm.app"}, "/System/Applications/Utilities/Terminal.app")
	if got := o.Default(); got != "terminal" {
		t.Errorf("Default = %q when the terminal Slorp was started from is not found, want one that is", got)
	}
}

func TestOpenRunsTheCommandInANewWindow(t *testing.T) {
	const command = `cd -- '/work/a b' && 'claude' attach "x"`
	kept := command + `; exec '/bin/fish' -l`
	tests := map[string][]string{
		"iterm": {"osascript", "-e", "on run argv", "-e", `tell application "iTerm"`, "-e", "set w to (create window with default profile)",
			"-e", "tell current session of w to write text (item 1 of argv)", "-e", "activate", "-e", "end tell", "-e", "end run", command},
		"terminal":  {"osascript", "-e", "on run argv", "-e", `tell application "Terminal"`, "-e", "do script (item 1 of argv)", "-e", "activate", "-e", "end tell", "-e", "end run", command},
		"ghostty":   {"open", "-na", "/Applications/Ghostty.app", "--args", "-e", "/bin/fish", "-lic", kept},
		"kitty":     {"open", "-na", "/Applications/kitty.app", "--args", "/bin/fish", "-lic", kept},
		"wezterm":   {"/Applications/WezTerm.app/Contents/MacOS/wezterm", "start", "--", "/bin/fish", "-lic", kept},
		"alacritty": {"open", "-na", "/Applications/Alacritty.app", "--args", "-e", "/bin/fish", "-lic", kept},
	}
	for app, want := range tests {
		o, ran := machine(map[string]string{"SHELL": "/bin/fish"}, "/Applications/iTerm.app", "/Applications/Ghostty.app", "/Applications/kitty.app", "/Applications/WezTerm.app", "/Applications/Alacritty.app", "/Applications/Terminal.app")
		if err := o.Open(context.Background(), app, command); err != nil {
			t.Errorf("Open %s: %v", app, err)
		}
		if len(*ran) != 1 || !reflect.DeepEqual((*ran)[0], want) {
			t.Errorf("Open %s ran\n%q, want\n%q", app, *ran, want)
		}
	}

	o, ran := machine(nil, "/Applications/kitty.app")
	if err := o.Open(context.Background(), "kitty", "true"); err != nil || (*ran)[0][4] != "/bin/zsh" {
		t.Errorf("Open with no SHELL set ran %q, %v; want zsh", *ran, err)
	}
	if err := o.Open(context.Background(), "iterm", "true"); !errors.Is(err, ErrUnknown) || len(*ran) != 1 {
		t.Errorf("Open of a terminal that is not installed = %v having run %q, want ErrUnknown and nothing run", err, (*ran)[1:])
	}
}

func TestOpenSaysWhyItCouldNot(t *testing.T) {
	o, _ := machine(nil, "/Applications/iTerm.app")
	fail := func(out string) error {
		o.Run = func(context.Context, []string) ([]byte, error) { return []byte(out), errors.New("exit status 1") }
		return o.Open(context.Background(), "iterm", "true")
	}
	if err := fail("execution error: Not authorized to send Apple events to iTerm. (-1743)\n"); err == nil || !strings.Contains(err.Error(), "Automation") || !strings.Contains(err.Error(), "iTerm2") {
		t.Errorf("refused by macOS: %v, want it to say where to allow Slorp to control iTerm2", err)
	}
	if err := fail("execution error: something else (-1)\n"); err == nil || err.Error() != "opening iTerm2: execution error: something else (-1)" {
		t.Errorf("another failure: %v, want what the command printed", err)
	}
	if err := fail(""); err == nil || err.Error() != "opening iTerm2: exit status 1" {
		t.Errorf("a silent failure: %v, want how the command ended", err)
	}
}
