// Package integrations reads the user's other tools, and never changes
// anything in them. It gives the local model a fixed menu of lookups: the
// model chooses one and its arguments, and never composes a command. It also
// collects each tool's feed, the things there that need the user, with no
// model involved. Every read is a fixed command line.
package integrations

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// Tool is one lookup the model may ask for.
type Tool struct {
	Name        string
	Description string
	// Params is the JSON schema of the arguments object.
	Params json.RawMessage
	// Run performs the lookup from dir, a session's working directory, and
	// returns what the model should read.
	Run func(ctx context.Context, dir string, args json.RawMessage) (string, error)
	// Label names one lookup for the user, such as "GitHub PR #59".
	Label func(args json.RawMessage) string
}

// Integration is a tool the user has, and the lookups it offers.
type Integration struct {
	ID    string
	Name  string
	About string
	Tools []Tool
	// Check reports whether the tool can be used on this machine, and why not.
	Check func(ctx context.Context) (ok bool, detail string)
	// Scope tells the model what can be looked up from dir, a session's
	// working directory. ok is false when nothing can be.
	Scope func(dir string) (note string, ok bool)
	// Feed collects what the tool holds for the user right now. Nil when the
	// tool has no feed.
	Feed func(ctx context.Context) (Found, error)
	// Watches says, for the user, what the feed surfaces.
	Watches []string
	// Clones names the repositories, as feed items name them, checked out at
	// dir or directly inside it.
	Clones func(dir string) []string
}

// FeedItem is one thing in a tool that needs the user.
type FeedItem struct {
	// Key is the same from one collection to the next for as long as the same
	// thing is needed, and changes when it is needed afresh.
	Key     string
	Kind    string
	Urgency string
	Title   string
	Ask     string
	// Summary states the facts the item rests on.
	Summary string
	URL     string
	// Repo is the repository the item belongs to, as "owner/name".
	Repo string
	At   time.Time
}

// PullRequest is one open pull request the user wrote or is asked to review.
type PullRequest struct {
	// Repo is the repository it belongs to, as "owner/name".
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	URL    string `json:"url,omitempty"`
	Author string `json:"author"`
	// Mine is true when the user wrote it.
	Mine  bool      `json:"mine"`
	Draft bool      `json:"draft"`
	At    time.Time `json:"at"`
	// Approvals and ChangesRequested count reviewers by their latest verdict.
	Approvals        int `json:"approvals"`
	ChangesRequested int `json:"changesRequested"`
	// Awaiting counts the reviewers asked who have not answered.
	Awaiting int `json:"awaiting"`
	// Checks is "passing", "failing", "running", "none", or "unknown" when the
	// tool did not say.
	Checks string `json:"checks"`
	// Conflicts is true when it cannot merge into its base branch as it stands.
	Conflicts bool `json:"conflicts"`
	// OnYou is true while the next move is the user's.
	OnYou bool `json:"onYou"`
	// Needs says what that move is, or what the pull request is waiting on.
	Needs string `json:"needs"`
}

// Found is what one collection of a feed brought back.
type Found struct {
	// Items are the things that need the user.
	Items []FeedItem
	// PullRequests are the open ones the user is involved in, whether or not
	// one needs them.
	PullRequests []PullRequest
}

// Feed is one integration's feed as a poller sees it.
type Feed struct {
	ID string
	// On is true while the integration is turned on and usable.
	On    bool
	Fetch func(ctx context.Context) (Found, error)
}

// Status is an integration as the UI shows it.
type Status struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	About     string   `json:"about"`
	Available bool     `json:"available"`
	Detail    string   `json:"detail"`
	Enabled   bool     `json:"enabled"`
	Lookups   []string `json:"lookups"`
	Watches   []string `json:"watches"`
}

const checkEvery = time.Minute

// Manager knows which integrations exist, which the user has turned on, and
// which work on this machine.
type Manager struct {
	list []Integration
	// Enabled reports the integrations the user has turned on, by ID.
	Enabled func() map[string]bool

	mu      sync.Mutex
	checked map[string]check
}

type check struct {
	ok     bool
	detail string
	at     time.Time
}

func NewManager(enabled func() map[string]bool, list ...Integration) *Manager {
	return &Manager{list: list, Enabled: enabled, checked: map[string]check{}}
}

func (m *Manager) check(ctx context.Context, in Integration) check {
	m.mu.Lock()
	c, ok := m.checked[in.ID]
	m.mu.Unlock()
	if ok && time.Since(c.at) < checkEvery {
		return c
	}
	c.ok, c.detail = in.Check(ctx)
	c.at = time.Now()
	m.mu.Lock()
	m.checked[in.ID] = c
	m.mu.Unlock()
	return c
}

// Has reports whether an integration with that ID exists.
func (m *Manager) Has(id string) bool {
	for _, in := range m.list {
		if in.ID == id {
			return true
		}
	}
	return false
}

// Recheck forgets what was learned about availability, so the next Status
// or Offer call looks again.
func (m *Manager) Recheck() {
	m.mu.Lock()
	m.checked = map[string]check{}
	m.mu.Unlock()
}

func (m *Manager) Status(ctx context.Context) []Status {
	enabled := m.Enabled()
	out := []Status{}
	for _, in := range m.list {
		c := m.check(ctx, in)
		s := Status{ID: in.ID, Name: in.Name, About: in.About, Available: c.ok, Detail: c.detail, Enabled: enabled[in.ID], Lookups: []string{}, Watches: append([]string{}, in.Watches...)}
		for _, t := range in.Tools {
			s.Lookups = append(s.Lookups, t.Description)
		}
		out = append(out, s)
	}
	return out
}

// Offer returns the lookups available to a session working in dir: those of
// every integration that is turned on, usable on this machine, and has
// something to look up from there. notes say what each can reach.
func (m *Manager) Offer(ctx context.Context, dir string) (tools []Tool, notes []string) {
	enabled := m.Enabled()
	for _, in := range m.list {
		if !enabled[in.ID] || !m.check(ctx, in).ok {
			continue
		}
		if note, ok := in.Scope(dir); ok {
			tools = append(tools, in.Tools...)
			notes = append(notes, note)
		}
	}
	return tools, notes
}

// Feeds lists every integration that has a feed, on or off.
func (m *Manager) Feeds(ctx context.Context) []Feed {
	enabled := m.Enabled()
	var out []Feed
	for _, in := range m.list {
		if in.Feed != nil {
			out = append(out, Feed{ID: in.ID, On: enabled[in.ID] && m.check(ctx, in).ok, Fetch: in.Feed})
		}
	}
	return out
}

// Clones maps each of dirs to the repositories checked out there, as the
// integrations that are turned on name them. Directories holding none are
// left out.
func (m *Manager) Clones(dirs []string) map[string][]string {
	enabled := m.Enabled()
	out := map[string][]string{}
	for _, in := range m.list {
		if !enabled[in.ID] || in.Clones == nil {
			continue
		}
		for _, d := range dirs {
			if found := in.Clones(d); len(found) > 0 {
				out[d] = append(out[d], found...)
			}
		}
	}
	return out
}
