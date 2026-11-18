package claude

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"
)

const (
	// launchTimeout bounds one of Claude Code's own commands.
	launchTimeout = 20 * time.Second
	// listedEvery is how often the roster is read for a session just started.
	listedEvery = 200 * time.Millisecond
)

// listedWait is how long a session just started is waited for in the roster.
var listedWait = 3 * time.Second

// Start begins a session in the background, working in dir and named name
// when that is not empty, with prompt as the first thing said in it. It
// returns the id the session goes by in the roster, empty when Claude Code
// started it without the roster listing it in time.
func (a *Adapter) Start(ctx context.Context, dir, name, prompt string) (sessionID string, err error) {
	args := []string{"--bg"}
	if name != "" {
		args = append(args, "--name", name)
	}
	return a.launch(ctx, dir, a.withPlugin(args), prompt)
}

// Resume reopens a closed session in the background, in the directory it
// worked in, and says prompt in it when that is not empty. Claude Code starts
// an empty session under sessionID when dir holds no session by that id.
func (a *Adapter) Resume(ctx context.Context, sessionID, dir, prompt string) error {
	args := []string{"--bg", "--resume", sessionID}
	// A session that has run in the background keeps the options it was
	// started with, and Claude Code answers any passed now by starting a
	// copy of it under a new id.
	rows, _ := a.roster(ctx, true)
	if !slices.ContainsFunc(rows, func(r agentRow) bool { return r.SessionID == sessionID }) {
		args = a.withPlugin(args)
	}
	_, err := a.launch(ctx, dir, args, prompt)
	return err
}

// withPlugin adds the option that loads the Slorp plugin, when there is one.
func (a *Adapter) withPlugin(args []string) []string {
	if a.Plugin == "" {
		return args
	}
	return append(args, "--plugin-dir", a.Plugin)
}

// Stop ends a background session by the id Claude Code's commands take. Its
// conversation is kept.
func (a *Adapter) Stop(ctx context.Context, job string) error {
	_, err := a.run(ctx, "", "stop", job)
	a.poke()
	return err
}

func (a *Adapter) launch(ctx context.Context, dir string, args []string, prompt string) (string, error) {
	if prompt != "" {
		args = append(args, "--", prompt)
	}
	out, err := a.run(ctx, dir, args...)
	if err != nil {
		return "", err
	}
	defer a.poke()
	job := backgrounded(out)
	if job == "" {
		return "", nil
	}
	for deadline := time.Now().Add(listedWait); ; {
		rows, _ := a.roster(ctx, false)
		for _, r := range rows {
			if r.ID == job && r.SessionID != "" {
				return r.SessionID, nil
			}
		}
		if time.Now().After(deadline) {
			return "", nil
		}
		select {
		case <-ctx.Done():
			return "", nil
		case <-time.After(listedEvery):
		}
	}
}

// run runs Claude Code from dir and returns what it printed, or as an error
// what it said went wrong.
func (a *Adapter) run(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, launchTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.Bin, args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		said := cmp.Or(strings.TrimSpace(stderr.String()), strings.TrimSpace(string(out)))
		if said == "" {
			return "", fmt.Errorf("claude %s: %w", args[0], err)
		}
		return "", errors.New(Tail(said, 300))
	}
	return string(out), nil
}

// backgrounded reads the id out of what `claude --bg` prints, such as
// "backgrounded · 3e14f875 · name", or returns "" when it said nothing like it.
func backgrounded(out string) string {
	for line := range strings.Lines(out) {
		if rest, ok := strings.CutPrefix(line, "backgrounded · "); ok {
			id, _, _ := strings.Cut(strings.TrimSpace(rest), " ")
			return id
		}
	}
	return ""
}

// poke has the roster read again now.
func (a *Adapter) poke() {
	select {
	case a.woken() <- struct{}{}:
	default:
	}
}
