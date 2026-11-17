package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jefflinse/slorp/internal/hub"
	"github.com/jefflinse/slorp/internal/testdb"
)

// line encodes one transcript entry the way Claude Code writes them: compact
// JSON on a single line.
func line(t *testing.T, v map[string]any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

func user(content any) map[string]any {
	return map[string]any{"type": "user", "gitBranch": "feat/x", "message": map[string]any{"role": "user", "content": content}}
}

func assistant(blocks ...map[string]any) map[string]any {
	return map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": blocks}}
}

func text(s string) map[string]any { return map[string]any{"type": "text", "text": s} }

func appendTo(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func TestTailReadsWhatMatters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	meta := user("text the harness injected, not the user")
	meta["isMeta"] = true
	side := assistant(text("a subagent talking"))
	side["isSidechain"] = true
	sideUser := user("a prompt inside a subagent")
	sideUser["isSidechain"] = true

	appendTo(t, path, strings.Join([]string{
		line(t, map[string]any{"type": "ai-title", "aiTitle": "First title"}),
		line(t, user("build the thing")),
		line(t, assistant(text("On it."), map[string]any{"type": "tool_use", "name": "Bash"}, text("Done."))),
		line(t, user([]map[string]any{{"type": "tool_result", "content": "exit 0"}})),
		line(t, meta),
		line(t, user("<command-name>/effort</command-name>")),
		line(t, side),
		line(t, sideUser),
		line(t, map[string]any{"type": "file-history-snapshot", "snapshot": map[string]any{"type": "assistant"}}),
		"this line is not JSON at all\n",
		line(t, map[string]any{"type": "ai-title", "aiTitle": "Second title"}),
	}, ""))

	tl := &tail{path: path}
	tl.read()

	if tl.title != "Second title" {
		t.Errorf("title = %q, want the latest", tl.title)
	}
	if tl.branch != "feat/x" {
		t.Errorf("branch = %q", tl.branch)
	}
	if tl.lastPrompt != "build the thing" {
		t.Errorf("lastPrompt = %q; tool results, meta entries, command echoes and subagent prompts are not the user's prompt", tl.lastPrompt)
	}
	if tl.lastReply != "On it.\n\nDone." {
		t.Errorf("lastReply = %q; want the main thread's text blocks joined", tl.lastReply)
	}
}

func TestTailReadsIncrementally(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	appendTo(t, path, line(t, assistant(text("first"))))
	tl := &tail{path: path}
	tl.read()
	if tl.lastReply != "first" {
		t.Fatalf("lastReply = %q", tl.lastReply)
	}

	whole := line(t, assistant(text("second")))
	appendTo(t, path, whole[:len(whole)-10])
	tl.read()
	if tl.lastReply != "first" {
		t.Fatalf("a half-written line was read: %q", tl.lastReply)
	}

	appendTo(t, path, whole[len(whole)-10:])
	tl.read()
	if tl.lastReply != "second" {
		t.Fatalf("the completed line was not picked up: %q", tl.lastReply)
	}

	appendTo(t, path, line(t, map[string]any{"type": "last-prompt", "lastPrompt": "and then?"}))
	tl.read()
	if tl.lastPrompt != "and then?" || tl.lastReply != "second" {
		t.Errorf("after a later append: prompt %q reply %q", tl.lastPrompt, tl.lastReply)
	}
}

func TestTailSurvivesAMissingOrReplacedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	tl := &tail{path: path}
	tl.read() // no file yet

	appendTo(t, path, line(t, assistant(text("a long first message that makes the file big"))))
	tl.read()
	if err := os.WriteFile(path, []byte(line(t, assistant(text("new")))), 0o600); err != nil {
		t.Fatal(err)
	}
	tl.read()
	if tl.lastReply != "new" {
		t.Errorf("a file that shrank was not re-read from the start: %q", tl.lastReply)
	}
}

