package server

import (
	"time"

	"github.com/jefflinse/slorp/internal/claude"
	"github.com/jefflinse/slorp/internal/hub"
)

// bridgeSnapshot is a plugin's current view, repeated on polls so missed
// events and service restarts do not leave a session in an old state.
type bridgeSnapshot struct {
	Status        hub.Status      `json:"status"`
	WaitingFor    string          `json:"waitingFor"`
	StartedAt     int64           `json:"startedAt"`
	ActiveAt      int64           `json:"activeAt"`
	PromptAt      int64           `json:"promptAt"`
	LastPrompt    string          `json:"lastPrompt"`
	LastReply     string          `json:"lastReply"`
	Model         string          `json:"model"`
	Reasoning     string          `json:"reasoning"`
	ContextTokens int             `json:"contextTokens"`
	ContextWindow int             `json:"contextWindow"`
	Used          *hub.Tokens     `json:"used"`
	CostUSD       *float64        `json:"costUsd"`
	Compacted     *hub.Compaction `json:"compacted"`
}

func (b *bridgeSnapshot) apply(s *hub.Session) {
	if b == nil {
		return
	}
	if s.Pending == nil {
		switch b.Status {
		case hub.Working, hub.Idle, hub.Failed:
			s.Status, s.WaitingFor = b.Status, first(b.WaitingFor, doingLimit)
		}
	}
	if b.StartedAt > 0 {
		s.StartedAt = time.UnixMilli(b.StartedAt)
	}
	if b.ActiveAt > 0 {
		s.ActiveAt = time.UnixMilli(b.ActiveAt)
	}
	if b.PromptAt > 0 && !time.UnixMilli(b.PromptAt).Before(s.PromptAt) {
		s.PromptAt, s.LastPrompt = time.UnixMilli(b.PromptAt), first(b.LastPrompt, 8000)
	}
	s.LastReply = claude.Tail(b.LastReply, claude.ReplyLimit)
	s.Model, s.Reasoning = first(b.Model, 200), first(b.Reasoning, nameLimit)
	s.ContextTokens, s.ContextWindow = max(0, b.ContextTokens), max(0, b.ContextWindow)
	if b.Used != nil {
		s.Used = hub.Tokens{In: max(0, b.Used.In), Out: max(0, b.Used.Out), CacheRead: max(0, b.Used.CacheRead), CacheWrite: max(0, b.Used.CacheWrite)}
	}
	if b.CostUSD != nil {
		s.CostUSD = max(0, *b.CostUSD)
	}
	if b.Compacted != nil && !b.Compacted.At.IsZero() {
		s.Compacted = b.Compacted
	}
}
