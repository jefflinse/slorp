// Package claude observes Claude Code sessions on this machine: the live
// roster from `claude agents --json`, and each session's title and last
// exchange from its transcript file. It also starts, resumes and stops
// sessions in the background through Claude Code's own commands.
package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jefflinse/slorp/internal/hub"
)

const (
	Agent = "claude"

	// background is the kind of a session Claude Code runs without a terminal.
	background = "background"

	// ReplyLimit is how much of a session's last message is kept, in runes.
	ReplyLimit = 30000

	maxFirstRead = 64 << 20
	// originRead is how much of a transcript is read for where it began.
	originRead  = 16 << 20
	promptLimit = 8000
	// exchangeReply is how much of an answer Exchanges keeps, in runes.
	exchangeReply = 2000
)

type Adapter struct {
	Hub      *hub.Hub
	Bin      string
	Home     string
	Interval time.Duration
	// Plugin is a directory holding the Slorp plugin, loaded into every
	// session started here. Empty when Claude Code has it installed.
	Plugin string
	// URL is where the plugin reaches Slorp, set when Slorp installs it.
	URL string

	tails map[string]*tail
	// pluginAt is when Claude Code was last asked whether it has the plugin.
	pluginAt time.Time
	once     sync.Once
	wake     chan struct{}
}

// woken is signalled when the roster is worth reading before the next tick.
func (a *Adapter) woken() chan struct{} {
	a.once.Do(func() { a.wake = make(chan struct{}, 1) })
	return a.wake
}

