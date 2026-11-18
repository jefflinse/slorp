package hub

import (
	"cmp"
	"slices"
	"time"
)

// archiveShown is how many closed sessions, the latest to close, are listed.
const archiveShown = 60

// Archived is a session that has closed, as it stood when it did.
type Archived struct {
	ID     string `json:"id"`
	Agent  string `json:"agent"`
	Label  string `json:"label"`
	Effort string `json:"effort"`
	Cwd    string `json:"cwd"`
	Branch string `json:"branch,omitempty"`
	// StartedAt is zero for a session stored before that was kept.
	StartedAt time.Time `json:"startedAt,omitzero"`
	EndedAt   time.Time `json:"endedAt"`
	// Title and Summary are the model's last words on it, if it wrote any.
	Title   string `json:"title,omitempty"`
	Summary string `json:"summary,omitempty"`
	// Moments counts its timeline, which only Archive itself carries.
	Moments  int      `json:"moments"`
	Timeline []Moment `json:"timeline,omitempty"`
}

// Archive returns one closed session with its timeline.
func (h *Hub) Archive(id string) (Archived, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	d, ok := h.archive[id]
	if !ok {
		return Archived{}, false
	}
	a := h.archived(id, d)
	a.Timeline = append([]Moment{}, d.Timeline...)
	return a, true
}

// archives lists the closed sessions without their timelines, the latest to
// close first.
func (h *Hub) archives() []Archived {
	out := make([]Archived, 0, len(h.archive))
	for id, d := range h.archive {
		out = append(out, h.archived(id, d))
	}
	slices.SortFunc(out, func(a, b Archived) int { return cmp.Or(b.EndedAt.Compare(a.EndedAt), cmp.Compare(a.ID, b.ID)) })
	return out[:min(len(out), archiveShown)]
}

func (h *Hub) archived(id string, d sessionDoc) Archived {
	a := Archived{ID: id, Agent: d.Agent, Label: cmp.Or(d.Label, id[:min(8, len(id))]), Cwd: d.Cwd, Branch: d.Branch,
		StartedAt: d.StartedAt, EndedAt: d.Ended, Moments: len(d.Timeline)}
	if h.kb.Has(d.Effort) {
		a.Effort = d.Effort
	}
	if d.Insight != nil {
		a.Title, a.Summary = d.Insight.Title, d.Insight.Summary
	}
	return a
}

// leave stores a session as it goes and files it in the archive, unless
// nothing was ever said in it or about it.
func (h *Hub) leave(s *Session) {
	d := h.doc(s)
	d.Ended = time.Now()
	h.shelve(s.ID, d)
}

func (h *Hub) shelve(id string, d sessionDoc) {
	h.warn(h.cache.Put("session", id, d))
	if d.worthKeeping() {
		h.archive[id] = d
		h.last = ""
	}
}

// worthKeeping reports whether anything was said in the session or about it.
func (d sessionDoc) worthKeeping() bool { return d.Insight != nil || len(d.Timeline) > 0 }

// sweep archives the sessions of an agent that were open when Slorp last
// stopped and have not been seen since, as of the last that is known of
// each. It is for once the agent's open sessions are known; one seen
// again was settled as it was restored.
func (h *Hub) sweep(agent string) {
	for id, d := range h.unsettled {
		if d.Agent != agent {
			continue
		}
		delete(h.unsettled, id)
		d.Ended = d.InsightAt
		if n := len(d.Timeline); n > 0 && d.Timeline[n-1].At.After(d.Ended) {
			d.Ended = d.Timeline[n-1].At
		}
		if d.Ended.IsZero() {
			d.Ended = time.Now()
		}
		h.shelve(id, d)
	}
}
