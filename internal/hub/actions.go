package hub

import (
	"cmp"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	// keptActions is how many of the local model's actions are kept, and
	// recentActions how many of the latest the state carries.
	keptActions   = 1000
	recentActions = 5
)

// What the local model does, as an Action's Kind.
const (
	ActDescribe = "describe"
	ActFile     = "file"
	ActRole     = "role"
	ActNote     = "note"
	ActBrief    = "brief"
	ActWrapup   = "wrapup"
	ActBackfill = "backfill"
)

// Named is something an action was about, by its ID and the name it went
// by when the action was taken.
type Named struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Action is one piece of work the local model did, and what it was about.
type Action struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// At is when it began, and Millis how long it took.
	At     time.Time `json:"at"`
	Millis int64     `json:"millis"`
	Model  string    `json:"model,omitempty"`
	// Outcome is what came of it, in a line, and Error why nothing did.
	Outcome string `json:"outcome,omitempty"`
	Error   string `json:"error,omitempty"`
	// Tries counts the times in a row it has failed, when more than one.
	Tries int `json:"tries,omitempty"`
	// Sessions, Efforts, Pulls and Workspaces are what it was about, and
	// Day the day of the journal it wrote, as "2006-01-02".
	Sessions   []Named      `json:"sessions,omitempty"`
	Efforts    []Named      `json:"efforts,omitempty"`
	Pulls      []WrapupPull `json:"pulls,omitempty"`
	Workspaces []Named      `json:"workspaces,omitempty"`
	Day        string       `json:"day,omitempty"`
	// Lookups names what the model looked up through an integration to do it.
	Lookups []string `json:"lookups,omitempty"`
}

// loadActions restores the actions the cache holds, the oldest first.
func (h *Hub) loadActions() {
	h.warn(h.cache.Each("action", func(_ string, body []byte) {
		var a Action
		if json.Unmarshal(body, &a) == nil && a.ID != "" {
			h.actions = append(h.actions, a)
		}
	}))
	slices.SortStableFunc(h.actions, func(a, b Action) int { return a.At.Compare(b.At) })
	h.trimActions()
}

// trimActions forgets the oldest actions beyond those that are kept.
func (h *Hub) trimActions() {
	for len(h.actions) > keptActions {
		h.warn(h.cache.Delete("action", h.actions[0].ID))
		h.actions = h.actions[1:]
	}
}

// Acted records a piece of work the local model did. What a names by ID
// alone is given the name it goes by, and what it names brings what goes
// with it: a session its effort, the pull request it is working on and the
// workspace it is in, and a pull request its effort and, when a names no
// session, the sessions working on it. Work that fails again takes the
// place of its last failure, so that one piece of work failing over and
// over does not crowd out the rest.
func (h *Hub) Acted(a Action) {
	h.mu.Lock()
	defer h.mu.Unlock()
	a.ID = newID()
	h.about(&a)
	for i, old := range slices.Backward(h.actions) {
		if old.Kind != a.Kind || old.subject() != a.subject() {
			continue
		}
		if a.Error != "" && old.Error != "" {
			a.Tries = max(old.Tries, 1) + 1
			h.warn(h.cache.Delete("action", old.ID))
			h.actions = slices.Delete(h.actions, i, i+1)
		}
		break
	}
	h.actions = append(h.actions, a)
	h.warn(h.cache.Put("action", a.ID, a))
	h.trimActions()
	h.last = ""
	h.changed()
}

// subject names what an action was chiefly about, which tells one piece of
// work tried again from another of its kind.
func (a Action) subject() string {
	switch {
	case a.Day != "":
		return a.Day
	case a.Kind == ActBrief && len(a.Pulls) > 0:
		return a.Pulls[0].Key
	case a.Kind == ActNote && len(a.Efforts) > 0:
		return a.Efforts[0].ID
	case len(a.Sessions) > 0:
		return a.Sessions[0].ID
	}
	return ""
}

// Actions returns what the local model has done, the latest first.
func (h *Hub) Actions() []Action {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.acted(len(h.actions))
}

// acted returns the latest n actions, the latest first.
func (h *Hub) acted(n int) []Action {
	out := append([]Action{}, h.actions[max(0, len(h.actions)-n):]...)
	slices.Reverse(out)
	return out
}

func named(list []Named, id string) bool {
	return slices.ContainsFunc(list, func(n Named) bool { return n.ID == id })
}

// about completes what an action says it was about.
func (h *Hub) about(a *Action) {
	listed := h.pulls()
	efforts := map[string]string{}
	for _, e := range h.kb.Efforts() {
		efforts[e.ID] = e.Name
	}
	file := func(id, name string) {
		if name = cmp.Or(name, efforts[id]); id != "" && name != "" && !named(a.Efforts, id) {
			a.Efforts = append(a.Efforts, Named{ID: id, Name: name})
		}
	}
	given := a.Efforts
	a.Efforts = nil
	for _, e := range given {
		file(e.ID, e.Name)
	}

	alone := len(a.Sessions) == 0
	for i, p := range a.Pulls {
		for _, l := range listed {
			if strings.EqualFold(pullName(l.PullRequest), p.Key) {
				a.Pulls[i].Title, a.Pulls[i].URL = cmp.Or(p.Title, l.Title), cmp.Or(p.URL, l.URL)
			}
		}
		repo, _, _ := strings.Cut(p.Key, "#")
		id, _ := h.filePull(repo, p.Key)
		file(id, "")
		for _, s := range h.sessions {
			if alone && strings.EqualFold(h.pullOn(s, listed), p.Key) {
				a.Sessions = append(a.Sessions, Named{ID: s.ID})
			}
		}
	}
	if alone {
		slices.SortFunc(a.Sessions, func(x, y Named) int { return cmp.Compare(x.ID, y.ID) })
	}

	workspaces := h.kb.Knowledge().Workspaces
	for i, n := range a.Sessions {
		var label, cwd, effort string
		if s, ok := h.sessions[n.ID]; ok {
			label, _ = s.label()
			cwd = s.Cwd
			effort, _ = h.filed(s)
			if p, ok := h.pullAt(s.Cwd, s.Branch, listed); ok && !slices.ContainsFunc(a.Pulls, func(x WrapupPull) bool { return strings.EqualFold(x.Key, pullName(p.PullRequest)) }) {
				a.Pulls = append(a.Pulls, WrapupPull{Key: pullName(p.PullRequest), Title: p.Title, URL: p.URL})
			}
		} else if d, ok := h.archive[n.ID]; ok {
			label, cwd, effort = d.Label, d.Cwd, d.Effort
		} else if d, ok := h.unsettled[n.ID]; ok {
			label, cwd, effort = d.Label, d.Cwd, d.Effort
		}
		a.Sessions[i].Name = cmp.Or(n.Name, label, n.ID[:min(8, len(n.ID))])
		file(effort, "")
		var in Named
		deepest := 0
		for _, w := range workspaces {
			dir := filepath.Clean(w.Dir)
			if (cwd == dir || strings.HasPrefix(cwd, dir+string(filepath.Separator))) && len(dir) > deepest {
				in, deepest = Named{ID: w.ID, Name: w.Name}, len(dir)
			}
		}
		if in.ID != "" && !named(a.Workspaces, in.ID) {
			a.Workspaces = append(a.Workspaces, in)
		}
	}
}
