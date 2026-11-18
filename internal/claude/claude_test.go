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

// answered is a response the model gave at an effort, over that many tokens
// sent fresh, written to the cache and read from it.
func answered(model, effort string, fresh, written, read int, blocks ...map[string]any) map[string]any {
	e := assistant(blocks...)
	e["effort"] = effort
	m := e["message"].(map[string]any)
	m["model"] = model
	m["usage"] = map[string]any{"input_tokens": fresh, "cache_creation_input_tokens": written, "cache_read_input_tokens": read, "output_tokens": 900}
	return e
}

func TestTailReadsWhatTheModelWasGiven(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	side := answered("claude-haiku-5-5", "low", 5, 0, 0, text("a subagent"))
	side["isSidechain"] = true
	notice := answered("<synthetic>", "", 0, 0, 0, text("API Error: overloaded"))

	appendTo(t, path, line(t, answered("claude-opus-5-5", "medium", 1, 2, 3, text("first")))+
		line(t, answered("claude-fable-5-2", "high", 2, 40000, 30000, text("second")))+
		line(t, side)+line(t, notice))
	tl := &tail{path: path}
	tl.read()
	if tl.model != "claude-fable-5-2" || tl.reasoning != "high" || tl.contextTokens != 70002 {
		t.Errorf("model %q, reasoning %q, %d tokens; want the main thread's last real response, with everything it was sent counted",
			tl.model, tl.reasoning, tl.contextTokens)
	}
	if tl.lastReply != "API Error: overloaded" {
		t.Errorf("lastReply = %q; a notice is still the last thing said", tl.lastReply)
	}

	appendTo(t, path, line(t, map[string]any{"type": "system", "subtype": "compact_boundary", "compactMetadata": map[string]any{"postTokens": 20000}}))
	tl.read()
	if tl.contextTokens != 0 || tl.model != "claude-fable-5-2" {
		t.Errorf("after a compaction: %d tokens, model %q; want the count unknown and the model kept", tl.contextTokens, tl.model)
	}

	appendTo(t, path, line(t, answered("claude-fable-5-2", "high", 0, 0, 25000, text("third")))+
		line(t, map[string]any{"type": "system", "subtype": "turn_duration", "of": map[string]any{"type": "assistant"}}))
	tl.read()
	if tl.contextTokens != 25000 {
		t.Errorf("after the next response and a notice that is no compaction: %d tokens", tl.contextTokens)
	}
}

// block is one content block of the response id, carrying the usage the
// response had reported by then.
func block(id string, fresh, written, read, out int) map[string]any {
	e := answered("claude-fable-5-2", "high", fresh, written, read, text("x"))
	m := e["message"].(map[string]any)
	m["id"] = id
	m["usage"].(map[string]any)["output_tokens"] = out
	return e
}

func TestTailTotalsWhatWasUsed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	side := block("msg_side", 5, 0, 100, 7)
	side["isSidechain"] = true

	appendTo(t, path, line(t, block("msg_1", 1, 20, 300, 4))+line(t, block("msg_2", 2, 0, 320, 10)))
	tl := &tail{path: path}
	tl.read()
	appendTo(t, path, line(t, block("msg_1", 1, 20, 300, 60))+line(t, side))
	tl.read()

	if want := (hub.Tokens{In: 8, CacheWrite: 20, CacheRead: 720, Out: 77}); tl.used != want {
		t.Errorf("used = %+v, want %+v: each response once, at what it last reported, subagents' included", tl.used, want)
	}
	if tl.contextTokens != 321 {
		t.Errorf("context = %d tokens; a subagent's response is not what the session was last answered over", tl.contextTokens)
	}
}

