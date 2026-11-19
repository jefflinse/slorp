package integrations

import (
	"cmp"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Discussion is what has been said and done on one pull request, as far
// back as the tool was asked.
type Discussion struct {
	// Viewer is the name the user goes by there.
	Viewer string `json:"viewer"`
	// Body is the pull request's own description.
	Body      string `json:"body,omitempty"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Files     int    `json:"files"`
	// Comments are those on the pull request as a whole, oldest first.
	Comments []Comment `json:"comments"`
	// Reviews are the verdicts given, oldest first.
	Reviews []Review `json:"reviews"`
	// Threads are the conversations on lines of the change.
	Threads []Thread `json:"threads"`
	// Commits are the latest of them, oldest first, and CommitCount is how
	// many there are in all.
	Commits     []Commit `json:"commits"`
	CommitCount int      `json:"commitCount"`
	// Requested names the reviewers asked who have not answered.
	Requested []string `json:"requested"`
	// Failing names the checks that failed on the newest commit.
	Failing []string `json:"failing"`
}

// Comment is one thing someone said.
type Comment struct {
	Author string `json:"author"`
	// Bot is true when the author is no person.
	Bot  bool      `json:"bot,omitempty"`
	Body string    `json:"body"`
	At   time.Time `json:"at"`
	URL  string    `json:"url,omitempty"`
}

// Review is one reviewer's verdict: "approved", "changes requested",
// "commented" or "dismissed", with what they said in giving it.
type Review struct {
	Comment
	State string `json:"state"`
}

// Thread is a conversation on one line of the change.
type Thread struct {
	Path string `json:"path"`
	Line int    `json:"line,omitempty"`
	// Resolved is true once someone marked it settled, and Outdated once
	// the line it is on has changed.
	Resolved bool      `json:"resolved"`
	Outdated bool      `json:"outdated,omitempty"`
	Comments []Comment `json:"comments"`
}

// Commit is one commit of the pull request.
type Commit struct {
	ID       string    `json:"id"`
	Headline string    `json:"headline"`
	Author   string    `json:"author"`
	At       time.Time `json:"at"`
}

// Kinds of point a discussion holds for the user.
const (
	// PointThread is a line conversation where someone else spoke last.
	PointThread = "thread"
	// PointMention is a comment that names the user.
	PointMention = "mention"
	// PointComment is a comment on the user's own pull request.
	PointComment = "comment"
	// PointReview is a review of the user's own pull request that says
	// something.
	PointReview = "review"
	// PointCommits is commits pushed since the user reviewed.
	PointCommits = "commits"
)

// Point is one thing in a discussion that waits on the user, judged only by
// who spoke last and to whom.
type Point struct {
	// ID stays the same for as long as the same thing waits.
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Who spoke or pushed, and when.
	Who string    `json:"who"`
	At  time.Time `json:"at"`
	URL string    `json:"url,omitempty"`
	// Path and Line say where in the change a thread is.
	Path string `json:"path,omitempty"`
	Line int    `json:"line,omitempty"`
	// Text is what was said last, or for commits what was pushed.
	Text string `json:"text"`
	// Before is what was said in a thread before that, oldest first.
	Before []Comment `json:"before,omitempty"`
}

const (
	// pointLimit is how many points one discussion gives.
	pointLimit = 12
	// pointText is how much of what was said a point keeps, in runes.
	pointText = 1200
	// reviewSpan is how long before a review is given its threads may
	// have been written.
	reviewSpan = 2 * time.Hour
)

func sameLogin(a, b string) bool { return a != "" && strings.EqualFold(a, b) }

// mentioning returns a test of whether a body names login with an @.
func mentioning(login string) func(body string) bool {
	if login == "" {
		return func(string) bool { return false }
	}
	return regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_-])@` + regexp.QuoteMeta(login) + `($|[^A-Za-z0-9-])`).MatchString
}

