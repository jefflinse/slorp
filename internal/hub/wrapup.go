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
)

// Wrapup is the model's account of one day across the user's sessions.
type Wrapup struct {
	Day string `json:"day"`
	// State is WrapupPending until the model has written it, then
	// WrapupReady or WrapupFailed; or WrapupEmpty from the start.
	State string `json:"state"`
	// At is when it was asked for, and then when it was written.
	At       time.Time      `json:"at"`
	Headline string         `json:"headline,omitempty"`
	Efforts  []WrapupEffort `json:"efforts,omitempty"`
	Error    string         `json:"error,omitempty"`
	// Moments is how many moments the day held when it was asked for.
	Moments int `json:"moments"`
}

// WrapupEffort is what moved in one effort that day.
type WrapupEffort struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Summary string   `json:"summary"`
	Points  []string `json:"points"`
	// Open is what was left unfinished or waiting.
	Open []string `json:"open"`
}

// DayLog is what the user said in one day's sessions and how each exchange
// came out, by effort, for the model to wrap up.
type DayLog struct {
	Day time.Time
	// Key names this asking, and differs when the day is asked for again.
	Key     string
	Efforts []DayEffort
}

// DayEffort is one effort's part of a day. Sessions filed nowhere come
// under the ID "unsorted".
type DayEffort struct {
	ID, Name, About string
	Sessions        []DaySession
}

type DaySession struct {
	Label   string
	Moments []Moment
}

// AskWrapup asks for the day named as "2006-01-02" to be wrapped up, in
// place of whatever was written for it before. It reports whether that
// names a day.
func (h *Hub) AskWrapup(day string) bool {
	start, err := time.ParseInLocation(dayLayout, day, time.Local)
	if err != nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	w := Wrapup{Day: day, State: WrapupPending, At: time.Now()}
	for _, e := range h.dayLog(start).Efforts {
		for _, s := range e.Sessions {
			w.Moments += len(s.Moments)
		}
	}
	if w.Moments == 0 {
		w.State = WrapupEmpty
	}
	h.keepWrapup(w)
	return true
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
			out = append(out, log)
		}
	}
	return out
}

// SetWrapup records what the model wrote for an asking of a day, unless
// the day has been asked for again since. A non-empty failure records
// that it could not be written.
func (h *Hub) SetWrapup(log DayLog, headline string, efforts []WrapupEffort, failure string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	day := log.Day.Format(dayLayout)
	w, ok := h.wrapups[day]
	if !ok || w.State != WrapupPending || log.Key != day+"@"+strconv.FormatInt(w.At.UnixNano(), 10) {
		return
	}
	w.State, w.At, w.Headline, w.Efforts = WrapupReady, time.Now(), headline, efforts
	if failure != "" {
		w.State, w.Error = WrapupFailed, failure
	}
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
	add := func(effort, label string, timeline []Moment) {
		var day []Moment
		for _, m := range timeline {
			if !m.At.Before(start) && m.At.Before(end) {
				day = append(day, m)
			}
		}
		if len(day) > 0 {
			bySession[effort] = append(bySession[effort], DaySession{Label: label, Moments: day[max(0, len(day)-wrapSessionMoments):]})
		}
	}
	for _, s := range h.sessions {
		label, _ := s.label()
		effort, _ := h.filed(s)
		add(effort, label, s.Timeline)
	}
	h.warn(h.cache.Each("session", func(id string, body []byte) {
		var d sessionDoc
		if _, open := h.sessions[id]; open || json.Unmarshal(body, &d) != nil {
			return
		}
		if !h.kb.Has(d.Effort) {
			d.Effort = ""
		}
		add(d.Effort, cmp.Or(d.Label, id[:min(8, len(id))]), d.Timeline)
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
