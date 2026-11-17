// Package hub holds Slorp's in-memory view of every agent session and the
// outbox of replies waiting to be delivered into them.
package hub

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jefflinse/slorp/internal/kb"
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
	// quiet is how long a session's content must hold still before it is worth describing.
	quiet = 3 * time.Second
	// pendingGrace is how long a new permission request is trusted before the roster confirms it.
	pendingGrace = 3 * time.Second
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
	// Effort is the effort this session is filed under, empty when unsorted.
	Effort    string `json:"effort"`
	EffortVia string `json:"effortVia,omitempty"`

	bridgeSeen time.Time
	key        string
	keySince   time.Time
	insightKey string
	guess      string
	guessKey   string
}

// ViaGuess marks a filing the model chose, as opposed to the user or a rule.
const ViaGuess = "guess"

// Effort is a declared effort with the model's account of where it stands.
type Effort struct {
	kb.Effort
	Line   string `json:"line"`
	Stands string `json:"stands"`
}

// Note is a model-written status for one effort.
type Note struct {
	Line   string `json:"line"`
	Stands string `json:"stands"`
}

// Unrouted is a described session that neither the user nor a rule has filed.
type Unrouted struct {
	Session Session
	Key     string
	Efforts []kb.Effort
	// Hints are the user's standing context and routing hints.
	Hints []string
}

// Unnoted is an effort whose sessions have changed since its note was written.
type Unnoted struct {
	Effort  kb.Effort
	Members []Session
	Key     string
	Context []string
	// Questions are what the user has left undecided about this effort.
	Questions []string
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
	// Context is the user's standing context.
	Context []string
}

// Pending is a permission request a session is blocked on.
type Pending struct {
	ID      string `json:"id"`
	Tool    string `json:"tool"`
	Summary string `json:"summary"`
	// Input is the tool's full input, shown to the user before they decide.
	Input string `json:"input"`
	// Answerable is true while Slorp may answer the request as well as the
	// session's own terminal, whose dialog is asking at the same time.
	Answerable bool      `json:"answerable"`
	At         time.Time `json:"at"`
}

// Decision is the user's answer, given in Slorp, to a permission request.
type Decision struct {
	Behavior string `json:"state"`
	Message  string `json:"message,omitempty"`
}

// Decision behaviors. Terminal means Slorp will not answer: the session's
// own dialog decides.
const (
	Allow    = "allow"
	Deny     = "deny"
	Terminal = "terminal"
)

// Settings are the user's preferences for how Slorp behaves.
type Settings struct {
	// AnswerPermissions lets permission prompts be answered in Slorp as well
	// as in the session's terminal.
	AnswerPermissions bool `json:"answerPermissions"`
}

// Cache is where the hub keeps what should outlive a restart.
type Cache interface {
	Get(kind, id string, v any) (bool, error)
	Put(kind, id string, v any) error
	Each(kind string, fn func(id string, body []byte)) error
}

// What the model wrote about one session, kept so a restart need not ask again.
type sessionDoc struct {
	Insight    *Insight `json:"insight"`
	InsightKey string   `json:"insightKey"`
	Guess      string   `json:"guess"`
	GuessKey   string   `json:"guessKey"`
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
	kb       *kb.Store
	notes    map[string]note
	cache    Cache
	settings Settings
	waiters  map[string]chan Decision
}

type note struct {
	Key string `json:"key"`
	Note
}

// New returns a hub restored from what c holds.
func New(k *kb.Store, c Cache) *Hub {
	h := &Hub{sessions: map[string]*Session{}, subs: map[chan struct{}]struct{}{}, kb: k, notes: map[string]note{},
		cache: c, settings: Settings{AnswerPermissions: true}, waiters: map[string]chan Decision{}}
	h.warn(c.Each("note", func(id string, body []byte) {
		var n note
		if json.Unmarshal(body, &n) == nil {
			h.notes[id] = n
		}
	}))
	h.warn(c.Each("reply", func(id string, body []byte) {
		var r Reply
		if json.Unmarshal(body, &r) == nil {
			h.replies = append(h.replies, &r)
		}
	}))
	_, err := c.Get("setting", "main", &h.settings)
	h.warn(err)
	return h
}

func (h *Hub) warn(err error) {
	if err != nil {
		log.Printf("hub: store: %v", err)
	}
}

// restore gives a session seen for the first time what the model last wrote about it.
func (h *Hub) restore(s *Session) {
	var d sessionDoc
	if ok, err := h.cache.Get("session", s.ID, &d); err != nil || !ok {
		h.warn(err)
		return
	}
	s.Insight, s.insightKey, s.guess, s.guessKey = d.Insight, d.InsightKey, d.Guess, d.GuessKey
}

