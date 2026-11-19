package hub

import (
	"cmp"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jefflinse/slorp/internal/integrations"
)

// Sorts of point, as the model tells them apart.
const (
	// SortQuestion asks the user something.
	SortQuestion = "question"
	// SortPushback disagrees with, or declines, what the user said.
	SortPushback = "pushback"
	// SortRequest asks the user to change or do something.
	SortRequest = "request"
	// SortDone reports that what the user asked for was done.
	SortDone = "done"
	// SortSettled needs nothing from the user.
	SortSettled = "settled"
)

// Kinds of item raised from a pull request's discussion.
const (
	KindPullQuestion = "pr-question"
	KindPullPushback = "pr-pushback"
)

// discussRetry is how long a discussion that could not be read is left
// before it is asked for again.
const discussRetry = 5 * time.Minute

// PullRef names one pull request an integration listed, and Key what the
// listing last said of it: a discussion or brief made under one key is out
// of date under another.
type PullRef struct {
	Source string
	Repo   string
	Number int
	Key    string
}

func (r PullRef) id() string {
	return r.Source + ":" + strings.ToLower(r.Repo) + "#" + strconv.Itoa(r.Number)
}

// PullBrief is the local model's account of a pull request, from its
// discussion.
type PullBrief struct {
	// Stands says where it stands, and Next whose move it is.
	Stands string `json:"stands"`
	Next   string `json:"next"`
	// Since says what the code pushed since the user reviewed does, empty
	// when nothing was.
	Since string    `json:"since,omitempty"`
	At    time.Time `json:"at"`
}

// Sorted is what the model made of one point: which sort it is, and the
// gist of what is being said.
type Sorted struct {
	Sort string `json:"sort"`
	Gist string `json:"gist"`
}

// PullPoint is one thing in a pull request's discussion that waits on the
// user, with what the model made of it once it has said.
type PullPoint struct {
	integrations.Point
	Sort string `json:"sort,omitempty"`
	Gist string `json:"gist,omitempty"`
}

// pullDoc is what Slorp keeps of one pull request's discussion.
type pullDoc struct {
	// Key is the listing the discussion was read under.
	Key        string                  `json:"key"`
	ReadAt     time.Time               `json:"readAt"`
	Discussion integrations.Discussion `json:"discussion"`
	Brief      *PullBrief              `json:"brief,omitempty"`
	// BriefKey is the listing the brief and sorts were written under.
	BriefKey string `json:"briefKey,omitempty"`
	// Sorts is what the model made of each point, by the point's ID.
	Sorts map[string]Sorted `json:"sorts,omitempty"`
}

// PullDetail is one pull request with the whole of its discussion.
type PullDetail struct {
	PullRequest
	// Discussion is nil until it has been read.
	Discussion *integrations.Discussion `json:"discussion"`
}

// Unbriefed is a pull request whose discussion the model has not yet
// written about.
type Unbriefed struct {
	Ref        PullRef
	Pull       PullRequest
	Discussion integrations.Discussion
	// Points are those waiting on the user, for the model to sort.
	Points []integrations.Point
	// Wanted is true while the user is looking at it.
	Wanted bool
}

func pullRef(p PullRequest) PullRef {
	return PullRef{Source: p.Source, Repo: p.Repo, Number: p.Number, Key: p.At.UTC().Format(time.RFC3339Nano)}
}

// discuss fills in what is known of a pull request's discussion: the points
// waiting on the user, the model's brief, and whether either is behind the
// listing.
func (h *Hub) discuss(p *PullRequest) {
	ref := pullRef(*p)
	d := h.discussed[ref.id()]
	if d == nil {
		p.Points = []PullPoint{}
		return
	}
	p.ReadAt, p.Brief = d.ReadAt, d.Brief
	p.Behind = d.Key != ref.Key || d.BriefKey != d.Key
	p.Points = []PullPoint{}
	open := false
	for _, pt := range d.Discussion.Points(p.Mine) {
		pt.Before = nil
		s := d.Sorts[pt.ID]
		p.Points = append(p.Points, PullPoint{Point: pt, Sort: s.Sort, Gist: s.Gist})
		open = open || s.Sort != SortSettled
	}
	if p.Reviewed && open {
		p.Needs = "You reviewed it, and its author has answered."
	}
}

// raised returns an item for each point of p's discussion the model took
// for a question or for pushback. One on the user's own pull request holds
// up their own work, and one on another's holds up its author.
func raised(p PullRequest) []Item {
	var out []Item
	for _, pt := range p.Points {
		kind, word := KindPullQuestion, "Question"
		switch pt.Sort {
		case SortQuestion:
		case SortPushback:
			kind, word = KindPullPushback, "Pushback"
		default:
			continue
		}
		ref := pullRef(p)
		sum := sha1.Sum([]byte(ref.id() + "\x00" + pt.ID))
		it := Item{ID: p.Source + "-" + hex.EncodeToString(sum[:6]), Kind: kind, Urgency: "blocking", Title: word + ": " + p.Title, Subtitle: pt.Gist,
			Ask: cmp.Or(pt.Gist, pt.Who+" spoke last") + ".", Summary: fmt.Sprintf("%s#%d, %s. %s wrote: %s", p.Repo, p.Number, whose(p), pt.Who, pt.Text),
			Replies: []string{}, At: pt.At, Source: p.Source, URL: cmp.Or(pt.URL, p.URL), Repo: p.Repo, Pull: p.Repo + "#" + strconv.Itoa(p.Number), Point: pt.ID,
			Effort: p.Effort, EffortVia: p.EffortVia}
		if p.Mine {
			it.Urgency = "awaiting"
		}
		out = append(out, it)
	}
	return out
}

