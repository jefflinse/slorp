package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

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
	b, err := json.Marshal(node{"data": node{"mine": node{"nodes": mine}, "review": node{"nodes": review}}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := GitHub((&recorder{out: string(b)}).run).Feed(ctx)
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
		{"conflicts with its base", set("mergeable", "CONFLICTING"), FeedConflicts, "soon"},
		{"conflicts while a review is outstanding", both(set("mergeable", "CONFLICTING"), set("reviewDecision", "REVIEW_REQUIRED")), FeedConflicts, "soon"},
		{"conflicts where you cannot merge", both(set("mergeable", "CONFLICTING"), permission("READ")), FeedConflicts, "soon"},
		{"conflicts and changes requested", both(set("mergeable", "CONFLICTING"), set("reviewDecision", "CHANGES_REQUESTED")), FeedChanges, "soon"},
		{"a conflicting draft", both(set("mergeable", "CONFLICTING"), set("isDraft", true)), "", ""},
		{"mergeability not worked out yet", set("mergeable", "UNKNOWN"), "", ""},
		{"a repository you cannot merge into", permission("READ"), "", ""},
		{"checks failed", checks("FAILURE"), FeedChecks, "soon"},
		{"checks errored", checks("ERROR"), FeedChecks, "soon"},
		{"changes requested", set("reviewDecision", "CHANGES_REQUESTED"), FeedChanges, "soon"},
		{"changes requested and checks failed", both(set("reviewDecision", "CHANGES_REQUESTED"), checks("FAILURE")), FeedChecks, "soon"},
		{"changes requested on a repository you cannot merge into", both(set("reviewDecision", "CHANGES_REQUESTED"), permission("READ")), FeedChanges, "soon"},
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
	got := collect(t, []node{own(nil), own(both(checks("FAILURE"), set("reviewDecision", "CHANGES_REQUESTED"), set("mergeable", "CONFLICTING")))}, nil)
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
		failing.Summary != "Acme/Widgets#7, opened by you. Review: changes requested. Checks: failing. It conflicts with its base branch." {
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
	if person.Kind != FeedReview || person.Urgency != "soon" || person.Title != "Review requested: Fix the flange" ||
		person.Ask != "Your review is requested on sam's pull request." || person.Summary != "Acme/Widgets#9, opened by sam." ||
		person.Key != "Acme/Widgets#9:review-requested" || person.Repo != "Acme/Widgets" || person.URL != "https://github.com/Acme/Widgets/pull/9" ||
		!person.At.Equal(time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("person's request = %+v", person)
	}
	if bot.Kind != FeedReview || bot.Urgency != "whenever" || !strings.Contains(bot.Ask, "dependabot") {
		t.Errorf("bot's request = %+v; want it listed, but not as soon as a person's", bot)
	}
	if odd.URL != "" || !strings.Contains(odd.Ask, "someone") {
		t.Errorf("odd request = %+v; want no link that is not https, and a stand-in for the missing author", odd)
	}
}

func TestFeedListsYourPullRequestsAndWhoseMoveItIs(t *testing.T) {
	tests := []struct {
		name  string
		edit  func(node)
		onYou bool
		needs string
	}{
		{"ready to merge", nil, true, "Ready to merge."},
		{"checks failed", checks("FAILURE"), true, "Checks are failing."},
		{"changes requested", set("reviewDecision", "CHANGES_REQUESTED"), true, "A reviewer asked for changes."},
		{"conflicts with its base", set("mergeable", "CONFLICTING"), true, "It conflicts with its base branch."},
		{"conflicts, and changes requested", both(set("mergeable", "CONFLICTING"), set("reviewDecision", "CHANGES_REQUESTED")), true, "A reviewer asked for changes."},
		{"a draft", set("isDraft", true), false, "Still a draft."},
		{"a failing draft", both(set("isDraft", true), checks("FAILURE")), false, "Still a draft."},
		{"checks still running", checks("PENDING"), false, "Checks are still running."},
		{"checks running, review outstanding", both(checks("PENDING"), set("reviewDecision", "REVIEW_REQUIRED")), false, "Checks are still running."},
		{"still waiting on a review", set("reviewDecision", "REVIEW_REQUIRED"), false, "Waiting on review."},
		{"approved where you cannot merge", both(set("reviewDecision", "APPROVED"), permission("READ")), false, "Waiting on someone else."},
		{"mergeability not worked out yet", set("mergeable", "UNKNOWN"), false, "Waiting on someone else."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := surveyed(t, []node{own(tt.edit)}, nil).PullRequests
			if len(got) != 1 || got[0].OnYou != tt.onYou || got[0].Needs != tt.needs {
				t.Fatalf("got %+v, want one pull request, on you = %v, %q", got, tt.onYou, tt.needs)
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
			set("author", node{"login": "you", "__typename": "User"}), set("mergeable", "CONFLICTING"), checks("PENDING")))},
		[]node{asked(nil), asked(both(set("number", 11), set("isDraft", true), set("url", "javascript:alert(1)"), checks("ERROR"))), asked(func(p node) { clear(p) })}).PullRequests
	if len(got) != 3 {
		t.Fatalf("listed %d pull requests, want yours and the two you are asked to review: %+v", len(got), got)
	}
	want := PullRequest{Repo: "Acme/Widgets", Number: 7, Title: "Add a thing", URL: "https://github.com/Acme/Widgets/pull/7", Author: "you", Mine: true,
		At: time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC), Approvals: 2, ChangesRequested: 1, Awaiting: 3, Checks: "running", Conflicts: true,
		OnYou: true, Needs: "It conflicts with its base branch."}
	if got[0] != want {
		t.Errorf("yours =\n%+v, want\n%+v", got[0], want)
	}
	want = PullRequest{Repo: "Acme/Widgets", Number: 9, Title: "Fix the flange", URL: "https://github.com/Acme/Widgets/pull/9", Author: "sam",
		At: time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC), Checks: "none", OnYou: true, Needs: "Your review is requested."}
	if got[1] != want {
		t.Errorf("one to review =\n%+v, want\n%+v", got[1], want)
	}
	if d := got[2]; !d.Draft || d.OnYou || d.Needs != "Your review is requested, but it is still a draft." || d.URL != "" || d.Checks != "failing" || d.Mine {
		t.Errorf("draft to review = %+v; want it listed but not on you, with no link that is not https", d)
	}
	for state, word := range map[string]string{"SUCCESS": "passing", "FAILURE": "failing"} {
		if got := surveyed(t, []node{own(checks(state))}, nil).PullRequests[0].Checks; got != word {
			t.Errorf("checks %s read as %q, want %q", state, got, word)
		}
	}
}

func TestFeedRunsOneFixedReadOnlyQuery(t *testing.T) {
	r := &recorder{out: `{"data":{"mine":{"nodes":[]},"review":{"nodes":[]}}}`}
	got, err := GitHub(r.run).Feed(ctx)
	if err != nil || got.Items == nil || len(got.Items) != 0 || got.PullRequests == nil || len(got.PullRequests) != 0 {
		t.Fatalf("Feed = %+v, %v; want two empty lists", got, err)
	}
	want := []string{"gh", "api", "graphql", "-f", "query=" + feedQuery}
	if !reflect.DeepEqual(r.argv, want) || r.dir != "" {
		t.Errorf("ran %q in %q", r.argv, r.dir)
	}
	if strings.Contains(feedQuery, "mutation") || !strings.HasPrefix(feedQuery, "query {") {
		t.Error("the feed's request is not a plain query")
	}
	for _, scope := range []string{"is:open author:@me", "is:open review-requested:@me", "reviewRequests", "latestOpinionatedReviews", "mergeable", "statusCheckRollup"} {
		if !strings.Contains(feedQuery, scope) {
			t.Errorf("the query does not ask for %q", scope)
		}
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