func TestTailReadsWhenThePromptWasSaid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	typed := user("build the thing")
	typed["timestamp"] = "2026-11-17T10:00:00.250Z"
	appendTo(t, path, line(t, typed)+line(t, map[string]any{"type": "last-prompt", "lastPrompt": "build the thing"}))
	tl := &tail{path: path}
	tl.read()
	if want := time.Date(2026, 11, 17, 10, 0, 0, 250e6, time.UTC); !tl.promptAt.Equal(want) {
		t.Fatalf("promptAt = %v, want %v from the prompt's own entry", tl.promptAt, want)
	}

	appendTo(t, path, line(t, map[string]any{"type": "last-prompt", "lastPrompt": "and a picture"}))
	tl.read()
	if tl.lastPrompt != "and a picture" || !tl.promptAt.IsZero() {
		t.Errorf("prompt %q said at %v; a prompt with no entry of its own has no time", tl.lastPrompt, tl.promptAt)
	}

	untimed := user("no clock")
	untimed["timestamp"] = "yesterday"
	appendTo(t, path, line(t, typed)+line(t, untimed))
	tl.read()
	if tl.lastPrompt != "no clock" || !tl.promptAt.IsZero() {
		t.Errorf("prompt %q said at %v; a time that cannot be read is no time", tl.lastPrompt, tl.promptAt)
	}
}

func TestTailKeepsThePromptAsItWasTyped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	long := "1. Yes!\n2. Yes!\n" + strings.Repeat("and another thing. ", 20) + "the end"
	note := func(of string) map[string]any {
		flat := []rune(strings.Join(strings.Fields(of), " "))
		return map[string]any{"type": "last-prompt", "lastPrompt": string(flat[:min(len(flat), 200)]) + "…"}
	}
	typedAt := func(content any, at string) map[string]any {
		e := user(content)
		e["timestamp"] = at
		return e
	}
	tl := &tail{path: path}
	check := func(when, prompt string, at time.Time) {
		t.Helper()
		tl.read()
		if tl.lastPrompt != prompt || !tl.promptAt.Equal(at) {
			t.Errorf("%s: prompt %q said at %v; want %q at %v", when, tl.lastPrompt, tl.promptAt, prompt, at)
		}
	}
	ten, eleven := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC), time.Date(2026, 11, 17, 11, 0, 0, 0, time.UTC)

	appendTo(t, path, line(t, typedAt(long, "2026-11-17T10:00:00Z"))+line(t, note(long)))
	check("a long prompt, then Claude Code's short note of it", long, ten)

	pictured := []map[string]any{text("look at this: [Image #1]"), {"type": "image", "source": map[string]any{"data": "…"}}, text("and say what you see")}
	appendTo(t, path, line(t, typedAt(pictured, "2026-11-17T11:00:00Z"))+
		line(t, map[string]any{"type": "last-prompt", "lastPrompt": "look at this: [Image #1]  and say what you see"}))
	check("a prompt with a picture", "look at this: [Image #1]\n\nand say what you see", eleven)

	appendTo(t, path, line(t, typedAt([]map[string]any{text("[Request interrupted by user for tool use]")}, "2026-11-17T12:00:00Z"))+
		line(t, typedAt([]map[string]any{{"type": "tool_result", "content": "exit 0"}, text("not typed")}, "2026-11-17T12:00:01Z")))
	check("notices and tool results", "look at this: [Image #1]\n\nand say what you see", eleven)

	appendTo(t, path, line(t, map[string]any{"type": "last-prompt", "lastPrompt": "look at this"}))
	check("a whole note that only begins like the prompt", "look at this", time.Time{})

	appendTo(t, path, line(t, note("look at that instead")))
	check("a note of a prompt that was never read", "look at that instead…", time.Time{})
}

