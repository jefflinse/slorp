package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// github stands in for gh talking to GitHub: it answers the feed's listing
// and detail queries from the pull requests it holds, ten details to a page.
type github struct {
	mine, review []node
	// fail names the requests, counted from 1, that GitHub answers with an error.
	fail map[int]bool
	// endless makes every page of details claim another follows.
	endless bool
	calls   [][]string
}

var detailFields = []string{"mergeable", "reviewRequests", "latestOpinionatedReviews", "commits"}

// only copies the named fields of each node, or with drop every other field.
func only(in []node, fields []string, drop bool) []node {
	out := []node{}
	for _, p := range in {
		c := node{}
		for k, v := range p {
			if slices.Contains(fields, k) != drop {
				c[k] = v
			}
		}
		out = append(out, c)
	}
	return out
}

func (g *github) run(_ context.Context, dir string, argv ...string) ([]byte, error) {
	g.calls = append(g.calls, argv)
	if g.fail[len(g.calls)] {
		return nil, errors.New("HTTP 502: Bad Gateway")
	}
	vars := map[string]string{}
	for i := 4; i < len(argv); i += 2 {
		k, v, _ := strings.Cut(argv[i], "=")
		vars[k] = v
	}
	switch {
	case dir != "" || len(argv) < 5 || !reflect.DeepEqual(argv[:4], []string{"gh", "api", "graphql", "-f"}):
	case vars["query"] == listQuery:
		return json.Marshal(node{"data": node{"mine": node{"nodes": only(g.mine, detailFields, true)}, "review": node{"nodes": only(g.review, detailFields, true)}}})
	case vars["query"] == detailQuery && (vars["q"] == mineSearch || vars["q"] == reviewSearch):
		all := g.mine
		if vars["q"] == reviewSearch {
			all = g.review
		}
		from, _ := strconv.Atoi(vars["after"])
		to := min(from+10, len(all))
		return json.Marshal(node{"data": node{"search": node{
			"pageInfo": node{"hasNextPage": to < len(all) || g.endless, "endCursor": strconv.Itoa(to)},
			"nodes":    only(all[from:to], append([]string{"number", "repository"}, detailFields...), false)}}})
	}
	return nil, fmt.Errorf("not a request the feed makes: %q in %q", argv, dir)
}

type node map[string]any

// own is one of the user's pull requests as gh reports it, ready to merge
// unless edit says otherwise.
func own(edit func(node)) node {
	p := node{"number": 7, "title": "Add a thing", "url": "https://github.com/Acme/Widgets/pull/7", "isDraft": false,
		"updatedAt": "2026-11-01T10:00:00Z", "reviewDecision": nil, "mergeable": "MERGEABLE", "headRefOid": "abc123",
		"repository": node{"nameWithOwner": "Acme/Widgets", "viewerPermission": "WRITE"},
		"commits":    node{"nodes": []node{{"commit": node{"statusCheckRollup": node{"state": "SUCCESS"}}}}}}
	if edit != nil {
		edit(p)
	}
	return p
}

// asked is a pull request waiting on the user's review.
func asked(edit func(node)) node {
	p := node{"number": 9, "title": "Fix the flange", "url": "https://github.com/Acme/Widgets/pull/9", "isDraft": false,
		"updatedAt": "2026-11-02T10:00:00Z", "author": node{"login": "sam", "__typename": "User"},
		"repository": node{"nameWithOwner": "Acme/Widgets"}}
	if edit != nil {
		edit(p)
	}
	return p
}

func checks(state any) func(node) {
	return func(p node) {
		var rollup any
		if state != nil {
			rollup = node{"state": state}
		}
		p["commits"] = node{"nodes": []node{{"commit": node{"statusCheckRollup": rollup}}}}
	}
}

func set(field string, v any) func(node) { return func(p node) { p[field] = v } }

func both(edits ...func(node)) func(node) {
	return func(p node) {
		for _, e := range edits {
			e(p)
		}
	}
}

func permission(level string) func(node) {
	return func(p node) { p["repository"] = node{"nameWithOwner": "Acme/Widgets", "viewerPermission": level} }
}

