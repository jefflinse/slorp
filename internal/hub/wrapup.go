package hub

import (
	"cmp"
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"time"
)

const (
	WrapupPending = "pending"
	WrapupReady   = "ready"
	WrapupFailed  = "failed"
	// WrapupEmpty is a day on which nothing was said in any session.
	WrapupEmpty = "empty"

	// dayLayout is how a day is named, in this machine's own time.
	dayLayout = "2006-01-02"
	// wrapSessionMoments is how many of one session's moments, the latest
	// of the day, a wrap-up is written from.
	wrapSessionMoments = 25
	// DayTags is how many tags the model may give a day.
	DayTags = 5
)

// Wrapup is the model's account of one day across the user's sessions, and
// that day's entry in the journal.
type Wrapup struct {
	Day string `json:"day"`
	// State is WrapupPending until the model has written it, then
	// WrapupReady or WrapupFailed; or WrapupEmpty from the start.
	State string `json:"state"`
	// At is when it was asked for, and then when it was written.
	At time.Time `json:"at"`
	// Headline and Efforts are what was last written, which stands while
	// the day is being written again and if that fails.
	Headline string         `json:"headline,omitempty"`
	Efforts  []WrapupEffort `json:"efforts,omitempty"`
	Error    string         `json:"error,omitempty"`
	// Moments is how many moments the day held when it was asked for.
	Moments int `json:"moments"`
	// Tags are what the day was about, for finding it again.
	Tags []Tag `json:"tags,omitempty"`
	// Dropped are the tags of the model's that the user took off, which it
	// is not to give the day again.
	Dropped []string `json:"dropped,omitempty"`
}

// Tag is one thing a day was about.
type Tag struct {
	Name string `json:"name"`
	// Via is ViaGuess for a tag the model gave, and empty for the user's own.
	Via string `json:"via,omitempty"`
}

// WrapupEffort is what moved in one effort that day.
type WrapupEffort struct {
	ID      string        `json:"id"`
	Name    string        `json:"name"`
	Summary string        `json:"summary"`
	Points  []WrapupPoint `json:"points"`
	// Open is what was left unfinished or waiting.
	Open []string `json:"open"`
	// Sessions are the effort's sessions that spoke that day, and Pulls the
	// pull requests they were working on, as known when it was written.
	Sessions []WrapupSession `json:"sessions,omitempty"`
	Pulls    []WrapupPull    `json:"pulls,omitempty"`
}

// WrapupPoint is one thing done, built, fixed or decided.
type WrapupPoint struct {
	Text string `json:"text"`
	// Sessions are the ids of the sessions the model says it came from.
	Sessions []string `json:"sessions,omitempty"`
}

// UnmarshalJSON also reads a point stored as its text alone, as points were
// before they named their sessions.
func (p *WrapupPoint) UnmarshalJSON(b []byte) error {
	if json.Unmarshal(b, &p.Text) == nil {
		return nil
	}
	type plain WrapupPoint
	return json.Unmarshal(b, (*plain)(p))
}

type WrapupSession struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// Pull is the Key of the pull request it was working on, if any.
	Pull string `json:"pull,omitempty"`
}

type WrapupPull struct {
	// Key names it as "owner/name#number".
	Key   string `json:"key"`
	Title string `json:"title"`
	URL   string `json:"url,omitempty"`
}

// DayLog is what the user said in one day's sessions and how each exchange
// came out, by effort, for the model to wrap up.
type DayLog struct {
	Day time.Time
	// Key names this asking, and differs when the day is asked for again.
	Key     string
	Efforts []DayEffort
	// Tags are the tags the journal's other days go by, the most used
	// first, and Dropped those the user took off this day.
	Tags, Dropped []string
}

// DayEffort is one effort's part of a day. Sessions filed nowhere come
// under the ID "unsorted".
type DayEffort struct {
	ID, Name, About string
	Sessions        []DaySession
}

type DaySession struct {
	ID, Label string
	// Pull is the pull request it is working on, or was when the day was
	// last written.
	Pull    *WrapupPull
	Moments []Moment
}

// AskWrapup asks for the day named as "2006-01-02" to be wrapped up. What
// was written for it before stands until it has been, and its tags stay. It
// reports whether that names a day.
func (h *Hub) AskWrapup(day string) bool {
	start, err := time.ParseInLocation(dayLayout, day, time.Local)
	if err != nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.askWrapup(start)
	return true
}

func (h *Hub) askWrapup(start time.Time) {
	w := h.wrapups[start.Format(dayLayout)]
	w.Day, w.State, w.At, w.Error, w.Moments = start.Format(dayLayout), WrapupPending, time.Now(), "", 0
	for _, e := range h.dayLog(start).Efforts {
		for _, s := range e.Sessions {
			w.Moments += len(s.Moments)
		}
	}
	if w.Moments == 0 {
		w.State, w.Headline, w.Efforts = WrapupEmpty, "", nil
	}
	h.keepWrapup(w)
}

