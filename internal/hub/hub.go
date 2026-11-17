// Package hub holds Slorp's in-memory view of every agent session and the
// outbox of replies waiting to be delivered into them.
package hub

import (
	"crypto/rand"
	"crypto/sha1"
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

const (
	// bridgeTTL is how long after its last poll a session's bridge still counts as connected.
	bridgeTTL = 10 * time.Second
	// settle is how long a session's content must hold still before it is worth describing.
	settle = 3 * time.Second
)

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
	Insight    *Insight  `json:"insight,omitempty"`

	bridgeSeen time.Time
	key        string
	keySince   time.Time
	insightKey string
}

// Insight is a model-written description of a session's current state.
type Insight struct {
	Title   string   `json:"title"`
	Summary string   `json:"summary"`
	Kind    string   `json:"kind"`
	Ask     string   `json:"ask"`
	Replies []string `json:"replies"`
}

// Insight kinds, and the item kinds the hub derives without a model.
const (
	KindQuestion   = "question"
	KindPermission = "permission"
	KindReview     = "review"
	KindNone       = "none"
	KindFailed     = "failed"
)

// Item is one thing that needs the user.
type Item struct {
	ID        string    `json:"id"`
	SessionID string    `json:"sessionId"`
	Kind      string    `json:"kind"`
	Urgency   string    `json:"urgency"`
	Title     string    `json:"title"`
	Ask       string    `json:"ask"`
	Summary   string    `json:"summary"`
	Replies   []string  `json:"replies"`
	At        time.Time `json:"at"`
}

// Summarizer reports which model, if any, is writing insights.
type Summarizer struct {
	Model string `json:"model"`
	Error string `json:"error,omitempty"`
}

// Undescribed is a settled session whose current content has no insight yet.
type Undescribed struct {
	Session Session
	Key     string
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
	summ     Summarizer
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
			s.key, s.keySince, s.insightKey, s.Insight = old.key, old.keySince, old.insightKey, old.Insight
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

// State returns the sessions, the items derived from them, and the summarizer.
func (h *Hub) State() ([]Session, []Item, Summarizer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	sessions := h.snapshot()
	return sessions, items(sessions), h.summ
}

// Undescribed lists sessions that have settled on content no insight covers.
// Working sessions are left out: their content is still moving.
func (h *Hub) Undescribed() []Undescribed {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Undescribed
	for _, s := range h.sessions {
		hasContent := s.LastReply != "" || s.Pending != nil
		if hasContent && s.key != s.insightKey && time.Since(s.keySince) >= settle && (s.Status != Working || s.Pending != nil) {
			out = append(out, Undescribed{Session: *s, Key: s.key})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Session.ActiveAt.After(out[j].Session.ActiveAt) })
	return out
}

// SetInsight attaches an insight written for the content identified by key.
func (h *Hub) SetInsight(sessionID, key string, in Insight) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s, ok := h.sessions[sessionID]; ok {
		s.Insight, s.insightKey = &in, key
		h.changed()
	}
}

func (h *Hub) SetSummarizer(s Summarizer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.summ != s {
		h.summ = s
		h.last = ""
		h.changed()
	}
}

// contentKey identifies what a session is currently showing the user.
func contentKey(s *Session) string {
	state, pending := "open", ""
	switch {
	case s.Pending != nil:
		state, pending = "permission", s.Pending.Tool+"\x00"+s.Pending.Summary
	case s.Status == Waiting, s.Status == Failed:
		state = string(s.Status)
	}
	sum := sha1.Sum([]byte(state + "\x00" + pending + "\x00" + s.LastPrompt + "\x00" + s.LastReply))
	return hex.EncodeToString(sum[:])
}

// items derives what needs the user. Permission prompts, blocked and failed
// sessions are items on their state alone; a question or finished work is an
// item only once an insight written for the current content says so.
func items(sessions []Session) []Item {
	out := []Item{}
	for _, s := range sessions {
		it := Item{ID: s.ID[:min(8, len(s.ID))] + "-" + s.key[:min(10, len(s.key))], SessionID: s.ID, At: s.ActiveAt, Replies: []string{}}
		it.Title = s.Title
		if it.Title == "" {
			it.Title = s.Name
		}
		fresh := s.Insight != nil && s.insightKey == s.key
		if fresh {
			it.Title, it.Summary, it.Ask = s.Insight.Title, s.Insight.Summary, s.Insight.Ask
		}
		switch {
		case s.Pending != nil:
			it.Kind, it.Urgency = KindPermission, "blocking"
			if it.Ask == "" {
				it.Ask = "Allow or deny " + s.Pending.Tool + ": " + s.Pending.Summary
			}
		case s.Status == Failed:
			it.Kind, it.Urgency = KindFailed, "soon"
			if it.Ask == "" {
				it.Ask = "This session failed. Look at what went wrong."
			}
		case s.Status == Waiting:
			it.Kind, it.Urgency = KindQuestion, "blocking"
			if it.Ask == "" {
				it.Ask = "This session is stopped until you respond."
			}
			if fresh {
				it.Replies = s.Insight.Replies
			}
		case fresh && s.Status != Working && s.Insight.Kind == KindQuestion:
			it.Kind, it.Urgency, it.Replies = KindQuestion, "blocking", s.Insight.Replies
		case fresh && s.Status != Working && s.Insight.Kind == KindReview:
			it.Kind, it.Urgency, it.Replies = KindReview, "whenever", s.Insight.Replies
		default:
			continue
		}
		if it.Replies == nil {
			it.Replies = []string{}
		}
		out = append(out, it)
	}
	return out
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
	now := time.Now()
	for _, s := range h.sessions {
		if k := contentKey(s); k != s.key {
			s.key, s.keySince = k, now
		}
	}
	snap := h.snapshot()
	b, _ := json.Marshal(map[string]any{"s": snap, "i": items(snap), "m": h.summ})
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
