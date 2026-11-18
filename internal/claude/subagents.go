package claude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jefflinse/slorp/internal/hub"
)

const (
	// handback is the tool a subagent reports back to its session with.
	handback = "SubagentHandback"
	// reportLimit is how much of a subagent's task and of its report is
	// kept, in runes.
	reportLimit = 60000
)

// subagent incrementally reads the transcript of one agent a session
// started. Claude Code keeps each beside the session's own transcript, with
// a note of what it was started as. Both formats are internal to Claude
// Code, so every field is optional.
type subagent struct {
	id, path string
	offset   int64
	modTime  time.Time

	started, active time.Time
	model, effort   string
	tools           int
	tokens          tail
	// prompt is the first thing it was told, and report what it handed
	// back, or else the last thing it said.
	prompt, report string
	handed         bool
}

// note is what Claude Code writes down about a subagent as it starts it.
type note struct {
	Type        string `json:"agentType"`
	Description string `json:"description"`
	Depth       int    `json:"spawnDepth"`
	Shape       string `json:"requestShape"`
	Model       string `json:"model"`
	Finished    bool   `json:"finished"`
}

// subagentsDir returns where the subagents of the session with the
// transcript at path are kept.
func subagentsDir(transcript string) string {
	return filepath.Join(strings.TrimSuffix(transcript, ".jsonl"), "subagents")
}

// subagentID returns the id a note's file name carries.
func subagentID(notePath string) string {
	return strings.TrimSuffix(strings.TrimPrefix(filepath.Base(notePath), "agent-"), ".meta.json")
}

// subagents lists the agents the session with the transcript at path has
// started, the first started first. known holds the readers of those seen
// before, so that only what is new of each is read; nil reads each afresh.
func subagents(transcript string, known map[string]*subagent) []hub.Subagent {
	notes, _ := filepath.Glob(filepath.Join(subagentsDir(transcript), "agent-*.meta.json"))
	out := make([]hub.Subagent, 0, len(notes))
	for _, path := range notes {
		id := subagentID(path)
		sub := known[id]
		if sub == nil {
			sub = &subagent{id: id, path: strings.TrimSuffix(path, ".meta.json") + ".jsonl"}
			if known != nil {
				known[id] = sub
			}
		}
		sub.read()
		var n note
		if raw, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(raw, &n)
		}
		out = append(out, hub.Subagent{
			ID: id, Type: n.Type, Description: n.Description, Depth: n.Depth, Background: n.Shape == "background", Done: n.Finished,
			StartedAt: sub.started, ActiveAt: sub.active, Model: cmpOr(sub.model, n.Model), Effort: sub.effort, Tools: sub.tools, Used: sub.tokens.used,
		})
	}
	slices.SortStableFunc(out, func(a, b hub.Subagent) int { return a.StartedAt.Compare(b.StartedAt) })
	return out
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// read takes in what has been written to the transcript since it was last
// read.
func (s *subagent) read() {
	fi, err := os.Stat(s.path)
	if err != nil || (fi.Size() == s.offset && fi.ModTime().Equal(s.modTime)) {
		return
	}
	s.modTime = fi.ModTime()
	if fi.Size() < s.offset {
		*s = subagent{id: s.id, path: s.path, modTime: s.modTime}
	}
	f, err := os.Open(s.path)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Seek(s.offset, io.SeekStart); err != nil {
		return
	}
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return // a trailing partial line is left for the next read
		}
		s.offset += int64(len(line))
		s.apply(line)
	}
}

func (s *subagent) apply(line []byte) {
	if !bytes.Contains(line, []byte(`"type":"assistant"`)) && !bytes.Contains(line, []byte(`"type":"user"`)) {
		return
	}
	var e entry
	if json.Unmarshal(line, &e) != nil {
		return
	}
	if at, err := time.Parse(time.RFC3339, e.Timestamp); err == nil {
		if s.started.IsZero() {
			s.started = at
		}
		s.active = at
	}
	switch e.Type {
	case "user":
		if s.prompt == "" {
			s.prompt = head(typed(e.Message.Content), reportLimit)
		}
	case "assistant":
		if e.Message.ID != "" {
			u := e.Message.Usage
			s.tokens.count(e.Message.ID, hub.Tokens{In: u.Input, CacheWrite: u.CacheWrite, CacheRead: u.CacheRead, Out: u.Output})
		}
		if u := e.Message.Usage; u.Input+u.CacheWrite+u.CacheRead > 0 {
			s.model, s.effort = e.Message.Model, e.Effort
		}
		var blocks []struct {
			Type  string `json:"type"`
			Name  string `json:"name"`
			Input struct {
				Message string `json:"message"`
			} `json:"input"`
		}
		_ = json.Unmarshal(e.Message.Content, &blocks)
		for _, b := range blocks {
			if b.Type != "tool_use" {
				continue
			}
			s.tools++
			if b.Name == handback && strings.TrimSpace(b.Input.Message) != "" {
				s.report, s.handed = head(b.Input.Message, reportLimit), true
			}
		}
		if text := assistantText(e.Message.Content); text != "" && !s.handed {
			s.report = head(text, reportLimit)
		}
	}
}

// Subagents lists the agents a session has started, the first started
// first, from Claude Code's own record of them. It reads each afresh, so it
// serves a session that has closed as well as one that is open.
func (a *Adapter) Subagents(sessionID string) []hub.Subagent {
	path := a.transcript(sessionID)
	if path == "" {
		return nil
	}
	return subagents(path, nil)
}

// Report returns what one of a session's subagents was asked and what it
// reported back, and false when the session started no such agent.
func (a *Adapter) Report(sessionID, id string) (hub.SubagentReport, bool) {
	path := a.transcript(sessionID)
	if path == "" || id == "" || strings.ContainsFunc(id, func(r rune) bool {
		return !(r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
	}) {
		return hub.SubagentReport{}, false
	}
	sub := &subagent{id: id, path: filepath.Join(subagentsDir(path), "agent-"+id+".jsonl")}
	if _, err := os.Stat(sub.path); err != nil {
		return hub.SubagentReport{}, false
	}
	sub.read()
	return hub.SubagentReport{Prompt: sub.prompt, Report: sub.report, Handed: sub.handed}, true
}