// whose says whose pull request p is, as an item's summary puts it.
func whose(p PullRequest) string {
	if p.Mine {
		return "opened by you"
	}
	return "opened by " + cmp.Or(p.Author, "someone")
}

// ranked orders pull requests for work on their discussions: the one the
// user is looking at, then those whose move is the user's, then the latest
// changed.
func (h *Hub) ranked() []PullRequest {
	pulls := h.pulls()
	slices.SortStableFunc(pulls, func(a, b PullRequest) int {
		wanted := func(p PullRequest) int {
			if pullRef(p).id() == h.wantedPull {
				return 0
			}
			return 1
		}
		return wanted(a) - wanted(b)
	})
	return pulls
}

// WantPull says which pull request the user is looking at, so that its
// discussion is read and written about before any other's. It reports
// whether there is anything still to do for it.
func (h *Hub) WantPull(source, repo string, number int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range h.pulls() {
		if p.Source == source && strings.EqualFold(p.Repo, repo) && p.Number == number {
			h.wantedPull = pullRef(p).id()
			return p.Behind || p.ReadAt.IsZero()
		}
	}
	return false
}

// Undiscussed lists up to n pull requests whose discussion has not been
// read since the listing last changed, leaving out those that could not be
// read a little while ago.
func (h *Hub) Undiscussed(n int) []PullRef {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []PullRef
	for _, p := range h.ranked() {
		ref := pullRef(p)
		if d := h.discussed[ref.id()]; len(out) < n && (d == nil || d.Key != ref.Key) && time.Since(h.discussFailed[ref.id()]) > discussRetry {
			out = append(out, ref)
		}
	}
	return out
}

// SetDiscussion records the discussion read for ref, or that it could not
// be read. What the model wrote about an earlier reading is kept until it
// writes again.
func (h *Hub) SetDiscussion(ref PullRef, d integrations.Discussion, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err != nil {
		h.discussFailed[ref.id()] = time.Now()
		return
	}
	delete(h.discussFailed, ref.id())
	doc := &pullDoc{Key: ref.Key, ReadAt: time.Now(), Discussion: d}
	if old := h.discussed[ref.id()]; old != nil {
		doc.Brief, doc.BriefKey, doc.Sorts = old.Brief, old.BriefKey, old.Sorts
	}
	h.discussed[ref.id()] = doc
	h.warn(h.cache.Put("pull", ref.id(), doc))
	h.last = ""
	h.changed()
}

// Unbriefed lists the pull requests whose discussion has been read and not
// yet written about by the model, the one the user is looking at first.
func (h *Hub) Unbriefed() []Unbriefed {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Unbriefed
	for _, p := range h.ranked() {
		ref := pullRef(p)
		if d := h.discussed[ref.id()]; d != nil && d.BriefKey != d.Key {
			ref.Key = d.Key
			out = append(out, Unbriefed{Ref: ref, Pull: p, Discussion: d.Discussion, Points: d.Discussion.Points(p.Mine), Wanted: ref.id() == h.wantedPull})
		}
	}
	return out
}

// SetBrief records what the model wrote about the discussion read under
// ref's key, unless it has been read again since.
func (h *Hub) SetBrief(ref PullRef, b PullBrief, sorts map[string]Sorted) {
	h.mu.Lock()
	defer h.mu.Unlock()
	d := h.discussed[ref.id()]
	if d == nil || d.Key != ref.Key {
		return
	}
	b.At = time.Now()
	d.Brief, d.BriefKey, d.Sorts = &b, ref.Key, sorts
	h.warn(h.cache.Put("pull", ref.id(), d))
	h.last = ""
	h.changed()
}

// Pull returns one listed pull request with its discussion, and false when
// no integration lists it.
func (h *Hub) Pull(source, repo string, number int) (PullDetail, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range h.pulls() {
		if p.Source != source || !strings.EqualFold(p.Repo, repo) || p.Number != number {
			continue
		}
		out := PullDetail{PullRequest: p}
		if d := h.discussed[pullRef(p).id()]; d != nil {
			out.Discussion = &d.Discussion
		}
		return out, true
	}
	return PullDetail{}, false
}

// forget drops what is kept of the discussions of source's pull requests
// that it no longer lists.
func (h *Hub) forget(source string, listed []integrations.PullRequest) {
	keep := map[string]bool{}
	for _, p := range listed {
		keep[PullRef{Source: source, Repo: p.Repo, Number: p.Number}.id()] = true
	}
	for id := range h.discussed {
		if strings.HasPrefix(id, source+":") && !keep[id] {
			delete(h.discussed, id)
			delete(h.discussFailed, id)
			h.warn(h.cache.Delete("pull", id))
		}
	}
}
