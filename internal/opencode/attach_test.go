package opencode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jefflinse/slorp/internal/hub"
)

func TestAttachCommandKeepsThePasswordInAPrivateFile(t *testing.T) {
	a := runtimeFixture(t)
	id, err := a.Start(context.Background(), hub.Launch{Dir: a.Home, Name: "Native", Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	s := a.Hub.Sessions()[0]
	r := a.runtimes[s.Job]

	if _, ok, err := a.AttachCommand("no-such-job"); ok || err != nil {
		t.Errorf("AttachCommand for a job Slorp runs none as = %v, %v; want no command", ok, err)
	}
	command, ok, err := a.AttachCommand(s.Job)
	script := filepath.Join(r.private, "attach")
	if want := "cd -- " + hub.ShellQuote(a.Home) + " && sh " + hub.ShellQuote(script); !ok || err != nil || command != want {
		t.Fatalf("AttachCommand = %q, %v, %v; want %q", command, ok, err, want)
	}
	if strings.Contains(command, r.password) {
		t.Errorf("the command holds the server's password: %s", command)
	}
	info, err := os.Stat(script)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("attach script mode: %v %v, want one only the user can read", info, err)
	}
	b, _ := os.ReadFile(script)
	if want := "#!/bin/sh\nOPENCODE_PASSWORD=" + hub.ShellQuote(r.password) + " exec " + hub.ShellQuote(a.Bin) + " --server " + hub.ShellQuote(r.base) + " --session " + hub.ShellQuote(id) + "\n"; string(b) != want {
		t.Errorf("attach script =\n%s\nwant\n%s", b, want)
	}
	if again, ok, err := a.AttachCommand(s.Job); again != command || !ok || err != nil {
		t.Errorf("a second AttachCommand = %q, %v, %v; want the same again", again, ok, err)
	}

	if err := a.Stop(context.Background(), s.Job); err != nil {
		t.Fatalf("stopping after an attach command was asked for: %v", err)
	}
	if _, ok, err := a.AttachCommand(s.Job); ok || err != nil {
		t.Errorf("AttachCommand for a stopped session = %v, %v; want no command", ok, err)
	}
	if _, err := os.Stat(script); !os.IsNotExist(err) {
		t.Errorf("the attach script outlived its session: %v", err)
	}
}
