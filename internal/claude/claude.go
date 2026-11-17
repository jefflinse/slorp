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
			s.Title, s.Branch, s.LastPrompt, s.LastReply = t.title, t.branch, t.lastPrompt, t.lastReply
			s.Model, s.Reasoning, s.ContextTokens = t.model, t.reasoning, t.contextTokens
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
	model, reasoning                     string
	// contextTokens is zero from a compaction until the next response.
	contextTokens int
}

type entry struct {
	Type        string `json:"type"`
	AITitle     string `json:"aiTitle"`
	LastPrompt  string `json:"lastPrompt"`
	GitBranch   string `json:"gitBranch"`
	IsSidechain bool   `json:"isSidechain"`
	IsMeta      bool   `json:"isMeta"`
	Subtype     string `json:"subtype"`
	Effort      string `json:"effort"`
	Message     struct {
		Content json.RawMessage `json:"content"`
		Model   string          `json:"model"`
		Usage   struct {
			Input      int `json:"input_tokens"`
			CacheWrite int `json:"cache_creation_input_tokens"`
			CacheRead  int `json:"cache_read_input_tokens"`
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
	if json.Unmarshal(line, &e) != nil || e.IsSidechain {
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
		if e.LastPrompt != "" {
			t.lastPrompt = head(e.LastPrompt, promptLimit)
		}
	case "user":
		// Typed prompts carry a plain string; tool results and attachments do not.
		var text string
		if !e.IsMeta && json.Unmarshal(e.Message.Content, &text) == nil && !strings.HasPrefix(strings.TrimSpace(text), "<") {
			t.lastPrompt = head(text, promptLimit)
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
