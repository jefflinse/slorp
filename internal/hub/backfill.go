package hub

import (
	"maps"
	"slices"
	"strings"
	"time"
)

const (
	// BackfillLimit is how many of a session's exchanges a backfill takes.
	BackfillLimit = timelineLimit
	// backfillBatch is how many exchanges the model is shown at once.
	backfillBatch = 5
)

// Exchange is one thing the user said in a session and the agent's answer
// to it, as the session's own record has them. Reply is empty while the
// agent has not answered. With Compacted set it is instead a compaction the
// record holds, and nothing was said.
type Exchange struct {
	At        time.Time
	Prompt    string
	Reply     string
	Compacted *Compaction
}

// Unfilled is some of a session's earlier exchanges, waiting for the model
// to say how each came out.
type Unfilled struct {
	SessionID string
	Label     string
	Exchanges []Exchange
}

// Filled is the model's line on one exchange of an Unfilled, by its place
// in that list.
type Filled struct {
	N       int
	Line    string
	Summary string
}

// Backfill rebuilds a session's timeline from its exchanges and compactions,
// oldest first, keeping what the model has already written about any of
// them, and queues the exchanges that were answered but never described. It
// returns how many are queued, and false when the session is not open.
func (h *Hub) Backfill(sessionID string, exchanges []Exchange) (queued int, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.sessions[sessionID]
	if !ok {
		return 0, false
	}
	exchanges = exchanges[max(0, len(exchanges)-BackfillLimit):]
	had := slices.Clone(s.Timeline)
	var timeline []Moment
	var queue []Exchange
	for _, x := range exchanges {
		if c := x.Compacted; c != nil {
			had = slices.DeleteFunc(had, func(old Moment) bool { return old.Compacted != nil && old.At.Equal(c.At) })
			timeline = append(timeline, Moment{At: c.At, Compacted: c})
			continue
		}
		m := Moment{At: x.At, Prompt: momentPrompt(x.Prompt)}
		if m.Prompt == "" {
			continue
		}
		if i := slices.IndexFunc(had, func(old Moment) bool { return old.Compacted == nil && samePrompt(old.Prompt, m.Prompt) }); i >= 0 {
			m.Line, m.Summary = had[i].Line, had[i].Summary
			had = slices.Delete(had, i, i+1)
		}
		if m.Line == "" && x.Reply != "" {
			queue = append(queue, x)
		}
		timeline = append(timeline, m)
	}
	// What the record does not have, such as a prompt it was read too soon
	// to hold, stays where it was.
	timeline = append(timeline, had...)
	slices.SortStableFunc(timeline, func(a, b Moment) int { return a.At.Compare(b.At) })
	s.Timeline = timeline[max(0, len(timeline)-timelineLimit):]
	if h.backfill == nil {
		h.backfill = map[string][]Exchange{}
	}
	h.backfill[sessionID] = queue
	h.remember(s)
	h.last = ""
	h.changed()
	return len(queue), true
}

// Unfilled returns the next few exchanges of each session being backfilled.
func (h *Hub) Unfilled() []Unfilled {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Unfilled
	for _, id := range slices.Sorted(maps.Keys(h.backfill)) {
		s, open := h.sessions[id]
		if queue := h.backfill[id]; open && len(queue) > 0 {
			label, _ := s.label()
			out = append(out, Unfilled{SessionID: id, Label: label, Exchanges: slices.Clone(queue[:min(len(queue), backfillBatch)])})
		}
	}
	return out
}

// SetFilled records what the model wrote about the exchanges of u, and
// takes them off the session's queue whether or not it wrote about each.
func (h *Hub) SetFilled(u Unfilled, lines []Filled) {
	h.mu.Lock()
	defer h.mu.Unlock()
	tried := func(x Exchange) bool {
		return slices.ContainsFunc(u.Exchanges, func(t Exchange) bool { return t.At.Equal(x.At) && t.Prompt == x.Prompt })
	}
	h.backfill[u.SessionID] = slices.DeleteFunc(h.backfill[u.SessionID], tried)
	s, ok := h.sessions[u.SessionID]
	if !ok {
		return
	}
	s.Timeline = slices.Clone(s.Timeline)
	for _, f := range lines {
		if f.N < 0 || f.N >= len(u.Exchanges) || strings.TrimSpace(f.Line) == "" {
			continue
		}
		x := u.Exchanges[f.N]
		at := func(m Moment) bool { return m.At.Equal(x.At) && samePrompt(m.Prompt, momentPrompt(x.Prompt)) }
		if i := slices.IndexFunc(s.Timeline, at); i >= 0 && s.Timeline[i].Line == "" {
			s.Timeline[i].Line, s.Timeline[i].Summary = strings.TrimSpace(f.Line), strings.TrimSpace(f.Summary)
		}
	}
	h.remember(s)
	h.last = ""
	h.changed()
}

// momentPrompt is what a moment keeps of what the user said.
func momentPrompt(said string) string {
	r := []rune(strings.Join(strings.Fields(said), " "))
	return string(r[:min(len(r), momentLimit)])
}