func (h *Hub) remember(s *Session) {
	h.warn(h.cache.Put("session", s.ID, sessionDoc{Insight: s.Insight, InsightKey: s.insightKey, Guess: s.guess, GuessKey: s.guessKey}))
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
			s.guess, s.guessKey = old.guess, old.guessKey
			// The roster stops saying "waiting" once the terminal's dialog is
			// answered. A request it has not caught up with yet is kept.
			if p := old.Pending; p != nil && (s.Status == Waiting || time.Since(p.At) < pendingGrace) {
				s.Pending, s.Status, s.WaitingFor = p, Waiting, "permission prompt"
			} else if p != nil {
				h.settle(p.ID)
			}
			if s.LastReply == "" {
				s.LastReply = old.LastReply
			}
			if s.LastPrompt == "" {
				s.LastPrompt = old.LastPrompt
			}
		} else {
			h.restore(&s)
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
		h.restore(s)
		h.sessions[id] = s
	}
	s.bridgeSeen = time.Now()
	if fn != nil {
		fn(s)
	}
	h.changed()
}

// Ask records that a session wants permission for a tool call. Slorp may
// answer it when the bridge says it can and the user has that turned on;
// the session's terminal is asking either way.
func (h *Hub) Ask(agent, sessionID string, p Pending, canAnswer bool) Pending {
	p.ID, p.At = newID(), time.Now()
	h.Apply(agent, sessionID, func(s *Session) {
		p.Answerable = canAnswer && h.settings.AnswerPermissions
		s.Pending, s.Status, s.WaitingFor, s.ActiveAt = &p, Waiting, "permission prompt", p.At
		if p.Answerable {
			h.waiters[p.ID] = make(chan Decision, 1)
		}
	})
	return p
}

// Await waits up to d for the user's decision in Slorp. ok is false when the
// wait ran out with the request still undecided. A request Slorp is not
// answering, or that the session has moved past, resolves at once to Terminal.
func (h *Hub) Await(ctx context.Context, requestID string, d time.Duration) (dec Decision, ok bool) {
	h.mu.Lock()
	ch, held := h.waiters[requestID]
	if held && len(ch) == 0 && !h.asking(requestID) {
		// The terminal answered, or the turn ended; nothing is waiting any more.
		delete(h.waiters, requestID)
		held = false
	}
	h.mu.Unlock()
	if !held {
		return Decision{Behavior: Terminal}, true
	}
	select {
	case dec = <-ch:
		h.mu.Lock()
		delete(h.waiters, requestID)
		h.mu.Unlock()
		return dec, true
	case <-time.After(d):
	case <-ctx.Done():
	}
	return Decision{}, false
}

// settle tells whoever is waiting on a request that Slorp will not answer it.
func (h *Hub) settle(requestID string) {
	if ch, ok := h.waiters[requestID]; ok {
		select {
		case ch <- Decision{Behavior: Terminal}:
		default:
		}
	}
}

func (h *Hub) asking(requestID string) bool {
	for _, s := range h.sessions {
		if s.Pending != nil && s.Pending.ID == requestID {
			return true
		}
	}
	return false
}

// Decide answers a request from Slorp, and reports whether it was still
// waiting for an answer.
func (h *Hub) Decide(requestID string, dec Decision) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.sessions {
		if s.Pending == nil || s.Pending.ID != requestID || !s.Pending.Answerable {
			continue
		}
		select {
		case h.waiters[requestID] <- dec:
		default:
			return false
		}
		s.Pending, s.Status, s.WaitingFor = nil, Working, ""
		h.changed()
		return true
	}
	return false
}

// Done records that a request was settled outside Slorp.
func (h *Hub) Done(requestID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.settle(requestID)
	for _, s := range h.sessions {
		if s.Pending != nil && s.Pending.ID == requestID {
			s.Pending, s.Status, s.WaitingFor = nil, Working, ""
			h.changed()
		}
	}
}

func (h *Hub) Settings() Settings {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.settings
}

func (h *Hub) SetSettings(s Settings) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.settings = s
	h.warn(h.cache.Put("setting", "main", s))
	h.last = ""
	h.changed()
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Sessions returns a snapshot ordered by how much each needs the user.
func (h *Hub) Sessions() []Session {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.snapshot()
}

// State is everything the web UI draws.
type State struct {
	Sessions   []Session    `json:"sessions"`
	Items      []Item       `json:"items"`
	Efforts    []Effort     `json:"efforts"`
	Summarizer Summarizer   `json:"summarizer"`
	Settings   Settings     `json:"settings"`
	Knowledge  kb.Knowledge `json:"knowledge"`
}

func (h *Hub) State() State {
	h.mu.Lock()
	defer h.mu.Unlock()
	sessions := h.snapshot()
	return State{sessions, items(sessions), h.efforts(sessions), h.summ, h.settings, h.kb.Knowledge()}
}

func (h *Hub) efforts(sessions []Session) []Effort {
	out := []Effort{}
	for _, e := range h.kb.Efforts() {
		v := Effort{Effort: e}
		for _, s := range sessions {
			if s.Effort == e.ID {
				v.Line, v.Stands = h.notes[e.ID].Line, h.notes[e.ID].Stands
				break
			}
		}
		out = append(out, v)
	}
	return out
}