// Unwrapped lists the days waiting to be wrapped up.
func (h *Hub) Unwrapped() []DayLog {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []DayLog
	for _, day := range slices.Sorted(maps.Keys(h.wrapups)) {
		if w := h.wrapups[day]; w.State == WrapupPending {
			start, _ := time.ParseInLocation(dayLayout, day, time.Local)
			log := h.dayLog(start)
			log.Key = day + "@" + strconv.FormatInt(w.At.UnixNano(), 10)
			log.Tags, log.Dropped = h.tagsInUse(), slices.Clone(w.Dropped)
			out = append(out, log)
		}
	}
	return out
}

// SetWrapup records what the model wrote for an asking of a day, unless
// the day has been asked for again since. Its tags take the place of those
// the model gave the day before, beside the user's own. A non-empty failure
// records instead that it could not be written.
func (h *Hub) SetWrapup(log DayLog, headline string, efforts []WrapupEffort, tags []string, failure string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	day := log.Day.Format(dayLayout)
	w, ok := h.wrapups[day]
	if !ok || w.State != WrapupPending || log.Key != day+"@"+strconv.FormatInt(w.At.UnixNano(), 10) {
		return
	}
	w.At = time.Now()
	if failure != "" {
		w.State, w.Error = WrapupFailed, failure
		h.keepWrapup(w)
		return
	}
	w.State, w.Headline, w.Efforts = WrapupReady, headline, efforts
	kept := slices.DeleteFunc(slices.Clone(w.Tags), func(t Tag) bool { return t.Via != "" })
	given := 0
	for _, t := range tags {
		name := tagName(t)
		has := func(t Tag) bool { return t.Name == name }
		if name != "" && given < DayTags && !slices.ContainsFunc(kept, has) && !slices.Contains(w.Dropped, name) {
			kept = append(kept, Tag{Name: name, Via: ViaGuess})
			given++
		}
	}
	w.Tags = kept
	h.keepWrapup(w)
}

func (h *Hub) keepWrapup(w Wrapup) {
	h.wrapups[w.Day] = w
	h.warn(h.cache.Put("wrapup", w.Day, w))
	h.last = ""
	h.changed()
}

// dayLog gathers the moments of the day starting at start from the sessions
// that are open and from those that have gone, by the effort each is filed
// under.
func (h *Hub) dayLog(start time.Time) DayLog {
	end := start.AddDate(0, 0, 1)
	bySession := map[string][]DaySession{}
	// A pull request that is no longer listed is still the one a session
	// was working on when the day was last written.
	before, pulls := map[string]*WrapupPull{}, h.pulls()
	for _, e := range h.wrapups[start.Format(dayLayout)].Efforts {
		for _, s := range e.Sessions {
			if at := slices.IndexFunc(e.Pulls, func(p WrapupPull) bool { return p.Key == s.Pull }); at >= 0 {
				before[s.ID] = &e.Pulls[at]
			}
		}
	}
	add := func(effort, id, label, cwd, branch string, timeline []Moment) {
		var day []Moment
		for _, m := range timeline {
			if m.Compacted == nil && !m.At.Before(start) && m.At.Before(end) {
				day = append(day, m)
			}
		}
		if len(day) == 0 {
			return
		}
		pull := before[id]
		if p, ok := h.pullAt(cwd, branch, pulls); ok {
			pull = &WrapupPull{Key: p.Repo + "#" + strconv.Itoa(p.Number), Title: p.Title, URL: p.URL}
		}
		bySession[effort] = append(bySession[effort], DaySession{ID: id, Label: label, Pull: pull, Moments: day[max(0, len(day)-wrapSessionMoments):]})
	}
	for _, s := range h.sessions {
		label, _ := s.label()
		effort, _ := h.filed(s)
		add(effort, s.ID, label, s.Cwd, s.Branch, s.Timeline)
	}
	h.warn(h.cache.Each("session", func(id string, body []byte) {
		var d sessionDoc
		if _, open := h.sessions[id]; open || json.Unmarshal(body, &d) != nil {
			return
		}
		if !h.kb.Has(d.Effort) {
			d.Effort = ""
		}
		add(d.Effort, id, cmp.Or(d.Label, id[:min(8, len(id))]), d.Cwd, d.Branch, d.Timeline)
	}))

	log := DayLog{Day: start}
	file := func(id, key, name, about string) {
		sessions := bySession[key]
		if len(sessions) == 0 {
			return
		}
		slices.SortFunc(sessions, func(a, b DaySession) int {
			return cmp.Or(a.Moments[0].At.Compare(b.Moments[0].At), cmp.Compare(a.Label, b.Label))
		})
		log.Efforts = append(log.Efforts, DayEffort{ID: id, Name: name, About: about, Sessions: sessions})
	}
	for _, e := range h.kb.Efforts() {
		file(e.ID, e.ID, e.Name, e.About)
	}
	file("unsorted", "", "Unsorted", "")
	return log
}
