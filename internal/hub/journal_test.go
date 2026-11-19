package hub

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/integrations"
)

// midnight is when today began, here.
func midnight() time.Time {
	y, m, d := time.Now().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.Local)
}

// pend has a day wait to be written, though nothing was said on it, and
// returns what the model would be shown.
func pend(t *testing.T, h *Hub, day string) DayLog {
	t.Helper()
	h.mu.Lock()
	w := h.wrapups[day]
	w.Day, w.State, w.At = day, WrapupPending, time.Now()
	h.wrapups[day] = w
	h.mu.Unlock()
	for _, log := range h.Unwrapped() {
		if log.Day.Format(dayLayout) == day {
			return log
		}
	}
	t.Fatalf("%s is not waiting to be written", day)
	return DayLog{}
}

// write has the hub hold an entry for a day as if the model had written it.
func write(t *testing.T, h *Hub, day, headline string, efforts []WrapupEffort, tags ...string) {
	t.Helper()
	h.SetWrapup(pend(t, h, day), headline, efforts, tags, "")
}

func names(tags []Tag) (out []string) {
	for _, t := range tags {
		out = append(out, t.Name+":"+t.Via)
	}
	return out
}

func TestJournalListsItsDaysTheLatestFirst(t *testing.T) {
	h, _, _ := newHub(t)
	write(t, h, "2026-11-16", "An early day", []WrapupEffort{{ID: "e1", Name: "Widgets"}, {ID: "unsorted", Name: "Unsorted"}}, "Refactoring")
	write(t, h, "2026-11-18", "A late day", nil)
	h.AskWrapup("2026-11-17")

	got := h.State().Journal
	if len(got) != 3 || got[0].Day != "2026-11-18" || got[1].Day != "2026-11-17" || got[2].Day != "2026-11-16" {
		t.Fatalf("journal = %+v, want its three days, the latest first", got)
	}
	if d := got[2]; d.State != WrapupReady || d.Headline != "An early day" || !slices.Equal(d.Efforts, []string{"Widgets", "Unsorted"}) || !slices.Equal(names(d.Tags), []string{"refactoring:guess"}) || d.At.IsZero() {
		t.Errorf("a written day is listed as %+v; want its headline, its efforts by name and its tags", d)
	}
	if d := got[1]; d.State != WrapupEmpty || d.Headline != "" {
		t.Errorf("a day with nothing said is listed as %+v, want it empty", d)
	}

	// Asked for again with nothing said on it any more, a day loses what
	// was written and keeps its tags.
	h.AskWrapup("2026-11-16")
	if w, _ := h.Wrapup("2026-11-16"); w.State != WrapupEmpty || w.Headline != "" || w.Efforts != nil || !slices.Equal(names(w.Tags), []string{"refactoring:guess"}) {
		t.Errorf("a written day asked for again with nothing said on it: %+v; want it empty but for its tags", w)
	}
}

func TestTaggingADay(t *testing.T) {
	h, k, st := newHub(t)
	const day = "2026-11-17"
	tags := func(h *Hub) []string {
		w, _ := h.Wrapup(day)
		return names(w.Tags)
	}

	if h.TagDay(day, "auth", true) {
		t.Fatal("a day the journal does not have took a tag")
	}
	write(t, h, day, "A day", nil, " #Code  Review ", "auth", "", "auth", "3", "4", "5", "6")
	if got := tags(h); !slices.Equal(got, []string{"code review:guess", "auth:guess", "3:guess", "4:guess", "5:guess"}) {
		t.Fatalf("the model's tags = %v; want each once, tidied, and no more than %d", got, DayTags)
	}
	if h.TagDay(day, " # ", true) {
		t.Error("a tag with no name was taken")
	}

	if !h.TagDay(day, "Flaky-_ Tests", true) || !h.TagDay(day, "AUTH", true) || !h.TagDay(day, "3", false) || !h.TagDay(day, "never there", false) {
		t.Fatal("TagDay refused a tag for a day the journal has")
	}
	want := []string{"code review:guess", "auth:", "4:guess", "5:guess", "flaky tests:"}
	if got := tags(h); !slices.Equal(got, want) {
		t.Fatalf("tags = %v, want %v: the user's own added, the model's taken over where it stood, and one taken off", got, want)
	}
	if got := tags(New(k, st)); !slices.Equal(got, want) {
		t.Errorf("after a restart tags = %v, want %v", got, want)
	}
	if d := h.State().Journal[0]; !slices.Equal(names(d.Tags), want) {
		t.Errorf("the journal lists the day with tags %v, want %v", names(d.Tags), want)
	}

	// Written again, the day keeps the user's tags, takes the model's new
	// ones, and is not given back what the user took off.
	log := pend(t, h, day)
	if !slices.Equal(log.Dropped, []string{"3"}) || !slices.Equal(log.Tags, []string{"4", "5", "auth", "code review", "flaky tests"}) {
		t.Errorf("the model is to be told of tags %v in use and %v taken off; want every tag in use and the one taken off", log.Tags, log.Dropped)
	}
	h.SetWrapup(log, "A day, again", nil, []string{"3", "auth", "tooling"}, "")
	if got := tags(h); !slices.Equal(got, []string{"auth:", "flaky tests:", "tooling:guess"}) {
		t.Errorf("written again, tags = %v; want the user's two and only the model's new one", got)
	}

	// A tag the user puts back is theirs, and the model may give it again elsewhere.
	h.TagDay(day, "3", true)
	h.TagDay(day, "3", false)
	if w, _ := h.Wrapup(day); len(w.Dropped) != 0 {
		t.Errorf("dropped = %v after the user gave the tag themselves and took it off; want only the model's tags held against it", w.Dropped)
	}
}

