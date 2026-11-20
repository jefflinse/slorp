package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jefflinse/slorp/internal/hub"
)

const (
	historyTimeout = 30 * time.Second
	historyBytes   = 64 << 20
	historyPrompt  = 8000
	historyReply   = 2000
)

var sessionIDPattern = regexp.MustCompile(`^ses[A-Za-z0-9_-]{1,128}$`)

type historyMessage struct {
	ID      string                             `json:"id"`
	Type    string                             `json:"type"`
	Time    struct{ Created, Completed int64 } `json:"time"`
	Text    string                             `json:"text"`
	Status  string                             `json:"status"`
	Reason  string                             `json:"reason"`
	Content []historyContent                   `json:"content"`
	Files   []struct{ Name string }            `json:"files"`
}

type historyContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type historyExport struct {
	Info struct {
		ID     string `json:"id"`
		Revert *struct {
			MessageID string `json:"messageID"`
		} `json:"revert"`
	} `json:"info"`
	Messages *[]historyMessage `json:"messages"`
}

// Exchanges exports a local session into a private temporary file and returns
// its latest exchanges and completed compactions, oldest first. Reading is
// cancelled with ctx and bounded by historyTimeout and historyBytes.
func (a *Adapter) Exchanges(ctx context.Context, sessionID string, limit int) ([]hub.Exchange, error) {
	if !sessionIDPattern.MatchString(sessionID) {
		return nil, errors.New("invalid OpenCode session id")
	}
	if limit <= 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, historyTimeout)
	defer cancel()
	f, err := os.CreateTemp("", "slorp-opencode-history-*.json")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	cmd := a.command(ctx, "session", "export", sessionID, "--standalone")
	replaceEnv(cmd, "OPENCODE_PURE", "true")
	cmd.Stdout = f
	stderr := &diagnosticBuffer{}
	cmd.Stderr = stderr
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("OpenCode history: %w", ctx.Err())
		}
		return nil, fmt.Errorf("OpenCode export failed: %w: %s", err, strings.TrimSpace(string(stderr.data)))
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > historyBytes {
		return nil, errors.New("OpenCode export exceeds the 64 MiB history limit")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	xs, err := readExchanges(&historyReader{ctx: ctx, Reader: f}, sessionID, min(limit, hub.BackfillLimit))
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return xs, err
}

type historyReader struct {
	io.Reader
	ctx context.Context
}

func (r *historyReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

type diagnosticBuffer struct{ data []byte }

func (b *diagnosticBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p[:min(len(p), 8192-len(b.data))]...)
	return len(p), nil
}

func readExchanges(r io.Reader, sessionID string, limit int) ([]hub.Exchange, error) {
	var export historyExport
	d := json.NewDecoder(r)
	if err := d.Decode(&export); err != nil {
		return nil, fmt.Errorf("invalid OpenCode export: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, errors.New("unexpected content after OpenCode export")
	}
	if export.Info.ID != sessionID || export.Messages == nil {
		return nil, errors.New("OpenCode export does not contain the requested session")
	}
	messages := *export.Messages
	if export.Info.Revert != nil {
		at := slices.IndexFunc(messages, func(m historyMessage) bool { return m.ID == export.Info.Revert.MessageID })
		if at < 0 {
			return nil, errors.New("OpenCode export is missing its revert boundary")
		}
		messages = messages[:at]
	}
	var out []hub.Exchange
	seen := map[string]bool{}
	last := -1
	// The projected transcript order is authoritative. V2 assistant messages have
	// no parent ID; checkpoints and synthetic continuations retain the active turn.
	for _, m := range messages {
		if m.ID == "" || m.Time.Created <= 0 || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		switch m.Type {
		case "user":
			said := strings.TrimSpace(m.Text)
			var files []string
			for _, file := range m.Files {
				name := strings.TrimSpace(file.Name)
				if name == "" {
					name = "attachment"
				}
				files = append(files, "[Attached: "+name+"]")
			}
			if said == "" {
				said = strings.Join(files, "\n")
			}
			if said == "" {
				last = -1
				continue
			}
			last = len(out)
			out = append(out, hub.Exchange{At: time.UnixMilli(m.Time.Created), Prompt: clipped(said, historyPrompt, false)})
		case "assistant":
			if last < 0 || m.Time.Completed <= 0 {
				continue
			}
			var text []string
			for _, content := range m.Content {
				if content.Type == "text" {
					if t := strings.TrimSpace(content.Text); t != "" {
						text = append(text, t)
					}
				}
			}
			if said := strings.Join(text, "\n\n"); said != "" {
				out[last].Reply = clipped(said, historyReply, true)
			}
		case "compaction":
			if m.Status == "completed" {
				c := &hub.Compaction{ID: m.ID, At: time.UnixMilli(m.Time.Created), Auto: m.Reason == "auto"}
				out = append(out, hub.Exchange{At: c.At, Compacted: c})
			}
		}
	}
	slices.SortStableFunc(out, func(a, b hub.Exchange) int { return a.At.Compare(b.At) })
	return out[max(0, len(out)-max(0, min(limit, hub.BackfillLimit))):], nil
}

func clipped(text string, n int, tail bool) string {
	runes := []rune(text)
	if len(runes) <= n {
		return text
	}
	if tail {
		return "…" + string(runes[len(runes)-n:])
	}
	return string(runes[:n]) + "…"
}
