package opencode

import (
	"fmt"
	"path/filepath"

	"github.com/jefflinse/slorp/internal/hub"
)

// AttachCommand returns a shell command that opens the session running as
// job in OpenCode's own terminal client, connected to the server Slorp runs
// it on. The server's password stays out of the command: it is in a file
// among the session's private ones, which only the user can read and which
// go when the session stops. ok is false when no session Slorp started runs
// as job, or it is starting or stopping.
func (a *Adapter) AttachCommand(job string) (command string, ok bool, err error) {
	a.initLifecycle()
	a.runtimeMu.Lock()
	r := a.runtimes[job]
	a.runtimeMu.Unlock()
	if r == nil || !r.busy.CompareAndSwap(false, true) {
		return "", false, nil
	}
	defer r.busy.Store(false)
	if r.session.ID == "" || r.base == "" {
		return "", false, nil
	}
	script := filepath.Join(r.private, "attach")
	body := "#!/bin/sh\nOPENCODE_PASSWORD=" + hub.ShellQuote(r.password) + " exec " + hub.ShellQuote(a.Bin) +
		" --server " + hub.ShellQuote(r.base) + " --session " + hub.ShellQuote(r.session.ID) + "\n"
	if err := atomicFile(script, []byte(body), 0o600); err != nil {
		return "", true, fmt.Errorf("writing the OpenCode attach script: %w", err)
	}
	return "cd -- " + hub.ShellQuote(r.dir) + " && sh " + hub.ShellQuote(script), true, nil
}