func TestTagsInUseAreTheMostUsedFirst(t *testing.T) {
	h, _, _ := newHub(t)
	write(t, h, "2026-11-16", "", nil, "auth", "tooling")
	write(t, h, "2026-11-17", "", nil, "tooling", "billing")
	write(t, h, "2026-11-18", "", nil, "pending")
	if got := pend(t, h, "2026-11-18").Tags; !slices.Equal(got, []string{"tooling", "auth", "billing", "pending"}) {
		t.Errorf("tags in use = %v; want the one two days go by first, then the rest by name", got)
	}
}

func TestSearchingTheJournal(t *testing.T) {
	h, _, _ := newHub(t)
	write(t, h, "2026-11-16", "Flanges shipped", []WrapupEffort{{ID: "e1", Name: "Widgets", Summary: "Flange support landed.",
		Points:   []WrapupPoint{{Text: "Added the retry to the flange uploader"}, {Text: "Fixed the rounding"}},
		Open:     []string{"Review still pending from Tomas"},
		Sessions: []WrapupSession{{ID: "s1", Label: "Uploader work", Pull: "acme/widgets#12"}},
		Pulls:    []WrapupPull{{Key: "acme/widgets#12", Title: "Retry uploads"}}}}, "reliability")
	write(t, h, "2026-11-17", "A quiet day", []WrapupEffort{{ID: "unsorted", Name: "Unsorted", Summary: "Tidied.", Points: []WrapupPoint{{Text: "Fixed the rounding again"}}}})
	write(t, h, "2026-11-18", "Nothing of note", nil)

	for query, want := range map[string][]JournalHit{
		"":                 {},
		"   ":              {},
		"zeppelin":         {},
		"ROUNDING fixed":   {{Day: "2026-11-17", Text: "Fixed the rounding again"}, {Day: "2026-11-16", Text: "Fixed the rounding"}},
		"retry tomas":      {{Day: "2026-11-16", Text: "Added the retry to the flange uploader"}},
		"flange":           {{Day: "2026-11-16", Text: "Flanges shipped"}},
		"reliability":      {{Day: "2026-11-16", Text: "reliability"}},
		"widgets#12":       {{Day: "2026-11-16", Text: "acme/widgets#12 Retry uploads"}},
		"uploader work":    {{Day: "2026-11-16", Text: "Uploader work"}},
		"widgets":          {{Day: "2026-11-16", Text: "Widgets"}},
		"quiet zeppelin":   {},
		"pending shipped":  {{Day: "2026-11-16", Text: "Flanges shipped"}},
		"tomas review day": {},
	} {
		if got := h.SearchJournal(query); !reflect.DeepEqual(got, want) {
			t.Errorf("SearchJournal(%q) = %+v, want %+v", query, got, want)
		}
	}
}

