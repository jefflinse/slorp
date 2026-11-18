// Package claude observes Claude Code sessions on this machine: the live
// roster from `claude agents --json`, and each session's title and last
// exchange from its transcript file.
package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jefflinse/slorp/internal/hub"
)

const (
	Agent = "claude"

	// ReplyLimit is how much of a session's last message is kept, in runes.
	ReplyLimit = 30000

	maxFirstRead = 64 << 20
	promptLimit  = 8000
)

type Adapter struct {
	Hub      *hub.Hub
	Bin      string
	Home     string
	Interval time.Duration

	tails map[string]*tail
}

// Run polls until ctx ends.
func (a *Adapter) Run(ctx context.Context) {
	a.tails = map[string]*tail{}
	t := time.NewTicker(a.Interval)
	defer t.Stop()
	for {
		if err := a.poll(ctx); err != nil && ctx.Err() == nil {
			log.Printf("claude: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

type agentRow struct {
	Cwd        string `json:"cwd"`
	Kind       string `json:"kind"`
	StartedAt  int64  `json:"startedAt"`
	State      string `json:"state"`
	Status     string `json:"status"`
	WaitingFor string `json:"waitingFor"`
	SessionID  string `json:"sessionId"`
	Name       string `json:"name"`
}

func (a *Adapter) poll(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, a.Bin, "agents", "--json").Output()
	if err != nil {
		return err
	}
	var rows []agentRow
	if err := json.Unmarshal(out, &rows); err != nil {
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
		if t := a.tail(r.SessionID); t != nil {
			t.read()
			s.Title, s.Branch, s.LastPrompt, s.LastReply, s.PromptAt = t.title, t.branch, t.lastPrompt, t.lastReply, t.promptAt
			s.Model, s.Reasoning, s.ContextTokens, s.Used = t.model, t.reasoning, t.contextTokens, t.used
			if !t.modTime.IsZero() {
				s.ActiveAt = t.modTime
			}
		}
		live = append(live, s)
	}
	a.Hub.SetRoster(Agent, live)
	return nil
}

func status(r agentRow) hub.Status {
	switch r.Status + r.State {
	case "busy", "working":
		return hub.Working
	case "waiting", "blocked":
		return hub.Waiting
	case "done":
		return hub.Done
	case "failed":
		return hub.Failed
	}
	return hub.Idle
}

func (a *Adapter) tail(sessionID string) *tail {
	if t, ok := a.tails[sessionID]; ok {
		return t
	}
	m, _ := filepath.Glob(filepath.Join(a.Home, ".claude", "projects", "*", sessionID+".jsonl"))
	if len(m) == 0 {
		return nil
	}
	t := &tail{path: m[0]}
	a.tails[sessionID] = t
	return t
}

// tail incrementally reads one transcript. The entry format is internal to
// Claude Code, so every field is optional and unknown entries are skipped.
type tail struct {
	path    string
	offset  int64
	modTime time.Time

	title, branch, lastPrompt, lastReply string
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
	LastPrompt  string `json:"lastPrompt"`
	GitBranch   string `json:"gitBranch"`
	IsSidechain bool   `json:"isSidechain"`
	IsMeta      bool   `json:"isMeta"`
	Subtype     string `json:"subtype"`
	Timestamp   string `json:"timestamp"`
	Effort      string `json:"effort"`
	Message     struct {
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
	[]byte(`"type":"ai-title"`), []byte(`"type":"last-prompt"`),
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
	if e.GitBranch != "" {
		t.branch = e.GitBranch
	}
	switch e.Type {
	case "ai-title":
		if e.AITitle != "" {
			t.title = e.AITitle
		}
	case "last-prompt":
		// Claude Code's own note of the prompt is cut short and untimed, so
		// it stands in only for a prompt that was not read from its entry.
		if said := head(e.LastPrompt, promptLimit); said != "" && !cutFrom(said, t.lastPrompt) {
			t.lastPrompt, t.promptAt = said, time.Time{}
		}
	case "user":
		if text := typed(e.Message.Content); text != "" && !e.IsMeta {
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