// Touch republishes state after the knowledge base changed.
func (h *Hub) Touch() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.changed()
}

// routeKey identifies what a model's filing guess was based on.
func (h *Hub) routeKey(s *Session) string {
	return s.insightKey + ":" + strconv.Itoa(h.kb.Revision())
}

// Unrouted lists described sessions that still need the model to guess an effort.
func (h *Hub) Unrouted() []Unrouted {
	efforts := h.kb.Efforts()
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Unrouted
	if len(efforts) == 0 {
		return out
	}
	for _, s := range h.sessions {
		if s.Insight == nil || s.insightKey != s.key || s.guessKey == h.routeKey(s) {
			continue
		}
		if _, _, ok := h.kb.Resolve(s.ID, s.Cwd); !ok {
			know := h.kb.Knowledge()
			out = append(out, Unrouted{Session: *s, Key: h.routeKey(s), Efforts: efforts, Hints: append(texts(know.Context), texts(know.Routing)...)})
		}
	}
	return out
}

// SetGuess records the model's filing for a session; an empty effortID means
// it found no effort the session clearly belongs to.
func (h *Hub) SetGuess(sessionID, key, effortID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s, ok := h.sessions[sessionID]; ok {
		s.guess, s.guessKey = effortID, key
		h.remember(s)
		h.changed()
	}
}

// Unnoted lists efforts with described sessions whose note is out of date.
func (h *Hub) Unnoted() []Unnoted {
	h.mu.Lock()
	defer h.mu.Unlock()
	sessions := h.snapshot()
	know := h.kb.Knowledge()
	var out []Unnoted
	for _, e := range h.kb.Efforts() {
		var members []Session
		var questions []string
		key := e.Name + "\x00" + e.About
		for _, q := range know.Questions {
			if q.Effort == e.ID {
				questions = append(questions, q.Text)
				key += "\x00?" + q.Text
			}
		}
		for _, s := range sessions {
			if s.Effort == e.ID && s.Insight != nil {
				members = append(members, s)
				key += "\x00" + s.ID + ":" + s.insightKey
			}
		}
		sum := sha1.Sum([]byte(key))
		if k := hex.EncodeToString(sum[:]); len(members) > 0 && h.notes[e.ID].Key != k {
			out = append(out, Unnoted{Effort: e, Members: members, Key: k, Context: texts(know.Context), Questions: questions})
		}
	}
	return out
}

func (h *Hub) SetNote(effortID, key string, n Note) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.notes[effortID] = note{Key: key, Note: n}
	h.warn(h.cache.Put("note", effortID, h.notes[effortID]))
	h.last = ""
	h.changed()
}

// Undescribed lists sessions that have settled on content no insight covers.
// Working sessions are left out: their content is still moving.
func (h *Hub) Undescribed() []Undescribed {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Undescribed
	for _, s := range h.sessions {
		hasContent := s.LastReply != "" || s.Pending != nil
		if hasContent && s.key != s.insightKey && time.Since(s.keySince) >= quiet && (s.Status != Working || s.Pending != nil) {
			out = append(out, Undescribed{Session: *s, Key: s.key, Context: texts(h.kb.Knowledge().Context)})
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
		h.remember(s)
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

// described reports whether the session's insight was written about what it
// is showing now. An insight written under older standing context still
// counts, so that editing the context does not blank everything it replaces.
func (s *Session) described() bool {
	return s.Insight != nil && contentOf(s.insightKey) == contentOf(s.key)
}

// contentOf strips the standing-context revision from a content key.
func contentOf(key string) string {
	if i := strings.LastIndexByte(key, ':'); i >= 0 {
		return key[:i]
	}
	return key
}

func texts(facts []kb.Fact) []string {
	out := make([]string, len(facts))
	for i, f := range facts {
		out[i] = f.Text
	}
	return out
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
		fresh := s.described()
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
		if id, via, ok := h.kb.Resolve(s.ID, s.Cwd); ok {
			c.Effort, c.EffortVia = id, via
		} else if s.guess != "" && h.kb.Has(s.guess) {
			// A guess made on older content stands until a newer one replaces it.
			c.Effort, c.EffortVia = s.guess, ViaGuess
		}
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
	r := &Reply{ID: newID(), SessionID: sessionID, Text: text, State: Queued, CreatedAt: time.Now()}
	h.replies = append(h.replies, r)
	h.warn(h.cache.Put("reply", r.ID, r))
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
			h.warn(h.cache.Put("reply", r.ID, r))
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
			h.warn(h.cache.Put("reply", r.ID, r))
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
	context := ":" + strconv.Itoa(h.kb.ContextRevision())
	for _, s := range h.sessions {
		if k := contentKey(s) + context; k != s.key {
			s.key, s.keySince = k, now
		}
	}
	snap := h.snapshot()
	b, _ := json.Marshal(map[string]any{"s": snap, "i": items(snap), "e": h.efforts(snap), "m": h.summ})
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
