// Package hub holds Slorp's in-memory view of every agent session and the
// outbox of replies waiting to be delivered into them.
package hub

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"log"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jefflinse/slorp/internal/integrations"
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
	// lookupTTL is how long an insight that relied on a lookup is trusted
	// before the lookup is worth repeating.
	lookupTTL = 10 * time.Minute
	// keptLookups is how many recent lookups the UI can show.
	keptLookups = 30
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
	// Role is the role this session has, empty for none.
	Role    string `json:"role"`
	RoleVia string `json:"roleVia,omitempty"`
	// Model is the model that gave the session's last response, as its agent
	// names it, and Reasoning how hard it was asked to think.
	Model     string `json:"model,omitempty"`
	Reasoning string `json:"reasoning,omitempty"`
	// ContextTokens is how much the last response was answered over, and
	// ContextWindow how much the model can take. Zero is unknown.
	ContextTokens int `json:"contextTokens,omitempty"`
	ContextWindow int `json:"contextWindow,omitempty"`
	// Used is what the session's responses have taken so far, as far as its
	// agent recorded them.
	Used Tokens `json:"used"`
	// CostUSD is the agent's own estimate of what the session has cost, zero
	// when it gave none.
	CostUSD float64 `json:"costUsd,omitempty"`

	bridgeSeen time.Time
	key        string
	keySince   time.Time
	insightKey string
	guess      string
	guessKey   string
	insightAt  time.Time
	roleGuess  string
	roleKey    string
	// attention is what the session's role lets through, when the user chose it.
	attention string
}

