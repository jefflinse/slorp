package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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

func TestTailReadsTheNameTheUserGaveTheSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	named := func(as string) string { return line(t, map[string]any{"type": "custom-title", "customTitle": as}) }
	appendTo(t, path, line(t, map[string]any{"type": "ai-title", "aiTitle": "Its own title"}))
	tl := &tail{path: path}
	tl.read()
	if tl.named != "" || tl.title != "Its own title" {
		t.Fatalf("named %q, titled %q before any renaming", tl.named, tl.title)
	}
	appendTo(t, path, named("First name")+named("  Second name ")+line(t, map[string]any{"type": "ai-title", "aiTitle": "Its own title"}))
	tl.read()
	if tl.named != "Second name" || tl.title != "Its own title" {
		t.Errorf("named %q, titled %q; want the latest name the user gave, beside the session's own title", tl.named, tl.title)
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
		// A background session with a live process says both.
		{agentRow{Status: "busy", State: "working"}, hub.Working},
		{agentRow{Status: "idle", State: "done"}, hub.Done},
		{agentRow{Status: "idle", State: "failed"}, hub.Failed},
		{agentRow{Status: "waiting", State: "working"}, hub.Waiting},
		{agentRow{Status: "busy", State: "blocked"}, hub.Waiting},
		{agentRow{Status: "busy", State: "done"}, hub.Working},
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
	  {"id":"aaa00000","cwd":"/w/a","kind":"interactive","startedAt":1700000000000,"pid":1,"status":"waiting","waitingFor":"permission prompt","sessionId":"aaa","name":"a-1"},
	  {"id":"bbb00000","cwd":"/w/b","kind":"background","startedAt":1700000000000,"state":"blocked","sessionId":"bbb","name":"bg"},
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
	appendTo(t, filepath.Join(project, "aaa.jsonl"), line(t, map[string]any{"type": "ai-title", "aiTitle": "Session A"})+line(t, map[string]any{"type": "custom-title", "customTitle": "Renamed A"})+line(t, user("go"))+line(t, block("msg_1", 10, 20, 30, 900)))

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
	if sa.Named != "Renamed A" || sa.Label != "Renamed A" || sa.Title != "Session A" || sa.LastPrompt != "go" || sa.LastReply != "x" || sa.Branch != "feat/x" {
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
	if got["bbb"].Job != "bbb00000" || sa.Job != "" {
		t.Errorf("jobs %q and %q, want only the background session to have one", got["bbb"].Job, sa.Job)
	}

	a.Bin = filepath.Join(dir, "missing")
	if err := a.poll(context.Background()); err == nil {
		t.Error("a roster command that cannot run reported success")
	}
	if len(h.Sessions()) != 2 {
		t.Error("a failed poll wiped the sessions already known")
	}
}

// by is a user entry as the session id wrote it.
func by(id, prompt string) map[string]any {
	e := user(prompt)
	e["session_id"] = id
	return e
}

func TestOrigin(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, ".claude", "projects", "-w")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	side := by("a-subagent", "inside a subagent")
	side["isSidechain"] = true
	write := func(id string, entries ...map[string]any) {
		t.Helper()
		for _, e := range entries {
			appendTo(t, filepath.Join(project, id+".jsonl"), line(t, e))
		}
	}
	write("fresh", map[string]any{"type": "ai-title", "aiTitle": "A title"}, user("said before the id was written"), side, by("fresh", "go"), by("later", "on"))
	write("carried", side, by("before", "copied from the session before"), by("carried", "said here"))
	write("silent", user("nothing says who wrote this"))

	a := &Adapter{Home: home}
	for id, want := range map[string]string{"fresh": "fresh", "carried": "before", "silent": "", "missing": "", "*": "", "../-w/fresh": "", "": ""} {
		if got := a.origin(id); got != want {
			t.Errorf("origin(%q) = %q, want %q", id, got, want)
		}
	}

	tl := &tail{path: filepath.Join(project, "carried.jsonl")}
	tl.read()
	if tl.origin != "before" {
		t.Errorf("a transcript read to its end began with %q, want the first session to write in it", tl.origin)
	}
}

