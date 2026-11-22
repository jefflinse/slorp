package hub

import (
	"cmp"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode"
)

const (
	// journalHits is how many days a search of the journal returns.
	journalHits = 60
	// tagRunes is how long a tag may be, and tagsOffered how many of the
	// tags in use the model is shown.
	tagRunes    = 32
	tagsOffered = 40
	// missedDays is how many days back a day nobody asked for is still
	// written.
	missedDays = 7
)

// JournalDay is one day of the journal as its list shows it. Wrapup returns
// the whole entry.
type JournalDay struct {
	Day      string    `json:"day"`
	State    string    `json:"state"`
	At       time.Time `json:"at"`
	Headline string    `json:"headline,omitempty"`
	Tags     []Tag     `json:"tags,omitempty"`
	// Efforts names the efforts that moved that day.
	Efforts []string `json:"efforts,omitempty"`
}

// JournalHit is a day of the journal that a search matched.
type JournalHit struct {
	Day string `json:"day"`
	// Text is the part of the entry holding most of what was looked for,
	// cut down to around it.
	Text string `json:"text"`
}

func (h *Hub) journal() []JournalDay {
	out := make([]JournalDay, 0, len(h.wrapups))
	for _, day := range slices.Backward(slices.Sorted(maps.Keys(h.wrapups))) {
		w := h.wrapups[day]
		d := JournalDay{Day: day, State: w.State, At: w.At, Headline: w.Headline, Tags: w.Tags}
		for _, e := range w.Efforts {
			d.Efforts = append(d.Efforts, e.Name)
		}
		out = append(out, d)
	}
	return out
}

// Wrapup returns the journal's entry for a day, and false when the day has
// never been asked for. The hub never changes an entry it has returned.
func (h *Hub) Wrapup(day string) (Wrapup, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	w, ok := h.wrapups[day]
	return w, ok
}

// TagDay gives a day of the journal a tag as the user's own, or takes one
// off, whoever gave it. It reports whether the journal has the day and tag
// names a tag.
func (h *Hub) TagDay(day, tag string, on bool) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	name := tagName(tag)
	w, ok := h.wrapups[day]
	if !ok || name == "" {
		return false
	}
	at := slices.IndexFunc(w.Tags, func(t Tag) bool { return t.Name == name })
	guessed := at >= 0 && w.Tags[at].Via != ""
	w.Tags = slices.DeleteFunc(slices.Clone(w.Tags), func(t Tag) bool { return t.Name == name })
	w.Dropped = slices.DeleteFunc(slices.Clone(w.Dropped), func(d string) bool { return d == name })
	switch {
	case on && at >= 0:
		w.Tags = slices.Insert(w.Tags, at, Tag{Name: name})
	case on:
		w.Tags = append(w.Tags, Tag{Name: name})
	case guessed:
		w.Dropped = append(w.Dropped, name)
	}
	h.keepWrapup(w)
	return true
}

// tagName returns a tag as the journal keeps it, so that one tag has one
// spelling: in lower case, its words parted by single spaces whether they
// came with spaces, hyphens or underscores, with no leading # and at most
// tagRunes runes. Empty names no tag.
func tagName(s string) string {
	words := strings.FieldsFunc(strings.ToLower(strings.TrimLeft(strings.TrimSpace(s), "#")), func(r rune) bool { return unicode.IsSpace(r) || r == '-' || r == '_' })
	s = strings.Join(words, " ")
	if r := []rune(s); len(r) > tagRunes {
		s = strings.TrimSpace(string(r[:tagRunes]))
	}
	return s
}

// tagsInUse lists the tags the journal's days go by, the most used first.
func (h *Hub) tagsInUse() []string {
	days := map[string]int{}
	for _, w := range h.wrapups {
		for _, t := range w.Tags {
			days[t.Name]++
		}
	}
	out := slices.SortedFunc(maps.Keys(days), func(a, b string) int { return cmp.Or(cmp.Compare(days[b], days[a]), cmp.Compare(a, b)) })
	return out[:min(len(out), tagsOffered)]
}

// SearchJournal finds the days whose entry holds every word of query,
// whatever its case, anywhere among its headline, tags, efforts, points,
// what was left open, and the names of its sessions and pull requests. The
// latest come first.
func (h *Hub) SearchJournal(query string) []JournalHit {
	words := strings.Fields(strings.ToLower(query))
	out := []JournalHit{}
	if len(words) == 0 {
		return out
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, day := range slices.Backward(slices.Sorted(maps.Keys(h.wrapups))) {
		texts := h.wrapups[day].texts()
		all := strings.ToLower(strings.Join(texts, "\n"))
		if len(out) == journalHits {
			break
		}
		if slices.ContainsFunc(words, func(w string) bool { return !strings.Contains(all, w) }) {
			continue
		}
		var best string
		var most []string
		for _, text := range texts {
			low := strings.ToLower(text)
			held := slices.DeleteFunc(slices.Clone(words), func(w string) bool { return !strings.Contains(low, w) })
			if len(held) > len(most) {
				best, most = text, held
			}
		}
		snippet, _ := match(best, most)
		out = append(out, JournalHit{Day: day, Text: snippet})
	}
	return out
}

// texts lists everything an entry says, the headline first.
func (w Wrapup) texts() []string {
	out := []string{w.Headline}
	for _, e := range w.Efforts {
		out = append(out, e.Summary)
		for _, p := range e.Points {
			out = append(out, p.Text)
		}
		out = append(out, e.Open...)
		out = append(out, e.Name)
		for _, s := range e.Sessions {
			out = append(out, s.Label)
		}
		for _, p := range e.Pulls {
			out = append(out, p.Key+" "+p.Title)
		}
	}
	for _, t := range w.Tags {
		out = append(out, t.Name)
	}
	return out
}

// AskMissed asks for the latest day wanting an entry that nobody asked for,
// among the missedDays days before the one now falls in: a day on which
// something was said and either nothing was written, or what was written
// came before the last of it. It reports whether there was such a day.
func (h *Hub) AskMissed(now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	year, month, date := now.In(time.Local).Date()
	today := time.Date(year, month, date, 0, 0, 0, 0, time.Local)
	// last is when the last thing was said on each day.
	last := map[string]time.Time{}
	said := func(timeline []Moment) {
		for _, m := range timeline {
			if day := m.At.In(time.Local).Format(dayLayout); m.Compacted == nil && m.At.After(last[day]) {
				last[day] = m.At
			}
		}
	}
	for _, s := range h.sessions {
		said(s.Timeline)
	}
	for _, d := range h.archive {
		said(d.Timeline)
	}
	for _, d := range h.unsettled {
		said(d.Timeline)
	}
	for back := 1; back <= missedDays; back++ {
		start := today.AddDate(0, 0, -back)
		spoke, ok := last[start.Format(dayLayout)]
		if !ok {
			continue
		}
		w, asked := h.wrapups[start.Format(dayLayout)]
		if !asked || ((w.State == WrapupReady || w.State == WrapupEmpty) && w.At.Before(spoke)) {
			h.askWrapup(start)
			return true
		}
	}
	return false
}