// lastActed is when the user last said or pushed anything in d, zero when
// they never have.
func (d Discussion) lastActed() time.Time {
	var at time.Time
	seen := func(who string, when time.Time) {
		if sameLogin(who, d.Viewer) && when.After(at) {
			at = when
		}
	}
	for _, c := range d.Comments {
		seen(c.Author, c.At)
	}
	for _, r := range d.Reviews {
		seen(r.Author, r.At)
	}
	for _, t := range d.Threads {
		for _, c := range t.Comments {
			seen(c.Author, c.At)
		}
	}
	for _, c := range d.Commits {
		seen(c.Author, c.At)
	}
	return at
}

// Points picks out what in d waits on the user, the latest first: threads
// they are in, or any on a pull request of their own, where someone else
// spoke last and nobody has marked it settled; comments since they last
// acted that name them, or on their own pull request any from a person;
// reviews of their own pull request since then that say something without
// approving it, unless the reviewer's threads from then already wait; and
// on one they reviewed without approving, the commits pushed since. mine
// says the pull request is the user's own.
func (d Discussion) Points(mine bool) []Point {
	me := func(who string) bool { return sameLogin(who, d.Viewer) }
	named := mentioning(d.Viewer)
	acted := d.lastActed()
	out := []Point{}
	for _, t := range d.Threads {
		n := len(t.Comments)
		if t.Resolved || n == 0 || me(t.Comments[n-1].Author) {
			continue
		}
		if !mine && !slices.ContainsFunc(t.Comments, func(c Comment) bool { return me(c.Author) }) {
			continue
		}
		last := t.Comments[n-1]
		out = append(out, Point{ID: cmp.Or(last.URL, t.Path+":"+strconv.Itoa(t.Line)+"@"+last.At.Format(time.RFC3339)), Kind: PointThread,
			Who: last.Author, At: last.At, URL: last.URL, Path: t.Path, Line: t.Line, Text: clip(last.Body, pointText), Before: slices.Clone(t.Comments[:n-1])})
	}
	said := func(kind string, c Comment) {
		out = append(out, Point{ID: cmp.Or(c.URL, kind+"@"+c.At.Format(time.RFC3339)), Kind: kind, Who: c.Author, At: c.At, URL: c.URL, Text: clip(c.Body, pointText)})
	}
	for _, c := range d.Comments {
		switch {
		case me(c.Author) || !c.At.After(acted):
		case named(c.Body):
			said(PointMention, c)
		case mine && !c.Bot:
			said(PointComment, c)
		}
	}
	// A review's own words are mostly a note covering its threads.
	covered := func(r Review) bool {
		return slices.ContainsFunc(out, func(p Point) bool {
			return p.Kind == PointThread && sameLogin(p.Who, r.Author) && !p.At.After(r.At.Add(time.Minute)) && p.At.After(r.At.Add(-reviewSpan))
		})
	}
	var reviewed *Review
	for i, r := range d.Reviews {
		if me(r.Author) {
			reviewed = &d.Reviews[i]
			continue
		}
		if r.At.After(acted) && strings.TrimSpace(r.Body) != "" && (mine && r.State != "approved" && !covered(r) || named(r.Body)) {
			kind := PointReview
			if !mine {
				kind = PointMention
			}
			said(kind, r.Comment)
		}
	}
	if !mine && reviewed != nil && reviewed.State != "approved" {
		var since []Commit
		for _, c := range d.Commits {
			if c.At.After(reviewed.At) && !me(c.Author) {
				since = append(since, c)
			}
		}
		if n := len(since); n > 0 {
			latest := since[n-1]
			text := latest.Headline
			if n > 1 {
				text = strconv.Itoa(n) + " commits, the latest: " + latest.Headline
			}
			out = append(out, Point{ID: "commits:" + latest.ID, Kind: PointCommits, Who: latest.Author, At: latest.At, Text: text})
		}
	}
	slices.SortStableFunc(out, func(a, b Point) int { return b.At.Compare(a.At) })
	return out[:min(len(out), pointLimit)]
}

// clip cuts s to n runes, marking where it was cut.
func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return strings.TrimSpace(string(r[:n])) + "…"
	}
	return s
}