func TestExchanges(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, ".claude", "projects", "-w")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	a := &Adapter{Home: home}
	at := func(e map[string]any, when string) map[string]any {
		e["timestamp"] = when
		return e
	}
	with := func(e map[string]any, key string) map[string]any {
		e[key] = true
		return e
	}
	appendTo(t, filepath.Join(project, "abc-123.jsonl"), strings.Join([]string{
		line(t, with(user("This session is being continued from a previous conversation."), "isCompactSummary")),
		line(t, assistant(text("an answer to nothing the user said"))),
		line(t, at(user("fix the rounding"), "2026-11-17T10:00:00Z")),
		line(t, assistant(text("Looking."), map[string]any{"type": "tool_use", "name": "Bash"})),
		line(t, user([]map[string]any{{"type": "tool_result", "content": "exit 0"}})),
		line(t, with(user("a prompt inside a subagent"), "isSidechain")),
		line(t, with(user("[Image: source: /tmp/x.png]"), "isMeta")),
		line(t, assistant(text("Fixed it. "+strings.Repeat("y", exchangeReply)))),
		line(t, with(assistant(text("a subagent's answer")), "isSidechain")),
		"not JSON\n",
		line(t, at(user([]map[string]any{text("now look: [Image #1]"), {"type": "image"}}), "2026-11-17T11:00:00Z")),
		line(t, assistant(text("I see a chart."))),
		line(t, at(user("and ship it"), "2026-11-17T12:00:00Z")),
	}, ""))

	got, err := a.Exchanges("abc-123", 10)
	if err != nil || len(got) != 3 {
		t.Fatalf("Exchanges = %+v, %v; want the three things the user typed", got, err)
	}
	if x := got[0]; x.Prompt != "fix the rounding" || !x.At.Equal(time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)) || x.Reply != "…"+strings.Repeat("y", exchangeReply) {
		t.Errorf("first = %q at %v, answered %d runes; want the prompt, its time, and the end of the main thread's last answer", x.Prompt, x.At, len([]rune(x.Reply)))
	}
	if x := got[1]; x.Prompt != "now look: [Image #1]" || x.Reply != "I see a chart." {
		t.Errorf("second = %+v, want the prompt with a picture and its answer", x)
	}
	if x := got[2]; x.Prompt != "and ship it" || x.Reply != "" {
		t.Errorf("third = %+v, want the prompt not yet answered", x)
	}
	if got, _ := a.Exchanges("abc-123", 2); len(got) != 2 || got[0].Prompt != "now look: [Image #1]" {
		t.Errorf("limited to two: %+v, want the latest two", got)
	}

	for _, id := range []string{"", "*", "../abc-123", "abc-123/..", "abc 123"} {
		if got, err := a.Exchanges(id, 10); err == nil {
			t.Errorf("Exchanges(%q) = %+v; want it refused as no session id", id, got)
		}
	}
	if _, err := a.Exchanges("nowhere", 10); err == nil {
		t.Error("a session with no transcript read as empty rather than failing")
	}
}

func TestTailDoesNotTakeACompactionSummaryForAPrompt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	summary := user("This session is being continued from a previous conversation.")
	summary["isCompactSummary"] = true
	appendTo(t, path, line(t, user("the real prompt"))+line(t, summary))
	tl := &tail{path: path}
	tl.read()
	if tl.lastPrompt != "the real prompt" {
		t.Errorf("lastPrompt = %q", tl.lastPrompt)
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
	if fi, _ := os.Stat(path); tl.offset != fi.Size() {
		t.Errorf("offset = %d of %d bytes; the next read would start over", tl.offset, fi.Size())
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
	appendTo(t, filepath.Join(project, "aaa.jsonl"), line(t, map[string]any{"type": "ai-title", "aiTitle": "Session A"})+line(t, user("go"))+line(t, block("msg_1", 10, 20, 30, 900)))

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
	if sa.Title != "Session A" || sa.LastPrompt != "go" || sa.LastReply != "x" || sa.Branch != "feat/x" {
		t.Errorf("session was not filled in from its transcript: %+v", sa)
	}
	if sa.Model != "claude-fable-5-2" || sa.Reasoning != "high" || sa.ContextTokens != 60 || sa.Used != (hub.Tokens{In: 10, CacheWrite: 20, CacheRead: 30, Out: 900}) {
		t.Errorf("model %q, reasoning %q, %d tokens, used %+v; want them from the transcript", sa.Model, sa.Reasoning, sa.ContextTokens, sa.Used)
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