// Run polls until ctx ends.
func (a *Adapter) Run(ctx context.Context) {
	a.tails = map[string]*tail{}
	for _, id := range a.Hub.Untraced(Agent) {
		a.trace(id, a.origin(id))
	}
	t := time.NewTicker(a.Interval)
	defer t.Stop()
	for {
		a.checkPlugin(ctx)
		if err := a.poll(ctx); err != nil && ctx.Err() == nil {
			log.Printf("claude: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.woken():
		}
	}
}

type agentRow struct {
	// ID is what Claude Code's own commands call a background session.
	ID         string `json:"id"`
	Cwd        string `json:"cwd"`
	Kind       string `json:"kind"`
	StartedAt  int64  `json:"startedAt"`
	State      string `json:"state"`
	Status     string `json:"status"`
	WaitingFor string `json:"waitingFor"`
	SessionID  string `json:"sessionId"`
	Name       string `json:"name"`
}

// roster lists the sessions Claude Code has open, and with all also the
// background sessions that have stopped.
func (a *Adapter) roster(ctx context.Context, all bool) ([]agentRow, error) {
	args := []string{"agents", "--json"}
	if all {
		args = append(args, "--all")
	}
	out, err := exec.CommandContext(ctx, a.Bin, args...).Output()
	if err != nil {
		return nil, err
	}
	var rows []agentRow
	return rows, json.Unmarshal(out, &rows)
}

func (a *Adapter) poll(ctx context.Context) error {
	rows, err := a.roster(ctx, false)
	if err != nil {
		return err
	}
	live := make([]hub.Session, 0, len(rows))
	for _, r := range rows {
		if r.SessionID == "" {
			continue
		}
		s := hub.Session{
			ID: r.SessionID, Agent: Agent, Kind: r.Kind, Name: r.Name, Cwd: r.Cwd,
			Status: status(r), WaitingFor: r.WaitingFor,
			StartedAt: time.UnixMilli(r.StartedAt), ActiveAt: time.UnixMilli(r.StartedAt),
		}
		if r.Kind == background {
			s.Job = r.ID
		}
		if t := a.tail(r.SessionID); t != nil {
			t.read()
			s.Title, s.Branch, s.LastPrompt, s.LastReply, s.PromptAt = t.title, t.branch, t.lastPrompt, t.lastReply, t.promptAt
			s.Model, s.Reasoning, s.ContextTokens, s.Used, s.Named = t.model, t.reasoning, t.contextTokens, t.used, t.named
			if !t.modTime.IsZero() {
				s.ActiveAt = t.modTime
			}
			if t.origin != "" {
				a.trace(r.SessionID, t.origin)
			}
		}
		live = append(live, s)
	}
	a.Hub.SetRoster(Agent, live)
	return nil
}

// status reads a row that may say how its session stands twice: Status for
// one with a process, State for one in the background, and both for a
// background session whose process is alive.
func status(r agentRow) hub.Status {
	switch {
	case r.Status == "waiting" || r.State == "blocked":
		return hub.Waiting
	case r.Status == "busy" || r.State == "working":
		return hub.Working
	case r.State == "failed":
		return hub.Failed
	case r.State == "done":
		return hub.Done
	}
	return hub.Idle
}

func (a *Adapter) tail(sessionID string) *tail {
	if t, ok := a.tails[sessionID]; ok {
		return t
	}
	path := a.transcript(sessionID)
	if path == "" {
		return nil
	}
	t := &tail{path: path}
	a.tails[sessionID] = t
	return t
}

// transcript returns the path of a session's transcript, or "" when there
// is none or sessionID could not name one.
func (a *Adapter) transcript(sessionID string) string {
	if sessionID == "" || strings.ContainsFunc(sessionID, func(r rune) bool {
		return !(r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
	}) {
		return ""
	}
	m, _ := filepath.Glob(filepath.Join(a.Home, ".claude", "projects", "*", sessionID+".jsonl"))
	if len(m) == 0 {
		return ""
	}
	return m[0]
}

// origin reads the start of a session's transcript for the session that
// began it: another when the transcript continues that one, and the session
// itself otherwise. It is "" when the transcript does not say.
func (a *Adapter) origin(sessionID string) string {
	f, err := os.Open(a.transcript(sessionID))
	if err != nil {
		return ""
	}
	defer f.Close()
	var t tail
	for r := bufio.NewReaderSize(io.LimitReader(f, originRead), 1<<20); t.origin == ""; {
		line, err := r.ReadBytes('\n')
		if err != nil {
			break
		}
		t.apply(line)
	}
	return t.origin
}

// trace tells the hub which session id continues, and likewise of each
// session before it that the hub has heard nothing of.
func (a *Adapter) trace(id, from string) {
	for a.Hub.Link(id, from) {
		id, from = from, a.origin(from)
	}
}

// tail incrementally reads one transcript. The entry format is internal to
// Claude Code, so every field is optional and unknown entries are skipped.
type tail struct {
	path    string
	offset  int64
	modTime time.Time

	title, branch, lastPrompt, lastReply string
	// origin is the session the transcript's first entry was written by,
	// which is another session when this one continues it.
	origin string
	// named is the name the user gave the session with /rename, if any.
	named string
	// promptAt is when lastPrompt was said, zero when the file does not say.
	promptAt         time.Time
	model, reasoning string
	// contextTokens is zero from a compaction until the next response.
	contextTokens int
	// used totals every response in the file, subagents' included. counted
	// holds each response's share, as one is written once per block.
	used    hub.Tokens
	counted map[string]hub.Tokens
}

type entry struct {
	Type        string `json:"type"`
	AITitle     string `json:"aiTitle"`
	CustomTitle string `json:"customTitle"`
	LastPrompt  string `json:"lastPrompt"`
	GitBranch   string `json:"gitBranch"`
	IsSidechain bool   `json:"isSidechain"`
	IsMeta      bool   `json:"isMeta"`
	// IsSummary marks the account of an earlier conversation that Claude
	// Code writes in the user's place when it compacts one.
	IsSummary bool   `json:"isCompactSummary"`
	Subtype   string `json:"subtype"`
	Timestamp string `json:"timestamp"`
	Effort    string `json:"effort"`
	// WrittenBy is the session that wrote the entry, which an entry keeps
	// when it is copied or carried into another session's transcript.
	WrittenBy string `json:"session_id"`
	Message   struct {
		Content json.RawMessage `json:"content"`
		ID      string          `json:"id"`
		Model   string          `json:"model"`
		Usage   struct {
			Input      int `json:"input_tokens"`
			CacheWrite int `json:"cache_creation_input_tokens"`
			CacheRead  int `json:"cache_read_input_tokens"`
			Output     int `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

var wanted = [][]byte{
	[]byte(`"type":"ai-title"`), []byte(`"type":"custom-title"`), []byte(`"type":"last-prompt"`),
	[]byte(`"type":"assistant"`), []byte(`"type":"user"`), []byte(`"subtype":"compact_boundary"`),
}

func (t *tail) read() {
	fi, err := os.Stat(t.path)
	if err != nil || (fi.Size() == t.offset && fi.ModTime().Equal(t.modTime)) {
		return
	}
	t.modTime = fi.ModTime()
	if fi.Size() < t.offset {
		t.offset = 0
	}
	f, err := os.Open(t.path)
	if err != nil {
		return
	}
	defer f.Close()

	skipPartial := false
	if t.offset == 0 && fi.Size() > maxFirstRead {
		t.offset, skipPartial = fi.Size()-maxFirstRead, true
	}
	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return
	}
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return // a trailing partial line is left for the next read
		}
		t.offset += int64(len(line))
		if skipPartial {
			skipPartial = false
			continue
		}
		t.apply(line)
	}
}

func (t *tail) apply(line []byte) {
	match := false
	for _, w := range wanted {
		if bytes.Contains(line, w) {
			match = true
			break
		}
	}
	if !match {
		return
	}
	var e entry
	if json.Unmarshal(line, &e) != nil {
		return
	}
	if e.Type == "assistant" && e.Message.ID != "" {
		u := e.Message.Usage
		t.count(e.Message.ID, hub.Tokens{In: u.Input, CacheWrite: u.CacheWrite, CacheRead: u.CacheRead, Out: u.Output})
	}
	if e.IsSidechain {
		return
	}
	if t.origin == "" {
		t.origin = e.WrittenBy
	}
	if e.GitBranch != "" {
		t.branch = e.GitBranch
	}
	switch e.Type {
	case "ai-title":
		if e.AITitle != "" {
			t.title = e.AITitle
		}
	case "custom-title":
		t.named = strings.TrimSpace(e.CustomTitle)
	case "last-prompt":
		// Claude Code's own note of the prompt is cut short and untimed, so
		// it stands in only for a prompt that was not read from its entry.
		if said := head(e.LastPrompt, promptLimit); said != "" && !cutFrom(said, t.lastPrompt) {
			t.lastPrompt, t.promptAt = said, time.Time{}
		}
	case "user":
		if text := e.said(); text != "" {
			t.lastPrompt = head(text, promptLimit)
			t.promptAt, _ = time.Parse(time.RFC3339, e.Timestamp)
		}
	case "assistant":
		if text := assistantText(e.Message.Content); text != "" {
			t.lastReply = Tail(text, ReplyLimit)
		}
		// A notice the harness wrote in the model's place reports no tokens.
		if u := e.Message.Usage; u.Input+u.CacheWrite+u.CacheRead > 0 {
			t.model, t.reasoning, t.contextTokens = e.Message.Model, e.Effort, u.Input+u.CacheWrite+u.CacheRead
		}
	case "system":
		if e.Subtype == "compact_boundary" {
			t.contextTokens = 0
		}
	}
}

// count makes now the share of the response id in the total, in place of
// whatever an earlier block of it reported.
func (t *tail) count(id string, now hub.Tokens) {
	if t.counted == nil {
		t.counted = map[string]hub.Tokens{}
	}
	was := t.counted[id]
	t.counted[id] = now
	t.used.In += now.In - was.In
	t.used.CacheWrite += now.CacheWrite - was.CacheWrite
	t.used.CacheRead += now.CacheRead - was.CacheRead
	t.used.Out += now.Out - was.Out
}

// said returns what the user typed, when the entry is the user typing.
func (e entry) said() string {
	if e.Type != "user" || e.IsMeta || e.IsSummary || e.IsSidechain {
		return ""
	}
	return typed(e.Message.Content)
}

// Exchanges reads the whole of a session's transcript and returns the
// latest things the user said in it, oldest first, each with the end of
// what the agent last answered before the user spoke again. At most limit
// are returned.
func (a *Adapter) Exchanges(sessionID string, limit int) ([]hub.Exchange, error) {
	path := a.transcript(sessionID)
	if path == "" {
		return nil, fmt.Errorf("no transcript was found for the session %q", sessionID)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []hub.Exchange
	for r := bufio.NewReaderSize(f, 1<<20); ; {
		line, err := r.ReadBytes('\n')
		if err != nil {
			break
		}
		if !bytes.Contains(line, []byte(`"type":"assistant"`)) && !bytes.Contains(line, []byte(`"type":"user"`)) {
			continue
		}
		var e entry
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		if text := e.said(); text != "" {
			at, _ := time.Parse(time.RFC3339, e.Timestamp)
			out = append(out, hub.Exchange{At: at, Prompt: head(text, promptLimit)})
		} else if e.Type == "assistant" && !e.IsSidechain && len(out) > 0 {
			if text := assistantText(e.Message.Content); text != "" {
				out[len(out)-1].Reply = Tail(text, exchangeReply)
			}
		}
	}
	return out[max(0, len(out)-limit):], nil
}

// typed returns what the user typed in a user entry: its text, with or
// without attachments. Tool results, command echoes and the harness's own
// notices are not typed, and give "".
func typed(content json.RawMessage) string {
	var text string
	if json.Unmarshal(content, &text) != nil {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(content, &blocks) != nil {
			return ""
		}
		var parts []string
		for _, b := range blocks {
			if b.Type == "tool_result" {
				return ""
			}
			if b.Type == "text" {
				parts = append(parts, b.Text)
			}
		}
		text = strings.Join(parts, "\n\n")
	}
	if text = strings.TrimSpace(text); strings.HasPrefix(text, "<") || strings.HasPrefix(text, "[Request interrupted") {
		return ""
	}
	return text
}

// cutFrom reports whether note is whole, give or take its spacing, or is
// its beginning with a mark where it was cut.
func cutFrom(note, whole string) bool {
	squeeze := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	note, whole = squeeze(note), squeeze(whole)
	return whole != "" && (note == whole || strings.HasSuffix(note, "…") && strings.HasPrefix(whole, strings.TrimSpace(strings.TrimSuffix(note, "…"))))
}

func assistantText(content json.RawMessage) string {
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, strings.TrimSpace(b.Text))
		}
	}
	return strings.Join(parts, "\n\n")
}

func head(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}

// Tail returns the last n runes of s, marked when it was cut.
func Tail(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return "…" + string(r[len(r)-n:])
}