func TestADayNobodyAskedForIsWritten(t *testing.T) {
	h, _, _ := newHub(t)
	today := midnight()
	day := func(back int) string { return today.AddDate(0, 0, -back).Format(dayLayout) }
	state := func(back int) string {
		w, _ := h.Wrapup(day(back))
		return w.State
	}
	var roster []Session
	said := func(id string, at time.Time) {
		roster = append(roster, Session{ID: id, Agent: agent, Status: Idle, Cwd: "/work", LastPrompt: "do " + id, PromptAt: at})
		h.SetRoster(agent, roster)
	}

	if h.AskMissed(time.Now()) {
		t.Fatal("a day was asked for with nothing said on any")
	}
	said("now", today.Add(time.Minute))
	said("old", today.AddDate(0, 0, -missedDays).Add(-time.Minute))
	if h.AskMissed(today.Add(2*time.Minute)) || len(h.State().Journal) != 0 {
		t.Fatalf("asked for %+v; want neither today, which is not over, nor a day more than %d back", h.State().Journal, missedDays)
	}

	said("two", today.AddDate(0, 0, -2).Add(9*time.Hour))
	said("three", today.AddDate(0, 0, -3).Add(9*time.Hour))
	h.SetRoster(agent, roster[:3])
	if !h.AskMissed(time.Now()) || state(2) != WrapupPending || state(3) != "" {
		t.Fatalf("after one asking: two days back %q, three back %q; want the later asked for first, though its session has gone", state(2), state(3))
	}
	if !h.AskMissed(time.Now()) || state(3) != WrapupPending {
		t.Fatalf("after a second asking: three days back %q, want it asked for", state(3))
	}
	if h.AskMissed(time.Now()) {
		t.Fatal("a day already waiting was asked for again")
	}

	// A compaction is nothing said.
	h.mu.Lock()
	h.sessions["now"].Timeline = append(h.sessions["now"].Timeline, Moment{At: today.AddDate(0, 0, -5).Add(time.Hour), Compacted: &Compaction{}})
	h.mu.Unlock()
	if h.AskMissed(time.Now()) {
		t.Fatal("a day on which a session was only compacted was asked for")
	}

	// A day written before the last of what was said in it is written
	// again; one written since is left alone, as is one that failed.
	stand := func(back int, state string, at time.Time) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.wrapups[day(back)] = Wrapup{Day: day(back), State: state, At: at, Headline: "As written"}
	}
	spoke := today.AddDate(0, 0, -2).Add(9 * time.Hour)
	stand(3, WrapupReady, time.Now())
	for _, c := range []struct {
		state string
		at    time.Time
		asked bool
	}{{WrapupReady, spoke.Add(time.Second), false}, {WrapupFailed, spoke.Add(-time.Hour), false}, {WrapupPending, spoke.Add(-time.Hour), false},
		{WrapupReady, spoke.Add(-time.Hour), true}, {WrapupEmpty, spoke.Add(-time.Hour), true}} {
		stand(2, c.state, c.at)
		if got := h.AskMissed(time.Now()); got != c.asked {
			t.Errorf("a day %s since %s before the last of it: asked for = %v, want %v", c.state, spoke.Sub(c.at), got, c.asked)
		}
	}
	if w, _ := h.Wrapup(day(2)); w.State != WrapupPending || w.Headline != "As written" {
		t.Errorf("a day being written again: %+v; want what was written before to stand meanwhile", w)
	}
}

func TestADaysSessionsNameTheirPullRequests(t *testing.T) {
	h, _, _ := newHub(t)
	today := midnight()
	day := today.Format(dayLayout)
	pr := pull("acme/widgets", 12, false, today)
	pr.Title, pr.URL, pr.Branch, pr.Base = "Retry uploads", "https://github.com/acme/widgets/pull/12", "retry", "main"
	h.SetFeed("github", nil, []integrations.PullRequest{pr}, nil)
	h.SetCheckouts(map[string][]string{"/w/widgets": {"acme/widgets"}})
	h.SetRoster(agent, []Session{
		{ID: "on", Agent: agent, Status: Idle, Cwd: "/w/widgets", Branch: "retry", LastPrompt: "add the retry", PromptAt: today.Add(time.Minute)},
		{ID: "off", Agent: agent, Status: Idle, Cwd: "/w/widgets", Branch: "main", LastPrompt: "look around", PromptAt: today.Add(2 * time.Minute)},
	})
	sessions := func() map[string]*WrapupPull {
		t.Helper()
		logs := h.Unwrapped()
		if len(logs) != 1 || len(logs[0].Efforts) != 1 || len(logs[0].Efforts[0].Sessions) != 2 {
			t.Fatalf("waiting = %+v, want the day with its two sessions", logs)
		}
		out := map[string]*WrapupPull{}
		for _, s := range logs[0].Efforts[0].Sessions {
			out[s.ID] = s.Pull
		}
		return out
	}
	want := WrapupPull{Key: "acme/widgets#12", Title: "Retry uploads", URL: "https://github.com/acme/widgets/pull/12"}

	h.AskWrapup(day)
	if got := sessions(); got["on"] == nil || *got["on"] != want || got["off"] != nil {
		t.Fatalf("pull requests = %+v; want the one the session's branch was made into, and none for the other", got)
	}
	h.SetWrapup(h.Unwrapped()[0], "A day", []WrapupEffort{{ID: "unsorted", Name: "Unsorted",
		Sessions: []WrapupSession{{ID: "on", Label: "on", Pull: want.Key}, {ID: "off", Label: "off"}}, Pulls: []WrapupPull{want}}}, nil, "")

	// Merged and no longer listed, it is still what the session worked on.
	h.SetFeed("github", nil, nil, nil)
	h.AskWrapup(day)
	if got := sessions(); got["on"] == nil || *got["on"] != want || got["off"] != nil {
		t.Errorf("once it is no longer listed, pull requests = %+v; want the one the day's entry already named", got)
	}
}