// surveyed runs the GitHub feed against a gh that answers with those nodes.
func surveyed(t *testing.T, mine, review []node) Found {
	t.Helper()
	got, err := GitHub((&github{mine: mine, review: review}).run).Feed(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// collect returns the items the GitHub feed makes of those nodes.
func collect(t *testing.T, mine, review []node) []FeedItem {
	t.Helper()
	return surveyed(t, mine, review).Items
}

func reviews(states ...string) func(node) {
	return func(p node) {
		nodes := []node{}
		for _, s := range states {
			nodes = append(nodes, node{"state": s})
		}
		p["latestOpinionatedReviews"] = node{"nodes": nodes}
	}
}

func TestFeedPicksOutYourPullRequestsThatNeedYou(t *testing.T) {
	tests := []struct {
		name    string
		edit    func(node)
		kind    string
		urgency string
	}{
		{"passing, mergeable, no review needed", nil, FeedReady, "whenever"},
		{"approved", set("reviewDecision", "APPROVED"), FeedReady, "whenever"},
		{"no checks at all", checks(nil), FeedReady, "whenever"},
		{"you administer the repository", permission("ADMIN"), FeedReady, "whenever"},
		{"you maintain the repository", permission("MAINTAIN"), FeedReady, "whenever"},
		{"a draft", set("isDraft", true), "", ""},
		{"a failing draft", both(set("isDraft", true), checks("FAILURE")), "", ""},
		{"still waiting on a review", set("reviewDecision", "REVIEW_REQUIRED"), "", ""},
		{"checks still running", checks("PENDING"), "", ""},
		{"conflicts with its base", set("mergeable", "CONFLICTING"), FeedConflicts, "awaiting"},
		{"conflicts while a review is outstanding", both(set("mergeable", "CONFLICTING"), set("reviewDecision", "REVIEW_REQUIRED")), FeedConflicts, "awaiting"},
		{"conflicts where you cannot merge", both(set("mergeable", "CONFLICTING"), permission("READ")), FeedConflicts, "awaiting"},
		{"conflicts and changes requested", both(set("mergeable", "CONFLICTING"), set("reviewDecision", "CHANGES_REQUESTED")), FeedChanges, "awaiting"},
		{"a conflicting draft", both(set("mergeable", "CONFLICTING"), set("isDraft", true)), "", ""},
		{"mergeability not worked out yet", set("mergeable", "UNKNOWN"), "", ""},
		{"a repository you cannot merge into", permission("READ"), "", ""},
		{"checks failed", checks("FAILURE"), FeedChecks, "awaiting"},
		{"checks errored", checks("ERROR"), FeedChecks, "awaiting"},
		{"changes requested", set("reviewDecision", "CHANGES_REQUESTED"), FeedChanges, "awaiting"},
		{"changes requested and checks failed", both(set("reviewDecision", "CHANGES_REQUESTED"), checks("FAILURE")), FeedChecks, "awaiting"},
		{"changes requested on a repository you cannot merge into", both(set("reviewDecision", "CHANGES_REQUESTED"), permission("READ")), FeedChanges, "awaiting"},
		{"not a pull request", func(p node) { clear(p) }, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := collect(t, []node{own(tt.edit)}, nil)
			if tt.kind == "" {
				if len(got) != 0 {
					t.Fatalf("got %+v, want nothing", got)
				}
				return
			}
			if len(got) != 1 || got[0].Kind != tt.kind || got[0].Urgency != tt.urgency {
				t.Fatalf("got %+v, want one %s item, %s", got, tt.kind, tt.urgency)
			}
		})
	}
}

func TestFeedItemSaysWhatItIsAbout(t *testing.T) {
	got := collect(t, []node{own(nil), own(both(set("number", 8), checks("FAILURE"), set("reviewDecision", "CHANGES_REQUESTED"), set("mergeable", "CONFLICTING")))}, nil)
	ready, failing := got[0], got[1]
	want := FeedItem{
		Key: "Acme/Widgets#7:ready-to-merge", Kind: FeedReady, Urgency: "whenever",
		Title: "Ready to merge: Add a thing", Ask: ready.Ask,
		Summary: "Acme/Widgets#7, opened by you. Review: none required. Checks: passing. No conflicts with its base branch.",
		URL:     "https://github.com/Acme/Widgets/pull/7", Repo: "Acme/Widgets", At: time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC),
	}
	if !reflect.DeepEqual(ready, want) {
		t.Errorf("ready item =\n%+v, want\n%+v", ready, want)
	}
	if !strings.Contains(ready.Ask, "Merge it") {
		t.Errorf("ready ask = %q", ready.Ask)
	}
	if failing.Title != "Checks failing: Add a thing" || !strings.Contains(failing.Ask, "failing") ||
		failing.Summary != "Acme/Widgets#8, opened by you. Review: changes requested. Checks: failing. It conflicts with its base branch." {
		t.Errorf("failing item = %+v", failing)
	}

	changes := collect(t, []node{own(both(set("reviewDecision", "CHANGES_REQUESTED"), checks("PENDING"), set("mergeable", "UNKNOWN")))}, nil)[0]
	if changes.Title != "Changes requested: Add a thing" || !strings.Contains(changes.Ask, "asked for changes") ||
		changes.Summary != "Acme/Widgets#7, opened by you. Review: changes requested. Checks: still running." {
		t.Errorf("changes item = %+v", changes)
	}
}

