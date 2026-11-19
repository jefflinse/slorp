package hub

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/integrations"
)

// talked is a discussion of the user's own pull request in which sam spoke
// last in a thread at at, and nothing else was said.
func talked(at time.Time, said string) integrations.Discussion {
	return integrations.Discussion{Viewer: "me", Threads: []integrations.Thread{{Path: "a.go", Line: 3, Comments: []integrations.Comment{
		{Author: "me", Body: "what I wrote", At: at.Add(-time.Hour), URL: "https://example.com/1"},
		{Author: "sam", Body: said, At: at, URL: "https://example.com/" + said}}}}}
}

func listed(t *testing.T, h *Hub, number int) PullRequest {
	t.Helper()
	for _, p := range h.State().PullRequests {
		if p.Number == number {
			return p
		}
	}
	t.Fatalf("pull request %d is not listed", number)
	return PullRequest{}
}

func refs(list []PullRef) []int {
	out := []int{}
	for _, r := range list {
		out = append(out, r.Number)
	}
	return out
}

func TestDiscussionsAreReadForWhatChanged(t *testing.T) {
	h, _, _ := newHub(t)
	day := func(d int) time.Time { return time.Date(2026, 11, d, 0, 0, 0, 0, time.UTC) }
	mine := pull("acme/widgets", 1, false, day(1))
	mine.Mine = true
	feed := []integrations.PullRequest{mine, pull("acme/widgets", 2, true, day(2)), pull("acme/widgets", 3, false, day(3))}
	h.SetFeed("github", nil, feed, nil)

	if p := listed(t, h, 1); p.Points == nil || len(p.Points) != 0 || !p.ReadAt.IsZero() || p.Behind || p.Brief != nil {
		t.Errorf("before its discussion is read: %+v; want an empty list of points and nothing else said", p)
	}
	all := h.Undiscussed(9)
	if got := refs(all); len(got) != 3 || got[0] != 2 || got[1] != 3 || got[2] != 1 {
		t.Fatalf("to read: %v; want all three, where the move is mine first and then the latest changed", got)
	}
	if got := refs(h.Undiscussed(2)); len(got) != 2 || got[1] != 3 {
		t.Errorf("limited to two: %v", got)
	}
	if !h.WantPull("github", "ACME/Widgets", 1) || h.WantPull("github", "acme/widgets", 99) || h.WantPull("gitlab", "acme/widgets", 1) {
		t.Error("WantPull: want a listed pull request with nothing read to have work due, and one not listed to have none")
	}
	if got := h.Undiscussed(2); len(got) != 2 || got[0].Number != 1 || !got[0].Wanted || got[1].Wanted {
		t.Errorf("to read once the user is looking at #1: %+v; want it first, and the only one marked as wanted", got)
	}

	first := all[2]
	h.SetDiscussion(first, talked(day(1), "why"), nil)
	p := listed(t, h, 1)
	if len(p.Points) != 1 || p.Points[0].Kind != integrations.PointThread || p.Points[0].Who != "sam" || p.Points[0].Text != "why" || p.Points[0].Line != 3 || p.Points[0].Before != nil {
		t.Errorf("points = %+v; want the thread where sam spoke last, without what led up to it", p.Points)
	}
	if p.ReadAt.IsZero() || !p.Behind || p.Brief != nil || p.Points[0].Sort != "" {
		t.Errorf("once read: %+v; want it behind until the model has written", p)
	}
	if got := refs(h.Undiscussed(9)); len(got) != 2 {
		t.Errorf("to read: %v; want the one just read left out", got)
	}
	if !h.WantPull("github", "acme/widgets", 1) {
		t.Error("WantPull: want work due while the model has not written")
	}

	h.SetDiscussion(all[0], integrations.Discussion{}, errors.New("HTTP 502"))
	if got := refs(h.Undiscussed(9)); len(got) != 1 || got[0] != 3 {
		t.Errorf("to read: %v; want one that just failed left a while", got)
	}
	h.discussFailed[all[0].id()] = time.Now().Add(-discussRetry - time.Second)
	if got := refs(h.Undiscussed(9)); len(got) != 2 {
		t.Errorf("to read after a while: %v; want the failed one tried again", got)
	}
	h.SetDiscussion(all[0], integrations.Discussion{}, errors.New("HTTP 502"))
	h.SetDiscussion(all[0], integrations.Discussion{}, nil)
	if got := refs(h.Undiscussed(9)); len(got) != 1 {
		t.Errorf("to read: %v; want the one just read left out", got)
	}

	// Two change: the listing moves on, and their discussions are behind.
	feed[0].At, feed[1].At = day(4), day(5)
	h.SetFeed("github", nil, feed, nil)
	if got := h.Undiscussed(9); len(got) != 3 || got[0].Number != 1 || got[0].Key == first.Key || got[1].Number != 2 {
		t.Errorf("to read after #1 and #2 changed: %+v; want them again, #1 under a new key, and #2 not held back for having once failed", got)
	}
	if p := listed(t, h, 1); len(p.Points) != 1 || !p.Behind {
		t.Errorf("after it changed: %+v; want what was read kept, and marked behind", p)
	}
	if waiting := h.Unbriefed(); len(waiting) != 2 || waiting[0].Ref.Number != 1 || waiting[0].Ref.Key != first.Key {
		t.Errorf("to write about: %+v; want #1 under the key its discussion was read under, not the listing's", waiting)
	}
}