// Tokens counts what a session's responses took, by how each is charged.
type Tokens struct {
	// In was sent fresh, CacheWrite sent and kept, CacheRead reused.
	In         int `json:"in"`
	CacheWrite int `json:"cacheWrite"`
	CacheRead  int `json:"cacheRead"`
	// Out is what the model wrote.
	Out int `json:"out"`
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

// Unroled is a described session the user has given no role, for the model
// to guess one.
type Unroled struct {
	Session Session
	Key     string
	Roles   []kb.Role
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
	// Lookups names what the model looked up through an integration to write this.
	Lookups []string `json:"lookups"`
}

// Lookup is one thing the model looked up through an integration.
type Lookup struct {
	At      time.Time `json:"at"`
	Session string    `json:"session"`
	What    string    `json:"what"`
	OK      bool      `json:"ok"`
	// Detail is what came back, or why nothing did.
	Detail string `json:"detail"`
}

// Insight kinds, and the item kinds the hub derives without a model.
const (
	KindQuestion   = "question"
	KindPermission = "permission"
	KindReview     = "review"
	KindNone       = "none"
	KindFailed     = "failed"
)

// Item is one thing that needs the user: a session waiting on them, or
// something an integration's feed found.
type Item struct {
	ID string `json:"id"`
	// SessionID is empty for an item from a feed.
	SessionID string `json:"sessionId,omitempty"`
	Kind      string `json:"kind"`
	// Urgency is "blocking" while the user is holding up another person,
	// "awaiting" while something of the user's own is stopped until they
	// act, and "whenever" when nothing is stopped.
	Urgency string    `json:"urgency"`
	Title   string    `json:"title"`
	Ask     string    `json:"ask"`
	Summary string    `json:"summary"`
	Replies []string  `json:"replies"`
	At      time.Time `json:"at"`
	// Source is the integration whose feed found the item.
	Source string `json:"source,omitempty"`
	URL    string `json:"url,omitempty"`
	// Repo is the repository a feed item belongs to, which is what it is filed by.
	Repo string `json:"repo,omitempty"`
	// Effort is the effort the item is filed under, empty when unsorted.
	Effort    string `json:"effort"`
	EffortVia string `json:"effortVia,omitempty"`
}

// FeedStatus is how an integration's last collection went.
type FeedStatus struct {
	At time.Time `json:"at"`
	// Error is why the last collection failed; the items are then older.
	Error string `json:"error,omitempty"`
	Count int    `json:"count"`
}

type feed struct {
	items []Item
	pulls []integrations.PullRequest
	FeedStatus
}

// PullRequest is an open pull request an integration listed, filed like the
// feed items from its repository.
type PullRequest struct {
	integrations.PullRequest
	Source    string `json:"source"`
	Effort    string `json:"effort"`
	EffortVia string `json:"effortVia,omitempty"`
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

// Pending is a request a session is blocked on: for permission to act, or
// for an answer to a question it put to the user.
type Pending struct {
	ID      string `json:"id"`
	Tool    string `json:"tool"`
	Summary string `json:"summary"`
	// Question is true when the session asked the user something, which is
	// answered by picking from its options and not by an allow.
	Question bool `json:"question"`
	// Input is the tool's full input, shown to the user before they decide.
	Input string `json:"input"`
	// Answerable is true while Slorp may answer the request as well as the
	// session's own terminal, whose dialog is asking at the same time.
	Answerable bool      `json:"answerable"`
	At         time.Time `json:"at"`

	// questions is what a question asks, read from its input.
	questions []asked
}

// asked is one question put to the user and the options it offers, by label.
type asked struct {
	text   string
	labels []string
	// many is true when more than one option may be picked.
	many bool
}

// questionsIn reads the questions a prompt's input holds. It returns nil
// unless each has its own text and at least one labelled option.
func questionsIn(input string) []asked {
	var in struct {
		Questions []struct {
			Question    string `json:"question"`
			MultiSelect bool   `json:"multiSelect"`
			Options     []struct {
				Label string `json:"label"`
			} `json:"options"`
		} `json:"questions"`
	}
	if json.Unmarshal([]byte(input), &in) != nil {
		return nil
	}
	var out []asked
	for _, q := range in.Questions {
		a := asked{text: q.Question, many: q.MultiSelect}
		for _, o := range q.Options {
			if o.Label == "" || slices.Contains(a.labels, o.Label) {
				return nil
			}
			a.labels = append(a.labels, o.Label)
		}
		if q.Question == "" || len(a.labels) == 0 || slices.ContainsFunc(out, func(b asked) bool { return b.text == q.Question }) {
			return nil
		}
		out = append(out, a)
	}
	return out
}

// Decision is the user's answer, given in Slorp, to a request.
type Decision struct {
	Behavior string `json:"state"`
	Message  string `json:"message,omitempty"`
	// Answers holds, for a question, what the user picked for each of its
	// questions by its text; several picks are joined with ", ".
	Answers map[string]string `json:"answers,omitempty"`
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
	// Integrations are the ones the user has turned on, by ID.
	Integrations map[string]bool `json:"integrations"`
	// IntegrationsRevision counts changes to Integrations.
	IntegrationsRevision int `json:"integrationsRevision"`
}

// Cache is where the hub keeps what should outlive a restart.
type Cache interface {
	Get(kind, id string, v any) (bool, error)
	Put(kind, id string, v any) error
	Each(kind string, fn func(id string, body []byte)) error
}

// What the model wrote about one session, kept so a restart need not ask again.
type sessionDoc struct {
	Insight    *Insight  `json:"insight"`
	InsightKey string    `json:"insightKey"`
	Guess      string    `json:"guess"`
	GuessKey   string    `json:"guessKey"`
	InsightAt  time.Time `json:"insightAt"`
	RoleGuess  string    `json:"roleGuess"`
	RoleKey    string    `json:"roleKey"`
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
	lookups  []Lookup
	feeds    map[string]*feed
	clones   map[string][]string
}

type note struct {
	Key string `json:"key"`
	Note
}

// New returns a hub restored from what c holds.
func New(k *kb.Store, c Cache) *Hub {
	h := &Hub{sessions: map[string]*Session{}, subs: map[chan struct{}]struct{}{}, kb: k, notes: map[string]note{},
		cache: c, settings: Settings{AnswerPermissions: true}, waiters: map[string]chan Decision{}, feeds: map[string]*feed{}}
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
	if h.settings.Integrations == nil {
		h.settings.Integrations = map[string]bool{}
	}
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
	s.Insight, s.insightKey, s.guess, s.guessKey, s.insightAt = d.Insight, d.InsightKey, d.Guess, d.GuessKey, d.InsightAt
	s.roleGuess, s.roleKey = d.RoleGuess, d.RoleKey
}

func (h *Hub) remember(s *Session) {
	h.warn(h.cache.Put("session", s.ID, sessionDoc{Insight: s.Insight, InsightKey: s.insightKey, Guess: s.guess, GuessKey: s.guessKey, InsightAt: s.insightAt,
		RoleGuess: s.roleGuess, RoleKey: s.roleKey}))
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
			s.guess, s.guessKey, s.insightAt = old.guess, old.guessKey, old.insightAt
			s.roleGuess, s.roleKey = old.roleGuess, old.roleKey
			s.ContextWindow, s.CostUSD = old.ContextWindow, old.CostUSD
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

// Apply records a bridge's report about one session. It creates the session
// if nothing has listed it yet, and reports whether it did.
func (h *Hub) Apply(agent, id string, fn func(*Session)) (created bool) {
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
	return !ok
}

// Retire drops the sessions of an agent whose bridge has reported nothing
// for longer than silence. It stands in for a roster where a session's
// bridge is the only witness that the session is still open.
func (h *Hub) Retire(agent string, silence time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, s := range h.sessions {
		if s.Agent == agent && time.Since(s.bridgeSeen) > silence {
			delete(h.sessions, id)
		}
	}
	h.changed()
}

// Ask records that a session wants permission for a tool call. Slorp may
// answer it when the bridge says it can and the user has that turned on;
// the session's terminal is asking either way.
func (h *Hub) Ask(agent, sessionID string, p Pending, canAnswer bool) Pending {
	p.ID, p.At = newID(), time.Now()
	h.Apply(agent, sessionID, func(s *Session) {
		if p.Question {
			p.questions = questionsIn(p.Input)
		}
		p.Answerable = canAnswer && h.settings.AnswerPermissions && (!p.Question || p.questions != nil)
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

// Decide allows or denies a request from Slorp, and reports whether it took.
// It does not when the request is no longer waiting, or is a question being
// allowed: Answer answers those.
func (h *Hub) Decide(requestID string, dec Decision) bool {
	dec.Answers = nil
	return h.decide(requestID, func(p *Pending) (Decision, bool) { return dec, !(p.Question && dec.Behavior == Allow) })
}

// Answer answers a question from Slorp with what the user picked, by label,
// for each of its questions by text, and reports whether it took. It does
// not when the request is no longer waiting or the picks do not answer it:
// every question needs a pick from its own options, and only one unless it
// takes several.
func (h *Hub) Answer(requestID string, picks map[string][]string) bool {
	return h.decide(requestID, func(p *Pending) (Decision, bool) {
		if len(p.questions) == 0 || len(picks) != len(p.questions) {
			return Decision{}, false
		}
		answers := map[string]string{}
		for _, q := range p.questions {
			got := picks[q.text]
			if len(got) == 0 || (len(got) > 1 && !q.many) {
				return Decision{}, false
			}
			var chosen []string
			for _, label := range q.labels {
				if slices.Contains(got, label) {
					chosen = append(chosen, label)
				}
			}
			if len(chosen) != len(got) {
				return Decision{}, false
			}
			answers[q.text] = strings.Join(chosen, ", ")
		}
		return Decision{Behavior: Allow, Answers: answers}, true
	})
}

// decide passes on to the bridge the decision with makes of a request Slorp
// may answer, and reports whether it did.
func (h *Hub) decide(requestID string, with func(*Pending) (Decision, bool)) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.sessions {
		if s.Pending == nil || s.Pending.ID != requestID || !s.Pending.Answerable {
			continue
		}
		dec, ok := with(s.Pending)
		if !ok {
			return false
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

// SetSettings replaces the user's preferences, leaving which integrations
// are on to SetIntegration.
func (h *Hub) SetSettings(s Settings) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s.Integrations, s.IntegrationsRevision = h.settings.Integrations, h.settings.IntegrationsRevision
	h.settings = s
	h.saveSettings()
}

// SetIntegration turns one integration on or off. Every session is then
// described again, since what the model can look up has changed.
func (h *Hub) SetIntegration(id string, on bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.settings.Integrations[id] == on {
		return
	}
	h.settings.Integrations[id] = on
	h.settings.IntegrationsRevision++
	h.saveSettings()
}

func (h *Hub) saveSettings() {
	h.warn(h.cache.Put("setting", "main", h.settings))
	h.last = ""
	h.changed()
}

// Looked records a lookup for the user to see.
func (h *Hub) Looked(l Lookup) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lookups = append(h.lookups, l)
	if n := len(h.lookups); n > keptLookups {
		h.lookups = h.lookups[n-keptLookups:]
	}
	h.last = ""
	h.changed()
}

// SetFeed records what an integration's feed found: the things that need the
// user and the pull requests it listed. When the collection failed, what it
// found the time before is kept.
func (h *Hub) SetFeed(source string, found []Item, pulls []integrations.PullRequest, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f := h.feeds[source]
	if f == nil {
		f = &feed{}
		h.feeds[source] = f
	}
	f.At, f.Error = time.Now(), ""
	if err != nil {
		f.Error = err.Error()
	} else {
		f.items = make([]Item, len(found))
		for i, it := range found {
			it.Source, it.SessionID, it.Replies = source, "", []string{}
			f.items[i] = it
		}
		f.pulls = pulls
	}
	f.Count = len(f.items)
	h.last = ""
	h.changed()
}

// DropFeed forgets an integration's feed, as when it is turned off.
func (h *Hub) DropFeed(source string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.feeds[source]; ok {
		delete(h.feeds, source)
		h.last = ""
		h.changed()
	}
}

// SetClones records which repositories are checked out in which of the
// efforts' directories, by directory. Feed items are filed by it.
func (h *Hub) SetClones(byDir map[string][]string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clones = byDir
	h.changed()
}

// fed returns every feed's items, each filed under the effort its
// repository belongs to.
func (h *Hub) fed() []Item {
	sources := make([]string, 0, len(h.feeds))
	for id := range h.feeds {
		sources = append(sources, id)
	}
	sort.Strings(sources)
	out := []Item{}
	for _, id := range sources {
		for _, it := range h.feeds[id].items {
			it.Effort, it.EffortVia = h.fileRepo(it.Repo)
			out = append(out, it)
		}
	}
	return out
}

// pulls returns every feed's pull requests, the most pressing of the user's
// moves first and those waiting on someone else last, the most recently
// changed first within each.
func (h *Hub) pulls() []PullRequest {
	out := []PullRequest{}
	for id, f := range h.feeds {
		for _, p := range f.pulls {
			pr := PullRequest{PullRequest: p, Source: id}
			pr.Effort, pr.EffortVia = h.fileRepo(p.Repo)
			out = append(out, pr)
		}
	}
	rank := map[string]int{"blocking": 3, "awaiting": 2, "whenever": 1}
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := rank[out[i].Urgency], rank[out[j].Urgency]; a != b {
			return a > b
		}
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.After(out[j].At)
		}
		return out[i].Repo+"#"+strconv.Itoa(out[i].Number) < out[j].Repo+"#"+strconv.Itoa(out[j].Number)
	})
	return out
}

// fileRepo reports the effort a repository's items belong under, empty when
// neither the user nor a directory rule says.
func (h *Hub) fileRepo(repo string) (effort, via string) {
	var held []string
	for dir, repos := range h.clones {
		for _, r := range repos {
			if strings.EqualFold(r, repo) {
				held = append(held, dir)
				break
			}
		}
	}
	if effort, via, ok := h.kb.ResolveRepo(repo, held); ok {
		return effort, via
	}
	return "", ""
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
	// Lookups are the most recent ones, newest last.
	Lookups []Lookup `json:"lookups"`
	// Feeds says how each integration's last collection went, by integration.
	Feeds map[string]FeedStatus `json:"feeds"`
	// PullRequests are the open ones the integrations listed.
	PullRequests []PullRequest `json:"pullRequests"`
}

func (h *Hub) State() State {
	h.mu.Lock()
	defer h.mu.Unlock()
	sessions := h.snapshot()
	feeds := map[string]FeedStatus{}
	for id, f := range h.feeds {
		feeds[id] = f.FeedStatus
	}
	return State{Sessions: sessions, Items: append(items(sessions), h.fed()...), Efforts: h.efforts(sessions), Summarizer: h.summ,
		Settings: h.settings, Knowledge: h.kb.Knowledge(), Lookups: append([]Lookup{}, h.lookups...), Feeds: feeds, PullRequests: h.pulls()}
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

// Unroled lists described sessions that still need the model to guess a role.
func (h *Hub) Unroled() []Unroled {
	roles := h.kb.Knowledge().Roles
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Unroled
	if len(roles) == 0 {
		return out
	}
	for _, s := range h.sessions {
		key := s.insightKey + ":" + strconv.Itoa(h.kb.RoleRevision())
		if s.insightKey != s.key || s.roleKey == key {
			continue
		}
		if _, ok := h.kb.RoleOf(s.ID); !ok {
			out = append(out, Unroled{Session: *s, Key: key, Roles: roles})
		}
	}
	return out
}

// SetRoleGuess records the model's choice of role for a session; an empty
// roleID means it found none the session clearly has.
func (h *Hub) SetRoleGuess(sessionID, key, roleID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s, ok := h.sessions[sessionID]; ok {
		s.roleGuess, s.roleKey = roleID, key
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
		outdated := s.key != s.insightKey ||
			(s.Insight != nil && len(s.Insight.Lookups) > 0 && time.Since(s.insightAt) > lookupTTL)
		if hasContent && outdated && time.Since(s.keySince) >= quiet && (s.Status != Working || s.Pending != nil) {
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
		s.Insight, s.insightKey, s.insightAt = &in, key, time.Now()
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

// contentOf strips from a content key the revisions of what the model reads
// besides the session: the standing context and the integrations turned on.
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

// items derives what needs the user from the sessions. Permission prompts,
// blocked and failed sessions are items on their state alone; a question or
// finished work is an item only once an insight written for the current
// content says so. An insight that calls for permission when no prompt is
// pending is taken as a question. A role the user gave a session can keep it
// from asking: altogether, or unless it is stopped.
func items(sessions []Session) []Item {
	out := []Item{}
	for _, s := range sessions {
		it := Item{ID: s.ID[:min(8, len(s.ID))] + "-" + s.key[:min(10, len(s.key))], SessionID: s.ID, At: s.ActiveAt, Replies: []string{},
			Effort: s.Effort, EffortVia: s.EffortVia}
		if s.attention == kb.AttentionNone {
			continue
		}
		it.Title = s.Title
		if it.Title == "" {
			it.Title = s.Name
		}
		fresh := s.described()
		if fresh {
			it.Title, it.Summary, it.Ask = s.Insight.Title, s.Insight.Summary, s.Insight.Ask
		}
		switch {
		case s.Pending != nil && s.Pending.Question:
			it.Kind, it.Urgency = KindQuestion, "awaiting"
			if it.Ask == "" {
				it.Ask = cmp.Or(s.Pending.Summary, "This session asked you something. Answer it in its terminal.")
			}
		case s.Pending != nil:
			it.Kind, it.Urgency = KindPermission, "awaiting"
			if it.Ask == "" {
				it.Ask = "Allow or deny " + s.Pending.Tool + ": " + s.Pending.Summary
			}
		case s.Status == Failed:
			it.Kind, it.Urgency = KindFailed, "awaiting"
			if it.Ask == "" {
				it.Ask = "This session failed. Look at what went wrong."
			}
		case s.Status == Waiting:
			it.Kind, it.Urgency = KindQuestion, "awaiting"
			if it.Ask == "" {
				it.Ask = "This session is stopped until you respond."
			}
			if fresh {
				it.Replies = s.Insight.Replies
			}
		case s.attention == kb.AttentionStopped:
			continue
		case fresh && s.Status != Working && (s.Insight.Kind == KindQuestion || s.Insight.Kind == KindPermission):
			it.Kind, it.Urgency, it.Replies = KindQuestion, "awaiting", s.Insight.Replies
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
		if role, ok := h.kb.RoleOf(s.ID); ok {
			c.Role, c.RoleVia, c.attention = role.ID, kb.ViaUser, role.Attention
		} else if s.roleGuess != "" && h.kb.HasRole(s.roleGuess) {
			c.Role, c.RoleVia = s.roleGuess, ViaGuess
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
	context := ":" + strconv.Itoa(h.kb.ContextRevision()) + "." + strconv.Itoa(h.settings.IntegrationsRevision)
	for _, s := range h.sessions {
		if k := contentKey(s) + context; k != s.key {
			s.key, s.keySince = k, now
		}
	}
	snap := h.snapshot()
	b, _ := json.Marshal(map[string]any{"s": snap, "i": items(snap), "f": h.fed(), "e": h.efforts(snap), "m": h.summ})
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