func TestFeedItemForConflictsSaysWhatToDo(t *testing.T) {
	got := collect(t, []node{own(both(set("mergeable", "CONFLICTING"), set("reviewDecision", "REVIEW_REQUIRED")))}, nil)[0]
	if got.Title != "Conflicts: Add a thing" || !strings.Contains(got.Ask, "Resolve the conflicts") || got.Key != "Acme/Widgets#7:abc123:conflicts" ||
		got.Summary != "Acme/Widgets#7, opened by you. Review: review required. Checks: passing. It conflicts with its base branch." {
		t.Errorf("conflicts item = %+v", got)
	}
}

func TestFeedKeyChangesWhenTheNeedIsNew(t *testing.T) {
	key := func(edit func(node)) string { return collect(t, []node{own(edit)}, nil)[0].Key }

	if key(nil) != key(set("updatedAt", "2026-11-05T00:00:00Z")) {
		t.Error("the same need got a different key on a later collection, so a dismissed item would come back")
	}
	if key(nil) == key(set("number", 8)) {
		t.Error("two pull requests share a key")
	}
	if key(nil) == key(set("reviewDecision", "CHANGES_REQUESTED")) {
		t.Error("a pull request that now needs something else kept its key, so a dismissal would hide the new need")
	}
	failing := checks("FAILURE")
	if key(failing) != key(failing) {
		t.Error("failing checks on the same commit got two keys")
	}
	if key(failing) == key(both(failing, set("headRefOid", "def456"))) {
		t.Error("checks failing on a new commit kept the old key, so a dismissal would hide the new failure")
	}
	conflicting := set("mergeable", "CONFLICTING")
	if key(conflicting) == key(both(conflicting, set("headRefOid", "def456"))) {
		t.Error("conflicts on a new commit kept the old key, so a dismissal would hide conflicts that came back")
	}
	if key(nil) != key(set("headRefOid", "def456")) {
		t.Error("a ready pull request got a new key from a new commit")
	}
}

func TestFeedListsReviewRequests(t *testing.T) {
	got := collect(t, nil, []node{
		asked(nil),
		asked(both(set("number", 10), set("author", node{"login": "dependabot", "__typename": "Bot"}))),
		asked(both(set("number", 11), set("isDraft", true))),
		asked(both(set("number", 12), set("url", "javascript:alert(1)"), set("author", nil))),
		asked(func(p node) { clear(p) }),
	})
	if len(got) != 3 {
		t.Fatalf("got %d items, want the person's, the bot's and the one with a bad link: %+v", len(got), got)
	}
	person, bot, odd := got[0], got[1], got[2]
	if person.Kind != FeedReview || person.Urgency != "blocking" || person.Title != "Review requested: Fix the flange" ||
		person.Ask != "Your review is requested on sam's pull request." || person.Summary != "Acme/Widgets#9, opened by sam." ||
		person.Key != "Acme/Widgets#9:review-requested" || person.Repo != "Acme/Widgets" || person.URL != "https://github.com/Acme/Widgets/pull/9" ||
		!person.At.Equal(time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("person's request = %+v", person)
	}
	if bot.Kind != FeedReview || bot.Urgency != "whenever" || !strings.Contains(bot.Ask, "dependabot") {
		t.Errorf("bot's request = %+v; want it listed, but nobody is held up by it", bot)
	}
	if odd.URL != "" || !strings.Contains(odd.Ask, "someone") {
		t.Errorf("odd request = %+v; want no link that is not https, and a stand-in for the missing author", odd)
	}
}

