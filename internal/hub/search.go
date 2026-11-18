package hub

import (
	"cmp"
	"slices"
	"strings"
	"time"
)

const (
	// searchHits is how many places a search returns, and sessionHits how
	// many of them may be in one session.
	searchHits  = 40
	sessionHits = 3
	// snippetRunes is how much of a matching text is returned, and
	// snippetLead how much of that may come before the match.
	snippetRunes = 180
	snippetLead  = 50
)

// Hit is one place a search matched: something about a session, open or
// closed, or one moment of its timeline.
type Hit struct {
	SessionID string `json:"sessionId"`
	Closed    bool   `json:"closed"`
	Agent     string `json:"agent"`
	Label     string `json:"label"`
	// Where is "name", "summary", "moment", "prompt" or "reply".
	Where string `json:"where"`
	// Text is the matching text, cut down to around the match.
	Text string `json:"text"`
	// Line is the model's line on the moment that matched, if it wrote one.
	Line string `json:"line,omitempty"`
	// At is when the moment happened, or when the session was last active.
	At time.Time `json:"at"`
}

// Search finds the sessions, open and closed, in which every word of query
// appears in one place: the name, the model's summary, a moment of the
// timeline, or an open session's last exchange. The latest come first.
func (h *Hub) Search(query string) []Hit {
	words := strings.Fields(strings.ToLower(query))
	out := []Hit{}
	if len(words) == 0 {
		return out
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	in := func(base Hit, name string, insight *Insight, timeline []Moment, extra ...Hit) {
		var found []Hit
		try := func(where, text, line string, at time.Time) {
			if snippet, ok := match(text, words); ok && len(found) < sessionHits {
				hit := base
				hit.Where, hit.Text, hit.Line, hit.At = where, snippet, line, cmp.Or(at, base.At)
				found = append(found, hit)
			}
		}
		try("name", name, "", time.Time{})
		if insight != nil {
			try("summary", insight.Title+". "+insight.Summary, "", time.Time{})
		}
		for _, m := range slices.Backward(timeline) {
			try("moment", strings.TrimSpace(m.Prompt+" "+m.Line+" "+m.Summary), m.Line, m.At)
		}
		// The last prompt is the latest moment over again, but for its length.
		inMoment := slices.ContainsFunc(found, func(x Hit) bool { return x.Where == "moment" })
		for _, x := range extra {
			if x.Where != "prompt" || !inMoment {
				try(x.Where, x.Text, "", time.Time{})
			}
		}
		out = append(out, found...)
	}
	for _, s := range h.sessions {
		label, _ := s.label()
		in(Hit{SessionID: s.ID, Agent: s.Agent, Label: label, At: s.ActiveAt}, label, s.Insight, s.Timeline,
			Hit{Where: "prompt", Text: s.LastPrompt}, Hit{Where: "reply", Text: s.LastReply})
	}
	for id, d := range h.archive {
		a := h.archived(id, d)
		in(Hit{SessionID: id, Closed: true, Agent: a.Agent, Label: a.Label, At: a.EndedAt}, a.Label, d.Insight, d.Timeline)
	}
	slices.SortFunc(out, func(a, b Hit) int {
		return cmp.Or(b.At.Compare(a.At), cmp.Compare(a.SessionID, b.SessionID), cmp.Compare(a.Where, b.Where))
	})
	return out[:min(len(out), searchHits)]
}

// match reports whether every word appears in text, whatever its case, and
// returns text cut down to around the first of them.
func match(text string, words []string) (snippet string, ok bool) {
	low := strings.ToLower(text)
	first := len(low)
	for _, w := range words {
		i := strings.Index(low, w)
		if i < 0 {
			return "", false
		}
		first = min(first, i)
	}
	// Lowering can change a text's length in bytes, and then the place is
	// only roughly right; the start of the text is shown instead.
	if len(low) != len(text) {
		first = 0
	}
	r, at := []rune(text), len([]rune(text[:first]))
	from := max(0, at-snippetLead)
	to := min(len(r), from+snippetRunes)
	snippet = strings.Join(strings.Fields(string(r[from:to])), " ")
	if from > 0 {
		snippet = "…" + snippet
	}
	if to < len(r) {
		snippet += "…"
	}
	return snippet, true
}