func TestTruncation(t *testing.T) {
	if got := Tail("  short  ", 10); got != "short" {
		t.Errorf("Tail = %q", got)
	}
	if got := Tail("abcdefghij", 4); got != "…ghij" {
		t.Errorf("Tail = %q, want the end kept and the cut marked", got)
	}
	if got := head("abcdefghij", 4); got != "abcd…" {
		t.Errorf("head = %q, want the start kept and the cut marked", got)
	}
	if got := Tail("héllo wörld", 5); got != "…wörld" {
		t.Errorf("Tail cut inside a character: %q", got)
	}
}

func TestStatus(t *testing.T) {
	tests := []struct {
		row  agentRow
		want hub.Status
	}{
		{agentRow{Status: "busy"}, hub.Working},
		{agentRow{Status: "waiting"}, hub.Waiting},
		{agentRow{Status: "idle"}, hub.Idle},
		{agentRow{State: "working"}, hub.Working},
		{agentRow{State: "blocked"}, hub.Waiting},
		{agentRow{State: "done"}, hub.Done},
		{agentRow{State: "failed"}, hub.Failed},
		{agentRow{State: "stopped"}, hub.Idle},
		{agentRow{}, hub.Idle},
	}
	for _, tt := range tests {
		if got := status(tt.row); got != tt.want {
			t.Errorf("status(%+v) = %q, want %q", tt.row, got, tt.want)
		}
	}
}

func TestPollBuildsTheRoster(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	roster := `[
	  {"cwd":"/w/a","kind":"interactive","startedAt":1700000000000,"pid":1,"status":"waiting","waitingFor":"permission prompt","sessionId":"aaa","name":"a-1"},
	  {"cwd":"/w/b","kind":"background","startedAt":1700000000000,"state":"blocked","sessionId":"bbb","name":"bg"},
	  {"cwd":"/w/c","kind":"interactive","status":"idle","name":"no session id yet"}
	]`
	script := "#!/bin/sh\n[ \"$1 $2\" = \"agents --json\" ] || exit 2\ncat <<'EOF'\n" + roster + "\nEOF\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(dir, ".claude", "projects", "-w-a")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	appendTo(t, filepath.Join(project, "aaa.jsonl"), line(t, map[string]any{"type": "ai-title", "aiTitle": "Session A"})+line(t, user("go"))+line(t, assistant(text("May I run this?"))))

	st := testdb.Open(t)
	h := hub.New(testdb.KB(t, st), st)
	a := &Adapter{Hub: h, Bin: bin, Home: dir, tails: map[string]*tail{}}
	if err := a.poll(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := map[string]hub.Session{}
	for _, s := range h.Sessions() {
		got[s.ID] = s
	}
	if len(got) != 2 {
		t.Fatalf("sessions = %d, want the two rows that have a session id", len(got))
	}
	sa := got["aaa"]
	if sa.Agent != Agent || sa.Status != hub.Waiting || sa.WaitingFor != "permission prompt" || sa.Cwd != "/w/a" || sa.Name != "a-1" {
		t.Errorf("session from the roster: %+v", sa)
	}
	if sa.Title != "Session A" || sa.LastPrompt != "go" || sa.LastReply != "May I run this?" || sa.Branch != "feat/x" {
		t.Errorf("session was not filled in from its transcript: %+v", sa)
	}
	if sa.StartedAt.UnixMilli() != 1700000000000 || !sa.ActiveAt.After(sa.StartedAt) {
		t.Errorf("times: started %v, active %v; want the roster's start and the transcript's last write", sa.StartedAt, sa.ActiveAt)
	}
	if sb := got["bbb"]; sb.Status != hub.Waiting || sb.Kind != "background" || sb.Title != "" {
		t.Errorf("background session with no transcript: %+v", sb)
	}

	a.Bin = filepath.Join(dir, "missing")
	if err := a.poll(context.Background()); err == nil {
		t.Error("a roster command that cannot run reported success")
	}
	if len(h.Sessions()) != 2 {
		t.Error("a failed poll wiped the sessions already known")
	}
}