func TestFeedListsYourPullRequestsAndWhoseMoveItIs(t *testing.T) {
	tests := []struct {
		name    string
		edit    func(node)
		urgency string
		needs   string
	}{
		{"ready to merge", nil, "whenever", "Ready to merge."},
		{"checks failed", checks("FAILURE"), "awaiting", "Checks are failing."},
		{"changes requested", set("reviewDecision", "CHANGES_REQUESTED"), "awaiting", "A reviewer asked for changes."},
		{"conflicts with its base", set("mergeable", "CONFLICTING"), "awaiting", "It conflicts with its base branch."},
		{"conflicts, and changes requested", both(set("mergeable", "CONFLICTING"), set("reviewDecision", "CHANGES_REQUESTED")), "awaiting", "A reviewer asked for changes."},
		{"a draft", set("isDraft", true), "", "Still a draft."},
		{"a failing draft", both(set("isDraft", true), checks("FAILURE")), "", "Still a draft."},
		{"checks still running", checks("PENDING"), "", "Checks are still running."},
		{"checks running, review outstanding", both(checks("PENDING"), set("reviewDecision", "REVIEW_REQUIRED")), "", "Checks are still running."},
		{"still waiting on a review", set("reviewDecision", "REVIEW_REQUIRED"), "", "Waiting on review."},
		{"approved where you cannot merge", both(set("reviewDecision", "APPROVED"), permission("READ")), "", "Waiting on someone else."},
		{"mergeability not worked out yet", set("mergeable", "UNKNOWN"), "", "Waiting on someone else."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := surveyed(t, []node{own(tt.edit)}, nil).PullRequests
			if len(got) != 1 || got[0].OnYou != (tt.urgency != "") || got[0].Urgency != tt.urgency || got[0].Needs != tt.needs {
				t.Fatalf("got %+v, want one pull request, urgency %q, %q", got, tt.urgency, tt.needs)
			}
		})
	}
	if got := surveyed(t, []node{own(func(p node) { clear(p) })}, nil).PullRequests; len(got) != 0 {
		t.Errorf("something that is not a pull request was listed: %+v", got)
	}
}

func TestFeedSaysHowAPullRequestStands(t *testing.T) {
	got := surveyed(t,
		[]node{own(both(reviews("APPROVED", "CHANGES_REQUESTED", "APPROVED", "DISMISSED"), set("reviewRequests", node{"totalCount": 3}),
			set("author", node{"login": "you", "__typename": "User"}), set("mergeable", "CONFLICTING"), checks("PENDING"),
			set("headRefName", "feat/thing"), set("baseRefName", "main")))},
		[]node{asked(nil), asked(both(set("number", 11), set("isDraft", true), set("url", "javascript:alert(1)"), checks("ERROR"))), asked(func(p node) { clear(p) })}).PullRequests
	if len(got) != 3 {
		t.Fatalf("listed %d pull requests, want yours and the two you are asked to review: %+v", len(got), got)
	}
	want := PullRequest{Repo: "Acme/Widgets", Number: 7, Title: "Add a thing", URL: "https://github.com/Acme/Widgets/pull/7", Author: "you", Mine: true,
		Branch: "feat/thing", Base: "main",
		At: time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC), Approvals: 2, ChangesRequested: 1, Awaiting: 3, Checks: "running", Conflicts: true,
		OnYou: true, Urgency: "awaiting", Needs: "It conflicts with its base branch."}
	if got[0] != want {
		t.Errorf("yours =\n%+v, want\n%+v", got[0], want)
	}
	want = PullRequest{Repo: "Acme/Widgets", Number: 9, Title: "Fix the flange", URL: "https://github.com/Acme/Widgets/pull/9", Author: "sam",
		At: time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC), Checks: "none", OnYou: true, Urgency: "blocking", Needs: "Your review is requested."}
	if got[1] != want {
		t.Errorf("one to review =\n%+v, want\n%+v", got[1], want)
	}
	if d := got[2]; !d.Draft || d.OnYou || d.Urgency != "" || d.Needs != "Your review is requested, but it is still a draft." || d.URL != "" || d.Checks != "failing" || d.Mine {
		t.Errorf("draft to review = %+v; want it listed but not on you, with no link that is not https", d)
	}
	bot := surveyed(t, nil, []node{asked(set("author", node{"login": "dependabot", "__typename": "Bot"}))}).PullRequests[0]
	if !bot.OnYou || bot.Urgency != "whenever" {
		t.Errorf("a bot's = %+v; want it on you, but holding nobody up", bot)
	}
	for state, word := range map[string]string{"SUCCESS": "passing", "FAILURE": "failing"} {
		if got := surveyed(t, []node{own(checks(state))}, nil).PullRequests[0].Checks; got != word {
			t.Errorf("checks %s read as %q, want %q", state, got, word)
		}
	}
}