func TestBriefs(t *testing.T) {
	h, k, st := newHub(t)
	day := func(d int) time.Time { return time.Date(2026, 11, d, 0, 0, 0, 0, time.UTC) }
	mine := pull("acme/widgets", 1, false, day(1))
	mine.Mine, mine.Needs = true, "Waiting on review."
	reviewed := pull("acme/widgets", 2, false, day(2))
	reviewed.Reviewed, reviewed.Needs = true, "You reviewed it. Waiting on its author."
	feed := []integrations.PullRequest{mine, reviewed, pull("acme/widgets", 3, false, day(3))}
	h.SetFeed("github", nil, feed, nil)
	if len(h.Unbriefed()) != 0 {
		t.Fatal("pull requests with nothing read are waiting to be written about")
	}
	var one, two PullRef
	for _, r := range h.Undiscussed(9) {
		switch r.Number {
		case 1:
			one = r
		case 2:
			two = r
		}
	}
	own := talked(day(1), "why")
	own.Comments = []integrations.Comment{{Author: "kim", Body: "only on my own does this wait", At: day(1).Add(-time.Minute)}}
	h.SetDiscussion(one, own, nil)
	h.SetDiscussion(two, talked(day(2), "no"), nil)

	waiting := h.Unbriefed()
	if len(waiting) != 2 || waiting[0].Ref.Number != 2 || waiting[1].Ref.Number != 1 || waiting[0].Wanted {
		t.Fatalf("to write about: %+v; want the two that were read, the latest changed first", waiting)
	}
	w := waiting[1]
	if w.Ref.Key != one.Key || w.Pull.Number != 1 || !w.Pull.Mine || len(w.Discussion.Threads) != 1 || len(w.Points) != 2 || len(w.Points[0].Before) != 1 {
		t.Errorf("waiting = %+v; want the pull request, its discussion, and its points as its author sees them, with what led up to each", w)
	}
	if got := waiting[0].Points; len(got) != 1 {
		t.Errorf("points of one that is not mine = %+v; want only the thread I am in", got)
	}
	h.WantPull("github", "acme/widgets", 1)
	if waiting := h.Unbriefed(); waiting[0].Ref.Number != 1 || !waiting[0].Wanted || waiting[1].Wanted {
		t.Errorf("to write about once the user is looking at #1: %+v; want it first and marked", waiting)
	}
	if p := listed(t, h, 2); p.Needs != "You reviewed it, and its author has answered." {
		t.Errorf("reviewed, with an answer waiting: needs %q", p.Needs)
	}
	if p := listed(t, h, 1); p.Needs != "Waiting on review." {
		t.Errorf("my own, with points waiting: needs %q; want it left as listed", p.Needs)
	}

	h.SetBrief(w.Ref, PullBrief{Stands: "Sam asks why.", Next: "Yours.", Since: "It moved."}, map[string]Sorted{w.Points[0].ID: {Sort: SortQuestion, Gist: "Sam asks why"}})
	p := listed(t, h, 1)
	if p.Brief == nil || p.Brief.Stands != "Sam asks why." || p.Brief.Next != "Yours." || time.Since(p.Brief.At) > time.Minute || p.Behind {
		t.Errorf("once written about: %+v (%+v); want the brief, timed, and nothing behind", p, p.Brief)
	}
	if p.Points[0].Sort != SortQuestion || p.Points[0].Gist != "Sam asks why" || p.Points[0].Line != 3 {
		t.Errorf("point = %+v; want what the model made of it beside where it is", p.Points[0])
	}
	if h.WantPull("github", "acme/widgets", 1) {
		t.Error("WantPull: work is still due for a pull request read and written about")
	}
	if waiting := h.Unbriefed(); len(waiting) != 1 || waiting[0].Ref.Number != 2 {
		t.Errorf("to write about: %+v; want only the other", waiting)
	}

	settled := map[string]Sorted{h.Unbriefed()[0].Points[0].ID: {Sort: SortSettled}}
	h.SetBrief(h.Unbriefed()[0].Ref, PullBrief{Stands: "Nothing to do."}, settled)
	if p := listed(t, h, 2); p.Needs != "You reviewed it. Waiting on its author." {
		t.Errorf("reviewed, with only a settled point: needs %q; want it left as listed", p.Needs)
	}

	// It is read again before the model answers for the reading before.
	feed[0].At = day(5)
	h.SetFeed("github", nil, feed, nil)
	if p := listed(t, h, 1); !p.Behind || p.Brief == nil {
		t.Errorf("changed since it was read and written about: %+v; want it marked behind", p)
	}
	again := h.Undiscussed(1)[0]
	h.SetDiscussion(again, talked(day(5), "and another thing"), nil)
	p = listed(t, h, 1)
	if p.Brief == nil || p.Brief.Stands != "Sam asks why." || !p.Behind || p.Points[0].Text != "and another thing" || p.Points[0].Sort != "" {
		t.Errorf("read again: %+v; want the old brief kept and marked behind, and the new point unsorted", p)
	}
	h.SetBrief(w.Ref, PullBrief{Stands: "Written of the earlier reading."}, nil)
	if p := listed(t, h, 1); p.Brief.Stands != "Sam asks why." {
		t.Errorf("a brief of an earlier reading replaced what was there: %+v", p.Brief)
	}
	h.SetBrief(PullRef{Source: "github", Repo: "acme/widgets", Number: 99, Key: "k"}, PullBrief{Stands: "Of nothing."}, nil)

	d, ok := h.Pull("github", "Acme/Widgets", 1)
	if !ok || d.Number != 1 || d.Discussion == nil || len(d.Discussion.Threads) != 1 || d.Discussion.Threads[0].Comments[1].Body != "and another thing" || len(d.Points) != 1 {
		t.Errorf("Pull = %+v, %v; want the pull request with the whole of its discussion", d, ok)
	}
	if d, ok := h.Pull("github", "acme/widgets", 3); !ok || d.Discussion != nil {
		t.Errorf("Pull of one not read = %+v, %v; want it without a discussion", d, ok)
	}
	if _, ok := h.Pull("github", "acme/widgets", 99); ok {
		t.Error("Pull found a pull request nothing lists")
	}
	if _, ok := h.Pull("gitlab", "acme/widgets", 1); ok {
		t.Error("Pull found a pull request under another integration's name")
	}

	other := pull("acme/widgets", 1, false, day(1))
	h.SetFeed("elsewhere", nil, []integrations.PullRequest{other}, nil)
	for _, r := range h.Undiscussed(9) {
		if r.Source == "elsewhere" {
			h.SetDiscussion(r, talked(day(1), "from elsewhere"), nil)
		}
	}

	h2 := New(k, st)
	h2.SetFeed("github", nil, feed, nil)
	h2.SetFeed("elsewhere", nil, []integrations.PullRequest{other}, nil)
	if p := listed(t, h2, 1); p.Brief == nil || p.Brief.Stands != "Sam asks why." || p.Brief.Since != "It moved." || len(p.Points) != 1 || p.Points[0].Text != "and another thing" || p.ReadAt.IsZero() {
		t.Errorf("after a restart: %+v; want what was last read, and what was written, kept", p)
	}
	if p := listed(t, h2, 2); p.Brief == nil || p.Points[0].Sort != SortSettled || p.Behind {
		t.Errorf("after a restart: %+v; want the model's sorting kept", p)
	}

	// One that is no longer listed is forgotten; a failed listing forgets nothing.
	h2.SetFeed("github", nil, nil, errors.New("timed out"))
	h2.SetFeed("github", nil, feed[:1], nil)
	h3 := New(k, st)
	h3.SetFeed("github", nil, feed, nil)
	h3.SetFeed("elsewhere", nil, []integrations.PullRequest{other}, nil)
	if p := listed(t, h3, 1); p.Brief == nil {
		t.Errorf("still listed: %+v; want it kept", p)
	}
	if d, _ := h3.Pull("elsewhere", "acme/widgets", 1); d.Discussion == nil {
		t.Error("what another integration lists was forgotten with what GitHub stopped listing")
	}
	if p := listed(t, h3, 2); p.Brief != nil || len(p.Points) != 0 || !p.ReadAt.IsZero() {
		t.Errorf("listed again after going: %+v; want it read afresh", p)
	}
}

