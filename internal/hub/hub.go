// Package hub holds Slorp's in-memory view of every agent session and the
// outbox of replies waiting to be delivered into them.
package hub

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sort"
	"sync"
	"time"
)

type Status string

const (
	Working Status = "working"
	Waiting Status = "waiting"
	Idle    Status = "idle"
	Done    Status = "done"
	Failed  Status = "failed"
)

// bridgeTTL is how long after its last poll a session's bridge still counts as connected.
const bridgeTTL = 10 * time.Second

// Session is one agent conversation, normalized across agents.
type Session struct {
	ID         string    `json:"id"`
	Agent      string    `json:"agent"`
	Kind       string    `json:"kind"`
	Name       string    `json:"name"`
	Title      string    `json:"title"`
	Cwd        string    `json:"cwd"`
	Branch     string    `json:"branch"`
	Status     Status    `json:"status"`
	WaitingFor string    `json:"waitingFor,omitempty"`
	StartedAt  time.Time `json:"startedAt"`
	ActiveAt   time.Time `json:"activeAt"`
	LastPrompt string    `json:"lastPrompt"`
	LastReply  string    `json:"lastReply"`
	Pending    *Pending  `json:"pending,omitempty"`
	CanReply   bool      `json:"canReply"`
	Replies    []Reply   `json:"replies,omitempty"`

	bridgeSeen time.Time
}

// Pending is a permission request a session is blocked on.
type Pending struct {
	Tool    string    `json:"tool"`
	Summary string    `json:"summary"`
	At      time.Time `json:"at"`
}

type ReplyState string

const (
	Queued    ReplyState = "queued"
	Claimed   ReplyState = "claimed"
	Delivered ReplyState = "delivered"
	Dropped   ReplyState = "dropped"
)

// Reply is text the user wrote in Slorp for delivery into a session.
type Reply struct {
	ID        string     `json:"id"`
	SessionID string     `json:"sessionId"`
	Text      string     `json:"text"`
	State     ReplyState `json:"state"`
	CreatedAt time.Time  `json:"createdAt"`
}

type Hub struct {
	mu       sync.Mutex
	sessions map[string]*Session
	replies  []*Reply
	subs     map[chan struct{}]struct{}
	last     string
}

func New() *Hub {
	return &Hub{sessions: map[string]*Session{}, subs: map[chan struct{}]struct{}{}}
}

// SetRoster replaces the known sessions of one agent with the live set an
// adapter observed. Fields only a bridge can report survive the replacement.
func (h *Hub) SetRoster(agent string, live []Session) {
	h.mu.Lock()
	defer h.mu.Unlock()
	seen := map[string]bool{}
	for i := range live {
		s := live[i]
		seen[s.ID] = true
		if old, ok := h.sessions[s.ID]; ok {
			s.bridgeSeen = old.bridgeSeen
			if s.Status != Idle {
				s.Pending = old.Pending
			}
			if s.Pending != nil && s.Status == Working {
				s.Status, s.WaitingFor = Waiting, "permission prompt"
			}
			if s.LastReply == "" {
				s.LastReply = old.LastReply
			}
			if s.LastPrompt == "" {
				s.LastPrompt = old.LastPrompt
			}
		}
		h.sessions[s.ID] = &s
	}
	for id, s := range h.sessions {
		if s.Agent == agent && !seen[id] {
			delete(h.sessions, id)
		}
	}
	h.changed()
}

// Apply records a bridge's report about one session, creating the session if
// no adapter has listed it yet.
func (h *Hub) Apply(agent, id string, fn func(*Session)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.sessions[id]
	if !ok {
		now := time.Now()
		s = &Session{ID: id, Agent: agent, Kind: "interactive", Status: Idle, StartedAt: now, ActiveAt: now}
		h.sessions[id] = s
	}
	s.bridgeSeen = time.Now()
	if fn != nil {
		fn(s)
	}
	h.changed()
}

// Sessions returns a snapshot ordered by how much each needs the user.
func (h *Hub) Sessions() []Session {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.snapshot()
}

func (h *Hub) snapshot() []Session {
	rank := map[Status]int{Waiting: 0, Failed: 1, Working: 2, Idle: 3, Done: 4}
	out := make([]Session, 0, len(h.sessions))
	for _, s := range h.sessions {
		c := *s
		c.CanReply = time.Since(s.bridgeSeen) < bridgeTTL
		for _, r := range h.replies {
			if r.SessionID == s.ID {
				c.Replies = append(c.Replies, *r)
			}
		}
		if n := len(c.Replies); n > 5 {
			c.Replies = c.Replies[n-5:]
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if rank[out[i].Status] != rank[out[j].Status] {
			return rank[out[i].Status] < rank[out[j].Status]
		}
		if !out[i].ActiveAt.Equal(out[j].ActiveAt) {
			return out[i].ActiveAt.After(out[j].ActiveAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Enqueue queues text for a session and reports whether the session exists.
func (h *Hub) Enqueue(sessionID, text string) (Reply, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.sessions[sessionID]; !ok {
		return Reply{}, false
	}
	b := make([]byte, 8)
	rand.Read(b)
	r := &Reply{ID: hex.EncodeToString(b), SessionID: sessionID, Text: text, State: Queued, CreatedAt: time.Now()}
	h.replies = append(h.replies, r)
	h.changed()
	return *r, true
}

// Claim hands a session's queued replies to its bridge, each exactly once.
func (h *Hub) Claim(sessionID string) []Reply {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Reply
	for _, r := range h.replies {
		if r.SessionID == sessionID && r.State == Queued {
			r.State = Claimed
			out = append(out, *r)
		}
	}
	if len(out) > 0 {
		h.changed()
	}
	return out
}

// Ack records what became of a claimed reply.
func (h *Hub) Ack(replyID string, state ReplyState) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.replies {
		if r.ID == replyID && r.State == Claimed {
			r.State = state
			h.changed()
		}
	}
}

// Subscribe returns a channel that receives a tick whenever the snapshot
// changes, and a function that ends the subscription.
func (h *Hub) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

func (h *Hub) changed() {
	b, _ := json.Marshal(h.snapshot())
	if string(b) == h.last {
		return
	}
	h.last = string(b)
	for ch := range h.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
