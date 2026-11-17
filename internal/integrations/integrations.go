// Package integrations gives the local model a fixed menu of read-only
// lookups into the user's other tools. The model chooses a lookup and its
// arguments; it never composes a command. Each lookup runs a fixed command
// line, so nothing here can change anything in the tool it reads.
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
		s := Status{ID: in.ID, Name: in.Name, About: in.About, Available: c.ok, Detail: c.detail, Enabled: enabled[in.ID], Lookups: []string{}}
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