// requested is n pull requests waiting on the user's review, numbered from
// 100, each conflicting with its base.
func requested(n int) []node {
	out := []node{}
	for i := range n {
		out = append(out, asked(both(set("number", 100+i), set("mergeable", "CONFLICTING"))))
	}
	return out
}

func TestFeedMakesOnlyFixedReadOnlyRequests(t *testing.T) {
	r := &recorder{out: `{"data":{"mine":{"nodes":[]},"review":{"nodes":[]}}}`}
	got, err := GitHub(r.run).Feed(ctx)
	if err != nil || got.Items == nil || len(got.Items) != 0 || got.PullRequests == nil || len(got.PullRequests) != 0 {
		t.Fatalf("Feed = %+v, %v; want two empty lists", got, err)
	}
	list := []string{"gh", "api", "graphql", "-f", "query=" + listQuery}
	if !reflect.DeepEqual(r.argv, list) || r.dir != "" {
		t.Errorf("with nothing listed, last ran %q in %q; want only the listing", r.argv, r.dir)
	}
	for _, field := range []string{"headRefName", "baseRefName"} {
		if !strings.Contains(listQuery, " "+field) {
			t.Errorf("the listing does not ask for %s, so no session could be matched to its pull request", field)
		}
	}

	g := &github{mine: []node{own(nil)}, review: requested(23)}
	found, err := GitHub(g.run).Feed(ctx)
	if err != nil {
		t.Fatal(err)
	}
	detail := func(vars ...string) []string {
		argv := []string{"gh", "api", "graphql", "-f", "query=" + detailQuery}
		for _, v := range vars {
			argv = append(argv, "-f", v)
		}
		return argv
	}
	want := [][]string{list, detail("q=" + mineSearch), detail("q=" + reviewSearch), detail("q="+reviewSearch, "after=10"), detail("q="+reviewSearch, "after=20")}
	if !reflect.DeepEqual(g.calls, want) {
		t.Errorf("ran\n%q, want the listing and then the details a page at a time", g.calls)
	}
	for _, p := range found.PullRequests[1:] {
		if !p.Conflicts {
			t.Errorf("pull request %d was not detailed: %+v", p.Number, p)
		}
	}

	for _, q := range []string{listQuery, detailQuery} {
		if strings.Contains(q, "mutation") || !strings.HasPrefix(q, "query") {
			t.Errorf("not a plain query: %s", q)
		}
	}
	for _, scope := range []string{"is:open author:@me", "is:open review-requested:@me"} {
		if !strings.Contains(listQuery, scope) {
			t.Errorf("the listing does not ask for %q", scope)
		}
	}
	for _, costly := range []string{"reviewRequests", "latestOpinionatedReviews", "mergeable", "statusCheckRollup"} {
		if strings.Contains(listQuery, costly) || !strings.Contains(detailQuery, costly) {
			t.Errorf("%s belongs in the paged details, and not in the listing", costly)
		}
	}
	if !strings.Contains(detailQuery, "first: 10,") {
		t.Error("a page of details is not ten pull requests")
	}
}

func TestFeedStopsAskingForDetailsAfterAFewPages(t *testing.T) {
	g := &github{review: requested(3), endless: true}
	if _, err := GitHub(g.run).Feed(ctx); err != nil {
		t.Fatal(err)
	}
	if len(g.calls) != 1+detailPages {
		t.Errorf("made %d requests, want the listing and %d pages of details", len(g.calls), detailPages)
	}
}

