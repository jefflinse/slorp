package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
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

var sessionIDPattern = regexp.MustCompile(`^ses_[A-Za-z0-9_-]{1,128}$`)

type historyMessage struct {
	Info struct {
		ID        string                             `json:"id"`
		SessionID string                             `json:"sessionID"`
		ParentID  string                             `json:"parentID"`
		Role      string                             `json:"role"`
		Summary   json.RawMessage                    `json:"summary"`
		Error     json.RawMessage                    `json:"error"`
		Time      struct{ Created, Completed int64 } `json:"time"`
	} `json:"info"`
	Parts []historyPart `json:"parts"`
}

type historyPart struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Text      string `json:"text"`
	Filename  string `json:"filename"`
	Mime      string `json:"mime"`
	URL       string `json:"url"`
	Synthetic bool   `json:"synthetic"`
	Ignored   bool   `json:"ignored"`
	Auto      bool   `json:"auto"`
	Overflow  bool   `json:"overflow"`
	canonical map[string]any
}

func (m *historyMessage) UnmarshalJSON(b []byte) error {
	type message historyMessage
	if err := json.Unmarshal(b, (*message)(m)); err != nil {
		return err
	}
	if m.Info.Role != "user" {
		return nil
	}
	var user struct {
		Parts []map[string]any `json:"parts"`
	}
	if err := json.Unmarshal(b, &user); err != nil {
		return err
	}
	for i, p := range user.Parts {
		delete(p, "id")
		delete(p, "messageID")
		delete(p, "sessionID")
		m.Parts[i].canonical = p
	}
	return nil
}

type historyExport struct {
	Info struct {
		ID     string `json:"id"`
		Revert *struct {
			MessageID string `json:"messageID"`
			PartID    string `json:"partID"`
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
	cmd := a.command(ctx, "export", "--pure", sessionID)
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
	slices.SortStableFunc(messages, func(a, b historyMessage) int {
		return time.UnixMilli(a.Info.Time.Created).Compare(time.UnixMilli(b.Info.Time.Created))
	})
	if export.Info.Revert != nil {
		at := slices.IndexFunc(messages, func(m historyMessage) bool { return m.Info.ID == export.Info.Revert.MessageID })
		if at < 0 {
			return nil, errors.New("OpenCode export is missing its revert boundary")
		}
		if export.Info.Revert.PartID == "" {
			messages = messages[:at]
		} else {
			part := slices.IndexFunc(messages[at].Parts, func(p historyPart) bool { return p.ID == export.Info.Revert.PartID })
			if part < 0 {
				return nil, errors.New("OpenCode export is missing its revert part")
			}
			messages[at].Parts = messages[at].Parts[:part]
			messages = messages[:at+1]
		}
	}
	var out []hub.Exchange
	parents := map[string]int{}
	type replay struct {
		parts []historyPart
		index int
	}
	type compaction struct {
		marker hub.Compaction
		replay *replay
	}
	compactions := map[string]compaction{}
	var previous []historyPart
	var pendingReplay *replay
	seen := map[string]bool{}
	last := -1
	for _, m := range messages {
		i := m.Info
		if i.SessionID != sessionID || i.ID == "" || i.Time.Created <= 0 || seen[i.ID] {
			continue
		}
		seen[i.ID] = true
		if i.Role != "user" {
			pendingReplay = nil
		}
		var text, files []string
		synthetic := false
		for _, part := range m.Parts {
			if part.Type == "text" && part.Synthetic {
				synthetic = true
			}
			if part.Ignored || part.Synthetic {
				continue
			}
			switch part.Type {
			case "text":
				if t := strings.TrimSpace(part.Text); t != "" {
					text = append(text, t)
				}
			case "file":
				name := strings.TrimSpace(part.Filename)
				if name == "" {
					name = "attachment"
				}
				files = append(files, "[Attached: "+name+"]")
			case "compaction":
				if i.Role == "user" {
					c := compaction{marker: hub.Compaction{ID: part.ID, Auto: part.Auto}}
					if part.Auto && part.Overflow && last >= 0 {
						c.replay = &replay{parts: previous, index: last}
					}
					compactions[i.ID] = c
				}
			}
		}
		said := strings.Join(text, "\n\n")
		switch i.Role {
		case "user":
			candidate := pendingReplay
			pendingReplay = nil
			if _, compacting := compactions[i.ID]; compacting {
				continue
			}
			previous = m.Parts
			if candidate != nil && isReplay(candidate.parts, m.Parts) {
				last = candidate.index
				parents[i.ID] = last
				if said == "" {
					said = strings.Join(files, "\n")
				}
				out[last].Aliases = append(out[last].Aliases, hub.PromptAlias{At: time.UnixMilli(i.Time.Created), Prompt: said})
				continue
			}
			if said == "" && len(files) > 0 {
				said = strings.Join(files, "\n")
			}
			if said == "" {
				if synthetic && last >= 0 {
					parents[i.ID] = last
				} else {
					last = -1
				}
				continue
			}
			last = len(out)
			parents[i.ID] = last
			out = append(out, hub.Exchange{At: time.UnixMilli(i.Time.Created), Prompt: clipped(said, historyPrompt, false)})
		case "assistant":
			if i.Time.Completed <= 0 {
				continue
			}
			if string(i.Summary) == "true" {
				if c, ok := compactions[i.ParentID]; ok && (len(i.Error) == 0 || string(i.Error) == "null") {
					c.marker.At = time.UnixMilli(i.Time.Completed)
					out = append(out, hub.Exchange{At: c.marker.At, Compacted: &c.marker})
					pendingReplay = c.replay
					delete(compactions, i.ParentID)
				}
				continue
			}
			if at, ok := parents[i.ParentID]; ok && said != "" {
				out[at].Reply = clipped(said, historyReply, true)
			}
		}
	}
	slices.SortStableFunc(out, func(a, b hub.Exchange) int { return a.At.Compare(b.At) })
	return out[max(0, len(out)-max(0, limit)):], nil
}

// isReplay recognizes the copied user parts immediately following an automatic
// overflow compaction, including media replaced with attachment descriptions.
func isReplay(original, copied []historyPart) bool {
	if len(original) == 0 || len(original) != len(copied) {
		return false
	}
	for i, a := range original {
		b := copied[i]
		expected := a.canonical
		if a.Type == "file" && b.Type == "text" {
			name := a.Filename
			if name == "" {
				name = "file"
			}
			expected = map[string]any{"type": "text", "text": "[Attached " + a.Mime + ": " + name + "]"}
		}
		if !reflect.DeepEqual(expected, b.canonical) {
			return false
		}
	}
	return true
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
