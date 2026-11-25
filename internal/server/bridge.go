package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jefflinse/slorp/internal/claude"
	"github.com/jefflinse/slorp/internal/hub"
)

func bridgeFenced(h *hub.Hub, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/bridge/") {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			http.Error(w, "bad request body", http.StatusBadRequest)
			return
		}
		var in struct {
			Agent     string `json:"agent"`
			SessionID string `json:"sessionId"`
			ID        string `json:"id"`
		}
		if json.Unmarshal(body, &in) != nil {
			http.Error(w, "bad request body", http.StatusBadRequest)
			return
		}
		ctx, release, err := h.BridgeLease(r.Context(), r.Header.Get("X-Slorp-Run"), in.Agent, in.SessionID, in.ID, r.URL.Path)
		if err != nil {
			http.Error(w, "unknown, stopped, or mismatched managed run", http.StatusConflict)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		// Network backpressure must not keep a managed mutation lease alive.
		response := &bridgeResponse{header: make(http.Header)}
		cancelled := false
		func() {
			defer release()
			next.ServeHTTP(response, r.WithContext(ctx))
			cancelled = ctx.Err() != nil
		}()
		if cancelled {
			http.Error(w, "managed request cancelled", http.StatusConflict)
			return
		}
		for key, values := range response.header {
			w.Header()[key] = values
		}
		if response.status == 0 {
			response.status = http.StatusOK
		}
		w.WriteHeader(response.status)
		_, _ = w.Write(response.body.Bytes())
	})
}

type bridgeResponse struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (w *bridgeResponse) Header() http.Header { return w.header }
func (w *bridgeResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *bridgeResponse) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(b)
}

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
	// Said is what the agent has said in answer to LastPrompt, oldest first,
	// and SaidEarlier how many more it said before those. A plugin that
	// does not tell of them leaves both out.
	Said        []string `json:"said"`
	SaidEarlier int      `json:"saidEarlier"`
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
	var dropped int
	s.Said, dropped = claude.Kept(b.Said)
	s.SaidEarlier = max(0, b.SaidEarlier) + dropped
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