func TestFeedTriesAFailedRequestOnceMore(t *testing.T) {
	for _, failed := range []int{1, 2} {
		g := &github{mine: []node{own(nil)}, fail: map[int]bool{failed: true}}
		got, err := GitHub(g.run).Feed(ctx)
		if err != nil || len(got.Items) != 1 || got.Items[0].Kind != FeedReady {
			t.Errorf("with request %d failing once: %+v, %v; want the ready pull request found all the same", failed, got.Items, err)
		}
		if len(g.calls) != 3 {
			t.Errorf("with request %d failing once, made %d requests, want 3", failed, len(g.calls))
		}
	}
	g := &github{mine: []node{own(nil)}, fail: map[int]bool{1: true, 2: true}}
	if _, err := GitHub(g.run).Feed(ctx); err == nil || !strings.Contains(err.Error(), "502") || len(g.calls) != 2 {
		t.Errorf("with the listing failing twice: err = %v after %d requests, want GitHub's error after 2", err, len(g.calls))
	}
}

func TestFeedGetsByWithoutDetailsGitHubWillNotGive(t *testing.T) {
	failing := func(n int) func(node) { return both(set("number", n), checks("FAILURE")) }
	review := []node{}
	for i := range 12 {
		review = append(review, asked(failing(100+i)))
	}
	// Requests: the listing, your details twice, their first page, their second page twice.
	g := &github{mine: []node{own(nil), own(both(set("number", 8), set("reviewDecision", "CHANGES_REQUESTED"))), own(both(set("number", 9), set("isDraft", true)))},
		review: review, fail: map[int]bool{2: true, 3: true, 5: true, 6: true}}
	got, err := GitHub(g.run).Feed(ctx)
	if err != nil || len(g.calls) != 6 {
		t.Fatalf("err = %v after %d requests; want what was listed, after 6", err, len(g.calls))
	}

	if len(got.Items) != 13 || got.Items[0].Kind != FeedChanges {
		t.Fatalf("items = %+v; want the changes requested and the 12 reviews, and nothing concluded from checks nobody reported", got.Items)
	}
	if got.Items[0].Summary != "Acme/Widgets#8, opened by you. Review: changes requested." {
		t.Errorf("summary = %q, want nothing said of checks or conflicts", got.Items[0].Summary)
	}
	for _, it := range got.Items[1:] {
		if it.Kind != FeedReview {
			t.Errorf("item = %+v, want a review request", it)
		}
	}

	prs := got.PullRequests
	if len(prs) != 15 {
		t.Fatalf("listed %d pull requests, want all 15", len(prs))
	}
	if p := prs[0]; p.OnYou || p.Checks != "unknown" || p.Needs != "GitHub did not say how this one stands." {
		t.Errorf("undetailed pull request of yours = %+v", p)
	}
	if p := prs[1]; !p.OnYou || p.Checks != "unknown" || p.Needs != "A reviewer asked for changes." {
		t.Errorf("undetailed pull request with changes requested = %+v", p)
	}
	if p := prs[2]; p.OnYou || p.Needs != "Still a draft." {
		t.Errorf("undetailed draft = %+v", p)
	}
	if p := prs[3]; !p.OnYou || p.Checks != "failing" {
		t.Errorf("first to review = %+v, want its details", p)
	}
	if p := prs[14]; !p.OnYou || p.Checks != "unknown" || p.Needs != "Your review is requested." {
		t.Errorf("last to review = %+v; want it on you all the same, its checks unknown", p)
	}
}

func TestFeedReportsFailure(t *testing.T) {
	if _, err := GitHub((&recorder{err: errors.New("HTTP 401: Bad credentials")}).run).Feed(ctx); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("err = %v, want gh's own", err)
	}
	if _, err := GitHub((&recorder{out: "<html>"}).run).Feed(ctx); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Errorf("err = %v, want unreadable", err)
	}
}

func TestGitHubDescribesItsFeed(t *testing.T) {
	in := GitHub((&recorder{}).run)
	if len(in.Watches) != 5 || in.Clones == nil {
		t.Errorf("Watches = %q, Clones set = %v", in.Watches, in.Clones != nil)
	}
}

// checkout makes dir a git checkout with that config.
func checkout(t *testing.T, dir, config string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
}

func remote(name, url string) string {
	return "[remote \"" + name + "\"]\n\turl = " + url + "\n\tfetch = +refs/heads/*:refs/remotes/" + name + "/*\n"
}