func TestPollFollowsAConversationToItsNewID(t *testing.T) {
	bin, _ := fakeClaude(t, `[{"id":"j1","cwd":"/w","kind":"interactive","startedAt":1700000000000,"status":"idle","sessionId":"old"}]`, "")
	home := t.TempDir()
	project := filepath.Join(home, ".claude", "projects", "-w")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	appendTo(t, filepath.Join(project, "old.jsonl"), line(t, by("old", "add flanges")))
	st := testdb.Open(t)
	h := hub.New(testdb.KB(t, st), st)
	a := &Adapter{Hub: h, Bin: bin, Home: home, tails: map[string]*tail{}}
	if err := a.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.Rename("old", "Flange work")

	// The conversation moved on twice while nothing was watching its second id.
	appendTo(t, filepath.Join(project, "between.jsonl"), line(t, by("old", "add flanges")))
	appendTo(t, filepath.Join(project, "new.jsonl"), line(t, by("between", "now add washers")))
	if err := os.WriteFile(filepath.Join(filepath.Dir(bin), "roster"), []byte(`[{"id":"j1","cwd":"/w","kind":"background","startedAt":1700000000000,"status":"idle","sessionId":"new"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.poll(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := h.State()
	if len(got.Sessions) != 1 || len(got.Archive) != 0 {
		t.Fatalf("%d open and %d closed sessions, want the one conversation under its new id", len(got.Sessions), len(got.Archive))
	}
	s := got.Sessions[0]
	if s.ID != "new" || s.Label != "Flange work" || len(s.Timeline) != 2 || s.Timeline[0].Prompt != "add flanges" || s.Timeline[1].Prompt != "now add washers" {
		t.Errorf("session %q named %q with timeline %+v; want the new id with the old one's name and both prompts", s.ID, s.Label, s.Timeline)
	}
}

func TestRunTracesTheSessionsThatClosedBeforeItWatched(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, ".claude", "projects", "-w")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	appendTo(t, filepath.Join(project, "old.jsonl"), line(t, by("old", "add flanges")))
	appendTo(t, filepath.Join(project, "new.jsonl"), line(t, by("old", "now add washers")))
	st := testdb.Open(t)
	h := hub.New(testdb.KB(t, st), st)
	at := time.Now().Add(-time.Hour)
	old := hub.Session{ID: "old", Agent: Agent, Status: hub.Idle, LastPrompt: "add flanges", PromptAt: at}
	h.SetRoster(Agent, []hub.Session{old, {ID: "new", Agent: Agent, Status: hub.Idle, LastPrompt: "now add washers", PromptAt: at.Add(time.Minute)}})
	h.SetRoster(Agent, nil)
	if got := h.Untraced(Agent); len(got) != 2 {
		t.Fatalf("untraced = %v, want both closed sessions", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	(&Adapter{Hub: h, Bin: filepath.Join(home, "no-claude"), Home: home, Interval: time.Hour}).Run(ctx)

	if got := h.Untraced(Agent); len(got) != 0 {
		t.Errorf("untraced = %v after Run, want every closed session traced", got)
	}
	h.SetRoster(Agent, []hub.Session{old})
	if got := h.State(); len(got.Archive) != 0 || len(got.Sessions) != 1 || len(got.Sessions[0].Timeline) != 2 {
		t.Errorf("%d closed and sessions %+v; want the reopened session to hold what the one that continued it did", len(got.Archive), got.Sessions)
	}
}

// spawned writes what Claude Code keeps of one subagent of the session whose
// transcript is at path: its note, and entries added to its own transcript.
func spawned(t *testing.T, path, id, note string, entries ...map[string]any) {
	t.Helper()
	dir := subagentsDir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if note != "" {
		if err := os.WriteFile(filepath.Join(dir, "agent-"+id+".meta.json"), []byte(note), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var lines string
	for _, e := range entries {
		e["isSidechain"], e["agentId"] = true, id
		lines += line(t, e)
	}
	appendTo(t, filepath.Join(dir, "agent-"+id+".jsonl"), lines)
}

// at stamps an entry with a time of day on one fixed date.
func at(clock string, e map[string]any) map[string]any {
	e["timestamp"] = "2026-11-17T" + clock + "Z"
	return e
}

func toolUse(name string, input map[string]any) map[string]any {
	return map[string]any{"type": "tool_use", "name": name, "input": input}
}

func TestPollListsASessionsSubagents(t *testing.T) {
	bin, _ := fakeClaude(t, `[{"id":"aaa00000","cwd":"/w/a","kind":"interactive","startedAt":1700000000000,"status":"busy","sessionId":"aaa"}]`, "")
	home := t.TempDir()
	path := filepath.Join(home, ".claude", "projects", "-w-a", "aaa.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	appendTo(t, path, line(t, user("look into both"))+line(t, block("msg_main", 1, 2, 3, 4)))

	asked := at("02:55:54", user("Find every caller of Apply."))
	first := at("02:56:10", block("msg_a1", 10, 20, 30, 40))
	first["message"].(map[string]any)["content"] = []map[string]any{text("Looking."), toolUse("Bash", map[string]any{"command": "grep"}), toolUse("Read", nil)}
	spawned(t, path, "a-late", `{"agentType":"general-purpose","description":"Write the fix","spawnDepth":2,"requestShape":"foreground","model":"claude-haiku-5-5"}`,
		at("03:10:00", user("Fix it.")))
	spawned(t, path, "z-early", `{"agentType":"Explore","description":"Find the callers","spawnDepth":1,"requestShape":"background","model":"claude-opus-5-5"}`, asked, first)

	st := testdb.Open(t)
	h := hub.New(testdb.KB(t, st), st)
	a := &Adapter{Hub: h, Bin: bin, Home: home, tails: map[string]*tail{}}
	poll := func() hub.Session {
		t.Helper()
		if err := a.poll(context.Background()); err != nil {
			t.Fatal(err)
		}
		return h.Sessions()[0]
	}

	s := poll()
	if len(s.Subagents) != 2 || s.Subagents[0].ID != "z-early" || s.Subagents[1].ID != "a-late" {
		t.Fatalf("subagents = %+v, want both, the first started first", s.Subagents)
	}
	early, late := s.Subagents[0], s.Subagents[1]
	if early.Type != "Explore" || early.Description != "Find the callers" || !early.Background || early.Depth != 1 || early.Done {
		t.Errorf("what it was started as: %+v", early)
	}
	if early.StartedAt.Format("15:04:05") != "02:55:54" || early.ActiveAt.Format("15:04:05") != "02:56:10" {
		t.Errorf("times: started %v, active %v; want its first and last entries'", early.StartedAt, early.ActiveAt)
	}
	if early.Model != "claude-fable-5-2" || early.Effort != "high" || early.Tools != 2 || early.Used != (hub.Tokens{In: 10, CacheWrite: 20, CacheRead: 30, Out: 40}) {
		t.Errorf("model %q, effort %q, %d tool calls, used %+v; want them from its transcript, whatever model it was started on", early.Model, early.Effort, early.Tools, early.Used)
	}
	if late.Background || late.Depth != 2 || late.Model != "claude-haiku-5-5" || late.Tools != 0 || late.Done {
		t.Errorf("one that has yet to answer: %+v, want what it was started as and the model it was started on", late)
	}
	if s.Used != (hub.Tokens{In: 11, CacheWrite: 22, CacheRead: 33, Out: 44}) {
		t.Errorf("session used %+v, want its subagents' tokens counted with its own", s.Used)
	}

	// The response grows by a block, and the subagent hands back and finishes.
	more := at("02:57:00", block("msg_a1", 10, 20, 30, 90))
	more["message"].(map[string]any)["content"] = []map[string]any{toolUse(handback, map[string]any{"message": "Two callers: hub and server."})}
	spawned(t, path, "z-early", `{"agentType":"Explore","description":"Find the callers","spawnDepth":1,"requestShape":"background","model":"claude-opus-5-5","finished":true}`, more)
	s = poll()
	if early = s.Subagents[0]; !early.Done || early.Tools != 3 || early.Used.Out != 90 || early.Used.In != 10 || early.ActiveAt.Format("15:04:05") != "02:57:00" {
		t.Errorf("after it finished: %+v; want it done, with only what was new read", early)
	}
	if s.Used.Out != 94 {
		t.Errorf("session used %+v, want the subagent's later tokens in place of its earlier", s.Used)
	}

	if got := a.Subagents("aaa"); len(got) != 2 || !reflect.DeepEqual(got[0], early) {
		t.Errorf("read afresh = %+v, want what the roster has", got)
	}
	if got := a.Subagents("zzz"); got != nil {
		t.Errorf("subagents of a session with no transcript = %+v", got)
	}
}

func TestReport(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude", "projects", "-w-a", "aaa.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	appendTo(t, path, line(t, user("go")))
	said := func(s string) map[string]any { return at("02:56:00", assistant(text(s))) }
	spawned(t, path, "handed", `{}`, at("02:55:00", user("Find every caller.")), at("02:55:30", user([]map[string]any{{"type": "tool_result", "content": "x"}})),
		said("Looking."), at("02:57:00", assistant(text("Reporting."), toolUse(handback, map[string]any{"message": "Two callers."}))), said("Done."))
	spawned(t, path, "talking", `{}`, at("02:55:00", user("Fix it.")), said("Trying one thing."), said("Trying another."))
	// A transcript outside the session's subagents, and one where a session
	// with no transcript of its own would be looked for.
	elsewhere := t.TempDir()
	t.Chdir(elsewhere)
	for _, outside := range []string{filepath.Join(filepath.Dir(path), "agent-outside.jsonl"), filepath.Join(elsewhere, "subagents", "agent-handed.jsonl")} {
		if err := os.MkdirAll(filepath.Dir(outside), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(outside, []byte(line(t, at("02:55:00", user("secret")))), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	a := &Adapter{Home: home}
	if got, ok := a.Report("aaa", "handed"); !ok || got != (hub.SubagentReport{Prompt: "Find every caller.", Report: "Two callers.", Handed: true}) {
		t.Errorf("report = %+v, %v; want what it was first told and what it handed back", got, ok)
	}
	if got, ok := a.Report("aaa", "talking"); !ok || got != (hub.SubagentReport{Prompt: "Fix it.", Report: "Trying another."}) {
		t.Errorf("report = %+v, %v; want the last thing it said, for one that has handed nothing back", got, ok)
	}
	for _, id := range []string{"nobody", "", "../../../../agent-outside", "..%2Foutside"} {
		if got, ok := a.Report("aaa", id); ok {
			t.Errorf("report of %q = %+v, want none", id, got)
		}
	}
	if _, ok := a.Report("zzz", "handed"); ok {
		t.Error("a report from a session with no transcript")
	}
}

func TestSubagentsAreReadOnlyAsFarAsTheyAreNew(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aaa.jsonl")
	first := at("02:56:10", block("msg_1", 10, 20, 30, 40))
	first["message"].(map[string]any)["content"] = []map[string]any{toolUse("Bash", nil), toolUse("Read", nil)}
	spawned(t, path, "sub", `{}`, at("02:55:54", user("Find them.")), first)

	known := map[string]*subagent{}
	if got := subagents(path, known); len(got) != 1 || got[0].Tools != 2 {
		t.Fatalf("subagents = %+v, want the one with its two tool calls", got)
	}

	// What was read is blanked where it lies, and more is written after it.
	file := filepath.Join(subagentsDir(path), "agent-sub.jsonl")
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(strings.Repeat(" ", len(raw)-1)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	more := at("02:57:00", block("msg_2", 1, 2, 3, 4))
	more["message"].(map[string]any)["content"] = []map[string]any{toolUse("Edit", nil)}
	spawned(t, path, "sub", "", more)

	got := subagents(path, known)
	if len(got) != 1 || got[0].Tools != 3 || got[0].Used != (hub.Tokens{In: 11, CacheWrite: 22, CacheRead: 33, Out: 44}) || got[0].StartedAt.Format("15:04:05") != "02:55:54" {
		t.Errorf("subagents = %+v, want what was read before kept and only the rest read", got)
	}
}
