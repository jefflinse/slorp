package hub

import (
	"cmp"
	"maps"
	"slices"
	"strings"
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
	// Subagents are the retained or source-record agents it started, included
	// only when one closed session is asked after.
	Subagents []Subagent `json:"subagents,omitempty"`
	// Used and CostUSD are what it had taken when it closed, zero for a
	// session stored before that was kept.
	Used    Tokens  `json:"used,omitzero"`
	CostUSD float64 `json:"costUsd,omitempty"`
}

// Past is what an effort has behind it.
type Past struct {
	// Days are the days of the journal the effort moved on, the latest
	// first, each as the journal has it.
	Days []PastDay `json:"days"`
	// Closed are the sessions filed under it that have closed, the latest
	// to close first and without their timelines, of which there are
	// ClosedCount in all.
	Closed      []Archived `json:"closed"`
	ClosedCount int        `json:"closedCount"`
	// Used and CostUSD are what all its closed sessions took between them.
	Used    Tokens  `json:"used"`
	CostUSD float64 `json:"costUsd"`
}

// PastDay is an effort's part of one day of the journal.
type PastDay struct {
	Day string `json:"day"`
	WrapupEffort
}

// pastDays and pastClosed are how much of an effort's past is given.
const pastDays, pastClosed = 14, 50

// Past returns what the effort has behind it, and false when there is no
// such effort.
func (h *Hub) Past(effortID string) (Past, bool) {
	if !h.kb.Has(effortID) {
		return Past{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	p := Past{Days: []PastDay{}, Closed: []Archived{}}
	for _, day := range slices.Backward(slices.Sorted(maps.Keys(h.wrapups))) {
		if len(p.Days) == pastDays {
			break
		}
		// A day has more than one part for an effort that others were merged into.
		var part *PastDay
		for _, e := range h.wrapups[day].Efforts {
			switch {
			case e.ID != effortID:
			case part == nil:
				part = &PastDay{Day: day, WrapupEffort: e}
			default:
				part.Summary = strings.TrimSpace(part.Summary + " " + e.Summary)
				part.Points, part.Open = slices.Concat(part.Points, e.Points), slices.Concat(part.Open, e.Open)
				part.Sessions, part.Pulls = slices.Concat(part.Sessions, e.Sessions), slices.Concat(part.Pulls, e.Pulls)
			}
		}
		if part != nil {
			p.Days = append(p.Days, *part)
		}
	}
	for id, d := range h.archive {
		if d.Effort != effortID {
			continue
		}
		p.Closed = append(p.Closed, h.archived(id, d))
		p.Used.In, p.Used.CacheWrite, p.Used.CacheRead, p.Used.Out = p.Used.In+d.Used.In, p.Used.CacheWrite+d.Used.CacheWrite, p.Used.CacheRead+d.Used.CacheRead, p.Used.Out+d.Used.Out
		p.CostUSD += d.CostUSD
	}
	slices.SortFunc(p.Closed, func(a, b Archived) int { return cmp.Or(b.EndedAt.Compare(a.EndedAt), cmp.Compare(a.ID, b.ID)) })
	p.ClosedCount = len(p.Closed)
	p.Closed = p.Closed[:min(len(p.Closed), pastClosed)]
	return p, true
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
	a.Subagents = h.mergeSubagents(id, d.Agent, nil, false)
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
		StartedAt: d.StartedAt, EndedAt: d.Ended, Moments: len(d.Timeline), Used: d.Used, CostUSD: d.CostUSD}
	if h.kb.Has(d.Effort) {
		a.Effort = d.Effort
	}
	if d.Insight != nil {
		a.Title, a.Summary = d.Insight.Title, d.Insight.Summary
	}
	return a
}

// leave stores a session as it goes and archives it if it has work to retain.
func (h *Hub) leave(s *Session) {
	for id, ref := range h.permissionRoots {
		if ref.root == s.ID {
			h.settle(id)
		}
	}
	delete(h.backfill, s.ID)
	d := h.doc(s)
	d.Ended = time.Now()
	h.shelve(s.ID, d)
}

func (h *Hub) shelve(id string, d sessionDoc) {
	h.warn(h.cache.Put("session", id, d))
	if h.worthKeeping(id, d) {
		h.archive[id] = d
		h.last = ""
	}
}

func (h *Hub) worthKeeping(id string, d sessionDoc) bool {
	if d.Managed || d.Insight != nil || len(d.Timeline) > 0 {
		return true
	}
	for _, r := range h.retained[id] {
		if r.Agent == d.Agent {
			return true
		}
	}
	return false
}

// sweep archives the sessions of an agent that were open when Slorp last
// stopped and have not been seen since, as of the last that is known of
// each. It is for once the agent's open sessions are known; one seen
// again was settled as it was restored.
func (h *Hub) sweep(agent string) {
	for id, d := range h.unsettled {
		if _, held := h.unread[id]; held || d.Agent != agent {
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
