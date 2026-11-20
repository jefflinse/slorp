package hub

import (
	"context"
	"errors"
	"strings"
)

var ErrLifecycleConflict = errors.New("session lifecycle operation conflicts with an active operation")
var ErrLaunchInvalid = errors.New("invalid launch choice")
var ErrLifecycleUnknown = errors.New("native lifecycle outcome is unknown; do not repeat with a new requestId")

type checkpointKey struct{}
type launchJobKey struct{}

// WithLaunchJob receives the created session and immutable owned job synchronously before Start returns.
func WithLaunchJob(ctx context.Context, report func(id, job string)) context.Context {
	return context.WithValue(ctx, launchJobKey{}, report)
}

// ReportLaunchJob reports the created generation to the launching request, if supplied.
func ReportLaunchJob(ctx context.Context, id, job string) {
	if report, ok := ctx.Value(launchJobKey{}).(func(string, string)); ok && report != nil {
		report(id, job)
	}
}

// WithLaunchCheckpoint records a native ID before any prompt can be dispatched.
func WithLaunchCheckpoint(ctx context.Context, checkpoint func(string) error) context.Context {
	return context.WithValue(ctx, checkpointKey{}, checkpoint)
}

// CheckpointLaunch saves the native identity through the operation's checkpoint, if any.
func CheckpointLaunch(ctx context.Context, id string) error {
	if checkpoint, ok := ctx.Value(checkpointKey{}).(func(string) error); ok {
		return checkpoint(id)
	}
	return nil
}

type LaunchAgent struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type LaunchModel struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Provider string   `json:"provider"`
	Variants []string `json:"variants"`
}

type LaunchDefaults struct {
	AgentName string `json:"agentName"`
	Model     string `json:"model"`
	Variant   string `json:"variant"`
}

type LaunchOptions struct {
	Agents   []LaunchAgent  `json:"agents"`
	Models   []LaunchModel  `json:"models"`
	Defaults LaunchDefaults `json:"defaults"`
}

// ShellQuote quotes one argument for a POSIX shell.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

// Launch is what a session is started with.
type Launch struct {
	// Dir is the directory it works in, Name what it is called unless
	// that is empty, and Prompt the first thing said in it.
	Dir, Name, Prompt string
	// Model is the model it runs on and Effort how hard that model works,
	// both in its agent's own words and empty for the agent's usual ones.
	Model, Effort      string
	AgentName, Variant string
	// Local runs it on a local model server in place of its agent's own
	// models, with Model naming one of that server's.
	Local *LocalModel
}

// LocalModel is a local model server a session runs on.
type LocalModel struct {
	// URL is the server's address, which speaks Anthropic's API.
	URL string
	// Thinking says whether the model thinks before it answers.
	Thinking bool
}