func TestClones(t *testing.T) {
	t.Run("a checkout names every remote, in lower case", func(t *testing.T) {
		dir := t.TempDir()
		checkout(t, dir, "[core]\n\tbare = false\n"+remote("origin", "git@github.com:Me/Widgets.git")+remote("upstream", "https://github.com/Acme/Widgets")+
			"[branch \"main\"]\n\tremote = origin\n")
		if got := clones(dir); !reflect.DeepEqual(got, []string{"me/widgets", "acme/widgets"}) {
			t.Errorf("clones = %q", got)
		}
	})
	t.Run("url forms", func(t *testing.T) {
		for url, want := range map[string]string{
			"git@github.com:acme/widgets.git":           "acme/widgets",
			"https://github.com/acme/widgets.git":       "acme/widgets",
			"https://github.com/acme/widgets/":          "acme/widgets",
			"ssh://git@ghe.example.com/acme/widgets":    "acme/widgets",
			"https://github.com/acme/widgets.github.io": "acme/widgets.github.io",
		} {
			dir := t.TempDir()
			checkout(t, dir, remote("origin", url))
			if got := clones(dir); len(got) != 1 || got[0] != want {
				t.Errorf("%s: clones = %q, want %q", url, got, want)
			}
		}
		dir := t.TempDir()
		checkout(t, dir, "[remote \"origin\"]\nurl=https://github.com/acme/tight\n")
		if got := clones(dir); len(got) != 1 || got[0] != "acme/tight" {
			t.Errorf("unspaced url: clones = %q", got)
		}
	})
	t.Run("only remotes that point somewhere else count", func(t *testing.T) {
		dir := t.TempDir()
		checkout(t, dir, remote("backup", "/Volumes/backup/acme/widgets")+"[submodule \"vendor/lib\"]\n\turl = https://github.com/other/lib\n"+
			"[remote \"origin\"]\n\tpushurl = git@github.com:acme/push-only.git\n")
		if got := clones(dir); len(got) != 0 {
			t.Errorf("clones = %q, want none: a local path, a submodule and a push address are not where it was cloned from", got)
		}
	})
	t.Run("a directory inside a checkout", func(t *testing.T) {
		root := t.TempDir()
		checkout(t, root, remote("origin", "git@github.com:acme/widgets.git"))
		checkout(t, filepath.Join(root, "docs", "examples", "nested"), remote("origin", "git@github.com:acme/nested.git"))
		deep := filepath.Join(root, "docs", "examples")
		if got := clones(deep); !reflect.DeepEqual(got, []string{"acme/widgets"}) {
			t.Errorf("clones = %q, want the checkout around it", got)
		}
	})
	t.Run("a directory of checkouts", func(t *testing.T) {
		dir := t.TempDir()
		checkout(t, filepath.Join(dir, "a"), remote("origin", "git@github.com:acme/a.git"))
		checkout(t, filepath.Join(dir, "b"), remote("origin", "git@github.com:acme/b.git")+remote("upstream", "git@github.com:acme/a.git"))
		checkout(t, filepath.Join(dir, "group", "deep"), remote("origin", "git@github.com:acme/deep.git"))
		os.MkdirAll(filepath.Join(dir, "plain"), 0o700)
		os.WriteFile(filepath.Join(dir, "notes.md"), nil, 0o600)
		if got := clones(dir); !reflect.DeepEqual(got, []string{"acme/a", "acme/b"}) {
			t.Errorf("clones = %q, want each checkout directly inside, once", got)
		}
	})
	t.Run("a linked worktree is read through its checkout", func(t *testing.T) {
		base := t.TempDir()
		main, tree := filepath.Join(base, "main"), filepath.Join(base, "tree")
		checkout(t, main, remote("origin", "git@github.com:acme/widgets.git"))
		linked := filepath.Join(main, ".git", "worktrees", "tree")
		os.MkdirAll(linked, 0o700)
		os.WriteFile(filepath.Join(linked, "commondir"), []byte("../..\n"), 0o600)
		os.MkdirAll(tree, 0o700)
		os.WriteFile(filepath.Join(tree, ".git"), []byte("gitdir: "+linked+"\n"), 0o600)
		if got := clones(tree); !reflect.DeepEqual(got, []string{"acme/widgets"}) {
			t.Errorf("clones = %q", got)
		}
	})
	t.Run("a submodule checkout is read from where its git directory points", func(t *testing.T) {
		base := t.TempDir()
		os.MkdirAll(filepath.Join(base, "modules", "lib"), 0o700)
		os.WriteFile(filepath.Join(base, "modules", "lib", "config"), []byte(remote("origin", "https://github.com/other/lib.git")), 0o600)
		os.MkdirAll(filepath.Join(base, "lib"), 0o700)
		os.WriteFile(filepath.Join(base, "lib", ".git"), []byte("gitdir: ../modules/lib\n"), 0o600)
		if got := clones(filepath.Join(base, "lib")); !reflect.DeepEqual(got, []string{"other/lib"}) {
			t.Errorf("clones = %q", got)
		}
	})
	t.Run("nothing to find", func(t *testing.T) {
		empty := t.TempDir()
		if got := clones(""); got != nil {
			t.Errorf("clones of no directory = %q", got)
		}
		if got := clones(empty); len(got) != 0 {
			t.Errorf("clones of an empty directory = %q", got)
		}
		if got := clones(filepath.Join(empty, "missing")); len(got) != 0 {
			t.Errorf("clones of a missing directory = %q", got)
		}
	})
	t.Run("a very large directory is only partly searched", func(t *testing.T) {
		dir := t.TempDir()
		for i := range maxScanned + 5 {
			os.MkdirAll(filepath.Join(dir, fmt.Sprintf("d%03d", i)), 0o700)
		}
		checkout(t, filepath.Join(dir, "d000"), remote("origin", "git@github.com:acme/first.git"))
		checkout(t, filepath.Join(dir, "zzz"), remote("origin", "git@github.com:acme/last.git"))
		if got := clones(dir); !reflect.DeepEqual(got, []string{"acme/first"}) {
			t.Errorf("clones = %q, want only what the first %d entries hold", got, maxScanned)
		}
	})
}