func TestQuestionsAndPushbackAreRaised(t *testing.T) {
	h, k, _ := newHub(t)
	a := effort(t, k, "A")
	must(t, k.AddDir(a, "/work/a"))
	h.SetClones(map[string][]string{"/work/a": {"acme/widgets"}})
	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	said := func(who string, mins int, body string) integrations.Comment {
		return integrations.Comment{Author: who, Body: body, At: at.Add(time.Duration(mins) * time.Minute), URL: "https://example.com/" + body}
	}
	mine := pull("acme/widgets", 1, false, at)
	mine.Mine, mine.Title, mine.URL = true, "Add retention", "https://example.com/pull/1"
	theirs := pull("acme/gadgets", 2, false, at)
	theirs.Title, theirs.Author = "Lag metric", "tomas"
	h.SetFeed("github", []Item{{ID: "github-listed", Kind: "changes-requested", Title: "Changes requested: Add retention"}}, []integrations.PullRequest{mine, theirs}, nil)
	var one, two PullRef
	for _, r := range h.Undiscussed(9) {
		if r.Number == 1 {
			one = r
		} else {
			two = r
		}
	}
	h.SetDiscussion(one, integrations.Discussion{Viewer: "me", Comments: []integrations.Comment{said("sam", 1, "why"), said("sam", 2, "no"), said("sam", 3, "please"), said("sam", 4, "did it"), said("sam", 5, "thanks"), said("sam", 6, "unsorted")}}, nil)
	late := said("tomas", 9, "I would rather not")
	late.URL = ""
	h.SetDiscussion(two, integrations.Discussion{Viewer: "me", Threads: []integrations.Thread{{Path: "m.go", Line: 5, Comments: []integrations.Comment{said("me", 1, "drop it"), late}}}}, nil)

	raised := func() []Item {
		var out []Item
		for _, it := range h.State().Items {
			if it.Point != "" {
				out = append(out, it)
			}
		}
		return out
	}
	if got := raised(); len(got) != 0 {
		t.Fatalf("raised %+v before the model sorted anything", got)
	}
	point := func(body string) string { return "https://example.com/" + body }
	h.SetBrief(one, PullBrief{Stands: "s"}, map[string]Sorted{point("why"): {Sort: SortQuestion, Gist: "Sam asks why"}, point("no"): {Sort: SortPushback, Gist: "Sam disagrees"},
		point("please"): {Sort: SortRequest, Gist: "x"}, point("did it"): {Sort: SortDone, Gist: "x"}, point("thanks"): {Sort: SortSettled, Gist: "x"}})
	var id string
	for _, w := range h.Unbriefed() {
		id = w.Points[0].ID
	}
	h.SetBrief(two, PullBrief{Stands: "s"}, map[string]Sorted{id: {Sort: SortPushback}})

	got := raised()
	if len(got) != 3 {
		t.Fatalf("raised %+v; want the question, and the pushback on each pull request", got)
	}
	if all := h.State().Items; len(all) != 4 || all[0].ID != "github-listed" {
		t.Errorf("items = %+v; want what the feed listed kept, and first", all)
	}
	pushback, question, other := got[0], got[1], got[2]
	if question.Kind != KindPullQuestion || question.Title != "Question: Add retention" || question.Ask != "Sam asks why." || question.Subtitle != "Sam asks why" || question.Urgency != "awaiting" ||
		question.URL != point("why") || question.Pull != "acme/widgets#1" || question.Point != point("why") || question.Repo != "acme/widgets" || question.Source != "github" ||
		!question.At.Equal(at.Add(time.Minute)) || question.Effort != a || question.Replies == nil || question.Summary != "acme/widgets#1, opened by you. sam wrote: why" {
		t.Errorf("question = %+v; want it to say what is asked, where, and to hold up my own work", question)
	}
	if pushback.Kind != KindPullPushback || pushback.Title != "Pushback: Add retention" || pushback.Point != point("no") {
		t.Errorf("pushback = %+v", pushback)
	}
	if other.Title != "Pushback: Lag metric" || other.Urgency != "blocking" || other.Ask != "tomas spoke last." || other.URL != "" || other.Effort != "" || other.Pull != "acme/gadgets#2" ||
		other.Summary != "acme/gadgets#2, opened by tomas. tomas wrote: I would rather not" {
		t.Errorf("pushback on another's = %+v; want it to hold up its author, and to make do without a gist or a link", other)
	}
	if question.ID == pushback.ID || !strings.HasPrefix(question.ID, "github-") || len(question.ID) != len("github-")+12 {
		t.Errorf("ids %q and %q; want each its own, under the integration's name", question.ID, pushback.ID)
	}
	again := raised()
	if again[1].ID != question.ID {
		t.Errorf("id was %q and is now %q; want it to stay while the same point waits", question.ID, again[1].ID)
	}
	theirs.Number = 3
	h.SetFeed("github", nil, []integrations.PullRequest{mine}, nil)
	if got := raised(); len(got) != 2 {
		t.Errorf("raised %+v once a pull request is no longer listed", got)
	}
}
