package hub

import (
	"cmp"
	"slices"
)

// lineageDepth is how far back a session is followed through the sessions
// it continues.
const lineageDepth = 64

// Link records that the session id continues the session from, or with an
// empty from that it continues none. What is first recorded of a session
// stands. It reports whether nothing is recorded of from yet.
func (h *Hub) Link(id, from string) (untraced bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if from == id {
		from = ""
	}
	if _, ok := h.origins[id]; !ok {
		h.origins[id] = from
		h.warn(h.cache.Put("origin", id, from))
	}
	_, known := h.origins[from]
	return from != "" && !known
}

// Untraced lists the closed sessions of agent that Link has recorded
// nothing of.
func (h *Hub) Untraced(agent string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, docs := range []map[string]sessionDoc{h.archive, h.unsettled} {
		for id, d := range docs {
			if _, known := h.origins[id]; d.Agent == agent && !known {
				out = append(out, id)
			}
		}
	}
	slices.Sort(out)
	return out
}

// root follows a session back through the sessions it continues to the
// first of them.
func (h *Hub) root(id string) string {
	for range lineageDepth {
		from := h.origins[id]
		if from == "" {
			break
		}
		id = from
	}
	return id
}

// fold gives each open session of agent what the closed sessions it shares
// a root with held, so that a conversation carried across several ids is
// one session. Where several with one root are open, the latest to start
// takes it; where several are closed, the latest to close is heeded first.
func (h *Hub) fold(agent string) {
	if len(h.origins) == 0 {
		return
	}
	heirs := map[string]*Session{}
	for _, s := range h.sessions {
		if s.Agent != agent {
			continue
		}
		r := h.root(s.ID)
		if cur := heirs[r]; cur == nil || cmp.Or(s.StartedAt.Compare(cur.StartedAt), cmp.Compare(s.ID, cur.ID)) > 0 {
			heirs[r] = s
		}
	}
	// Which session takes a root is not known while one reported in it
	// could not be read.
	for id, u := range h.unread {
		if u.agent == agent {
			delete(heirs, h.root(id))
		}
	}
	var closed []string
	for id, d := range h.archive {
		if d.Agent == agent && heirs[h.root(id)] != nil {
			closed = append(closed, id)
		}
	}
	slices.SortFunc(closed, func(a, b string) int {
		return cmp.Or(h.archive[b].Ended.Compare(h.archive[a].Ended), cmp.Compare(a, b))
	})
	for _, id := range closed {
		h.inherit(heirs[h.root(id)], id, h.archive[id])
	}
	// What the user said of a session that left nothing worth keeping
	// still holds for the one that continues it.
	for _, s := range heirs {
		id := s.ID
		for range lineageDepth {
			if id = h.origins[id]; id == "" {
				break
			}
			h.warn(h.kb.Carry(id, s.ID))
		}
	}
}

// inherit gives an open session what a closed one held, where it has none
// of its own, and forgets the closed one.
func (h *Hub) inherit(s *Session, id string, d sessionDoc) {
	s.Timeline = join(d.Timeline, s.Timeline)
	if s.given == "" {
		s.given = d.Given
	}
	if s.pinned == "" {
		s.pinned = d.Pinned
	}
	if s.Insight == nil {
		s.Insight, s.insightKey, s.insightAt = d.Insight, d.InsightKey, d.InsightAt
	}
	if s.guess == "" {
		s.guess, s.guessKey = d.Guess, d.GuessKey
	}
	if s.roleGuess == "" {
		s.roleGuess, s.roleKey = d.RoleGuess, d.RoleKey
	}
	h.warn(h.kb.Carry(id, s.ID))
	for _, r := range h.replies {
		if r.SessionID == id {
			r.SessionID = s.ID
			h.warn(h.cache.Put("reply", r.ID, r))
		}
	}
	delete(h.archive, id)
	h.warn(h.cache.Delete("session", id))
	h.remember(s)
	h.last = ""
}

// join is two timelines as one, oldest first, cut to the latest moments a
// session keeps.
func join(a, b []Moment) []Moment {
	all := slices.Concat(a, b)
	slices.SortStableFunc(all, func(x, y Moment) int { return x.At.Compare(y.At) })
	all = tidy(all)
	return all[max(0, len(all)-timelineLimit):]
}
