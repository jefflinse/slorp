package hub

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/jefflinse/slorp/internal/kb"
	"github.com/jefflinse/slorp/internal/store"
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
	// One that could not be taken up keeps those closed before it from
	// being heeded first.
	stuck, current := map[*Session]bool{}, map[*Session]bool{}
	for _, id := range closed {
		heir := heirs[h.root(id)]
		if stuck[heir] {
			continue
		}
		if !current[heir] {
			h.track(heir, time.Now())
			current[heir] = true
		}
		if err := h.inherit(heir, id, h.archive[id]); err != nil {
			h.warn(err)
			stuck[heir] = true
		}
	}
	// What the user said of a session that left nothing worth keeping
	// still holds for the one that continues it.
	for _, s := range heirs {
		if stuck[s] {
			continue
		}
		filed, _ := h.filed(s)
		id := s.ID
		for range lineageDepth {
			if id = h.origins[id]; id == "" {
				break
			}
			_, open := h.sessions[id]
			_, closed := h.archive[id]
			_, held := h.unread[id]
			if !open && !closed && !held {
				h.warn(h.kb.Carry(id, s.ID))
			}
		}
		if now, _ := h.filed(s); now != filed {
			h.remember(s)
		}
	}
}

// inherit gives an open session what a closed one held, where it has none
// of its own, and forgets the closed one. The open one as it then is, the
// closed one's filing and the replies waiting for it, and its going are
// saved in one write: it is taken up whole or, with an error, not at all.
func (h *Hub) inherit(s *Session, id string, d sessionDoc) error {
	next := *s
	next.Timeline = join(d.Timeline, s.Timeline)
	if next.given == "" {
		next.given = d.Given
	}
	if next.pinned == "" {
		next.pinned = d.Pinned
	}
	if next.Insight == nil {
		next.Insight, next.insightKey, next.insightAt = d.Insight, d.InsightKey, d.InsightAt
	}
	if next.guess == "" {
		next.guess, next.guessKey = d.Guess, d.GuessKey
	}
	if next.roleGuess == "" {
		next.roleGuess, next.roleKey = d.RoleGuess, d.RoleKey
	}
	next.took = slices.Clone(next.took)
	for _, t := range append([]Taken{h.handed(id, d)}, d.Took...) {
		if t.ID == s.ID {
			continue
		}
		i := slices.IndexFunc(next.took, func(o Taken) bool { return o.ID == t.ID })
		if i < 0 {
			next.took = append(next.took, t)
			continue
		}
		next.took[i].Children = slices.Clone(next.took[i].Children)
		for _, a := range t.Children {
			if j := slices.IndexFunc(next.took[i].Children, func(o Subagent) bool { return o.ID == a.ID }); j >= 0 {
				next.took[i].Children[j] = a
			} else {
				next.took[i].Children = append(next.took[i].Children, a)
			}
		}
	}
	// The retained subagents handed over become the open one's own, as
	// they stand.
	var copied []subagentRecord
	for child, r := range h.retained[id] {
		if own, ok := h.retained[s.ID][child]; ok && own.Revision >= r.Revision {
			continue
		}
		r.SessionID, r.Handed = s.ID, true
		copied = append(copied, r)
	}
	var waiting []*Reply
	err := h.kb.Carrying(id, s.ID, func(kind, kbID string, kbDoc any, then kb.Staged) error {
		doc := docIn(then, &next)
		kept, err := fingerprint(doc)
		if err != nil {
			return err
		}
		docs := []store.Doc{{Kind: kind, ID: kbID, Value: kbDoc}, {Kind: "session", ID: s.ID, Value: doc}, {Kind: "session", ID: id, Delete: true}}
		for _, r := range h.replies {
			if r.SessionID == id {
				moved := *r
				moved.SessionID = s.ID
				docs, waiting = append(docs, store.Doc{Kind: "reply", ID: r.ID, Value: moved}), append(waiting, r)
			}
		}
		for _, r := range copied {
			docs = append(docs, store.Doc{Kind: "subagent", ID: subagentKey(s.ID, r.Subagent.ID), Value: r})
		}
		if err := h.cache.PutAll(docs...); err != nil {
			return err
		}
		next.persisted, next.indexed = kept, childKey(doc.Children)
		return nil
	})
	if err != nil {
		return fmt.Errorf("continuing %s in %s: %w", id, s.ID, err)
	}
	*s = next
	for _, r := range waiting {
		r.SessionID = s.ID
	}
	for _, r := range copied {
		if h.retained[s.ID] == nil {
			h.retained[s.ID] = map[string]subagentRecord{}
		}
		h.retained[s.ID][r.Subagent.ID] = r
	}
	delete(h.archive, id)
	h.last = ""
	return nil
}

// handed is what a closed session hands over as it is taken up: its own
// subagents, as last seen and as retained.
func (h *Hub) handed(id string, d sessionDoc) Taken {
	t := Taken{ID: id, Children: slices.Clone(d.Children)}
	for _, r := range h.retained[id] {
		if !slices.ContainsFunc(t.Children, func(a Subagent) bool { return a.ID == r.Subagent.ID }) {
			t.Children = append(t.Children, childIndex([]Subagent{r.Subagent})...)
		}
	}
	return t
}

// took is what a session, open or closed, was handed of those it took up.
func (h *Hub) tookOf(id string) []Taken {
	if s := h.sessions[id]; s != nil {
		return s.took
	}
	return h.archive[id].Took
}

// SubagentSessions lists the session ids under which a subagent of a
// session may have been started: its own, then each it took up that handed
// that subagent over.
func (h *Hub) SubagentSessions(id, child string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []string{id}
	for _, t := range h.tookOf(id) {
		if slices.ContainsFunc(t.Children, func(a Subagent) bool { return a.ID == child }) {
			out = append(out, t.ID)
		}
	}
	return out
}

// join is two timelines as one, oldest first, cut to the latest moments a
// session keeps.
func join(a, b []Moment) []Moment {
	all := slices.Concat(a, b)
	slices.SortStableFunc(all, func(x, y Moment) int { return x.At.Compare(y.At) })
	all = tidy(all)
	return all[max(0, len(all)-timelineLimit):]
}