func feeder(id string, available bool, found []FeedItem) Integration {
	in := fake(id, available, true, new(int))
	in.Feed = func(ctx context.Context) (Found, error) { return Found{Items: found}, nil }
	in.Watches = []string{"Watches " + id}
	in.Clones = func(dir string) []string {
		if filepath.Base(dir) == "empty" {
			return nil
		}
		return []string{id + "/" + filepath.Base(dir)}
	}
	return in
}

func TestManagerFeeds(t *testing.T) {
	on := map[string]bool{"ready": true, "missing": true, "quiet": true}
	m := NewManager(func() map[string]bool { return on },
		feeder("ready", true, []FeedItem{{Key: "k"}}), feeder("off", true, nil), feeder("missing", false, nil), fake("quiet", true, true, new(int)))

	got := m.Feeds(ctx)
	if len(got) != 3 {
		t.Fatalf("Feeds = %d entries, want every integration that has a feed", len(got))
	}
	for i, want := range []struct {
		id string
		on bool
	}{{"ready", true}, {"off", false}, {"missing", false}} {
		if got[i].ID != want.id || got[i].On != want.on {
			t.Errorf("feed %d = %s on=%v, want %s on=%v", i, got[i].ID, got[i].On, want.id, want.on)
		}
	}
	if found, err := got[0].Fetch(ctx); err != nil || len(found.Items) != 1 || found.Items[0].Key != "k" {
		t.Errorf("Fetch = %+v, %v", found, err)
	}

	if s := m.Status(ctx); len(s[0].Watches) != 1 || s[0].Watches[0] != "Watches ready" || s[3].Watches == nil || len(s[3].Watches) != 0 {
		t.Errorf("Status watches = %q and %q; want the feed described, and an empty list where there is none", s[0].Watches, s[3].Watches)
	}
}

func TestManagerClones(t *testing.T) {
	on := map[string]bool{"a": true, "b": true, "plain": true}
	silent := feeder("b", true, nil)
	silent.Clones = func(string) []string { return nil }
	m := NewManager(func() map[string]bool { return on }, feeder("a", true, nil), feeder("off", true, nil), silent, fake("plain", true, true, new(int)))

	got := m.Clones([]string{"/w/one", "/w/empty", "/w/two"})
	want := map[string][]string{"/w/one": {"a/one"}, "/w/two": {"a/two"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Clones = %v, want %v: only integrations that are on, and only directories that hold something", got, want)
	}
	on["off"] = true
	if got := m.Clones([]string{"/w/one"}); !reflect.DeepEqual(got["/w/one"], []string{"a/one", "off/one"}) {
		t.Errorf("Clones = %v, want both integrations' findings for the directory", got)
	}
}
