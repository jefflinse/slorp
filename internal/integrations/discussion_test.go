package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

var noon = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

// mins is a time that many minutes after noon.
func mins(n int) time.Time { return noon.Add(time.Duration(n) * time.Minute) }

func by(who string, at int, body string) Comment {
	return Comment{Author: who, Body: body, At: mins(at), URL: "https://example.com/" + who + "/" + mins(at).Format("1504")}
}

func thread(path string, comments ...Comment) Thread {
	return Thread{Path: path, Line: 10, Comments: comments}
}

func kinds(points []Point) []string {
	out := []string{}
	for _, p := range points {
		out = append(out, p.Kind+" "+p.Who)
	}
	return out
}

func TestPointsOfThreads(t *testing.T) {
	resolved := thread("d.go", by("sam", 5, "one"))
	resolved.Resolved = true
	d := Discussion{Viewer: "Me", Threads: []Thread{
		thread("a.go", by("sam", 1, "first"), by("me", 2, "second"), by("sam", 3, "third")),
		thread("b.go", by("sam", 4, "answered"), by("ME", 6, "by me")),
		thread("c.go", by("sam", 7, "between others"), by("kim", 8, "not my thread")),
		resolved, thread("e.go"),
	}}

	theirs := d.Points(false)
	if got := kinds(theirs); !reflect.DeepEqual(got, []string{"thread sam"}) {
		t.Fatalf("on someone else's pull request: %v; want only the thread I am in where someone else spoke last", got)
	}
	p := theirs[0]
	if p.ID != "https://example.com/sam/1203" || p.URL != p.ID || p.Path != "a.go" || p.Line != 10 || p.Text != "third" || !p.At.Equal(mins(3)) {
		t.Errorf("point = %+v; want it named for, and pointing at, the last thing said", p)
	}
	if len(p.Before) != 2 || p.Before[0].Body != "first" || p.Before[1].Body != "second" {
		t.Errorf("before = %+v; want what led up to it, oldest first", p.Before)
	}

	if got := kinds(d.Points(true)); !reflect.DeepEqual(got, []string{"thread kim", "thread sam"}) {
		t.Errorf("on my own pull request: %v; want every open thread where someone else spoke last, the latest first", got)
	}
}

func TestPointsOfComments(t *testing.T) {
	bot := by("ci", 30, "Coverage went up.")
	bot.Bot = true
	d := Discussion{Viewer: "me", Comments: []Comment{
		by("sam", 1, "@me before I last spoke"), by("me", 10, "I spoke"),
		by("sam", 20, "no name in this one"), by("kim", 25, "what do you think, @Me?"), bot,
		by("sam", 35, "mail x@me.example or ask @me-too or @mentor"), by("me2", 36, "(@me) in brackets"),
	}}
	if got := kinds(d.Points(false)); !reflect.DeepEqual(got, []string{"mention me2", "mention kim"}) {
		t.Errorf("on someone else's: %v; want only what names me, said since I last spoke", got)
	}
	if got := kinds(d.Points(true)); !reflect.DeepEqual(got, []string{"mention me2", "comment sam", "mention kim", "comment sam"}) {
		t.Errorf("on my own: %v; want everything a person said since I last spoke", got)
	}

	d.Commits = []Commit{{ID: "abc", Author: "me", At: mins(40)}}
	if got := d.Points(true); len(got) != 0 {
		t.Errorf("after I pushed: %v; want nothing said before then still waiting", kinds(got))
	}
	if got := kinds((Discussion{Comments: d.Comments}).Points(true)); len(got) != 6 || slices.Contains(got, "mention kim") || slices.Contains(got, "comment ci") {
		t.Errorf("with no name for me: %v; want every person's comment and no mention", got)
	}
}

func review(who string, at int, state, body string) Review {
	return Review{Comment: by(who, at, body), State: state}
}

func TestPointsOfReviews(t *testing.T) {
	d := Discussion{Viewer: "me", Reviews: []Review{
		review("sam", 1, "changes requested", "before I acted"), review("me", 5, "commented", "my own"),
		review("sam", 10, "changes requested", "Please split this."), review("kim", 11, "approved", "Lovely."),
		review("lee", 12, "commented", "  "), review("ann", 13, "commented", "@me should look at the lock"),
	}}
	if got := kinds(d.Points(true)); !reflect.DeepEqual(got, []string{"review ann", "review sam"}) {
		t.Errorf("on my own: %v; want the reviews since I acted that say something short of approving", got)
	}
	if got := kinds(d.Points(false)); !reflect.DeepEqual(got, []string{"mention ann"}) {
		t.Errorf("on someone else's: %v; want only the review that names me", got)
	}

	d = Discussion{Viewer: "me", Reviews: []Review{review("sam", 60, "changes requested", "Two things inline."), review("kim", 60, "changes requested", "One thing overall.")},
		Threads: []Thread{thread("a.go", by("sam", 30, "written while reviewing")), thread("b.go", by("kim", -200, "from an earlier review"))}}
	if got := kinds(d.Points(true)); !reflect.DeepEqual(got, []string{"review kim", "thread sam", "thread kim"}) {
		t.Errorf("%v; want a review's own words left out when its threads already wait, and kept when they do not", got)
	}
}

func TestPointsOfCommitsSinceAReview(t *testing.T) {
	commits := []Commit{{ID: "a1", Headline: "Before", Author: "sam", At: mins(1)}, {ID: "b2", Headline: "Fix one", Author: "sam", At: mins(20)},
		{ID: "c3", Headline: "Mine", Author: "me", At: mins(25)}, {ID: "d4", Headline: "Fix two", Author: "sam", At: mins(30)}}
	d := Discussion{Viewer: "me", Commits: commits, Reviews: []Review{review("me", 5, "approved", ""), review("me", 10, "changes requested", "")}}
	got := d.Points(false)
	if len(got) != 1 || got[0].Kind != PointCommits || got[0].ID != "commits:d4" || got[0].Who != "sam" || got[0].Text != "2 commits, the latest: Fix two" || !got[0].At.Equal(mins(30)) {
		t.Errorf("points = %+v; want one for the two commits someone else pushed since my last review", got)
	}
	if got := d.Points(true); len(got) != 0 {
		t.Errorf("on my own pull request: %v", kinds(got))
	}
	d.Commits = commits[:2]
	if got := d.Points(false); len(got) != 1 || got[0].Text != "Fix one" {
		t.Errorf("points = %+v; want a single commit told by its headline", got)
	}
	d.Reviews = []Review{review("me", 10, "approved", "")}
	if got := d.Points(false); len(got) != 0 {
		t.Errorf("after I approved: %v; want commits since to need nothing of me", kinds(got))
	}
}

func TestPointsAreCutToSize(t *testing.T) {
	d := Discussion{Viewer: "me"}
	for i := range pointLimit + 3 {
		d.Comments = append(d.Comments, by("sam", i, " "+strings.Repeat("é", pointText+10)+" "))
	}
	got := d.Points(true)
	if len(got) != pointLimit || !got[0].At.Equal(mins(pointLimit+2)) {
		t.Fatalf("%d points from %v; want the latest %d", len(got), got[0].At, pointLimit)
	}
	if got[0].Text != strings.Repeat("é", pointText)+"…" {
		t.Errorf("text is %d runes; want it cut to %d and marked", len([]rune(got[0].Text)), pointText)
	}
	long := Discussion{Viewer: "me", Threads: []Thread{thread("a.go", by("sam", 1, strings.Repeat("é", pointText+10)))}}
	if got := long.Points(true); len(got) != 1 || got[0].Text != strings.Repeat("é", pointText)+"…" {
		t.Errorf("a thread's last word is %d runes; want it cut to %d and marked", len([]rune(got[0].Text)), pointText)
	}
}

// answering is a stand-in for gh that gives one answer and remembers what
// it was asked.
type answering struct {
	out   string
	fail  int
	calls [][]string
}

func (a *answering) run(_ context.Context, dir string, argv ...string) ([]byte, error) {
	a.calls = append(a.calls, argv)
	if len(a.calls) <= a.fail {
		return nil, errors.New("HTTP 502: Bad Gateway")
	}
	return []byte(a.out), nil
}

const discussed = `{"data":{"viewer":{"login":"me"},"repository":{"pullRequest":{
 "body":"  What it does.  ","additions":12,"deletions":3,"changedFiles":2,
 "comments":{"nodes":[{"body":"hello","createdAt":"2026-11-17T12:01:00Z","url":"https://github.com/a/b/pull/7#c1","author":{"login":"sam","__typename":"User"}},
   {"body":"coverage","createdAt":"2026-11-17T12:02:00Z","url":"javascript:alert(1)","author":{"login":"ci","__typename":"Bot"}},
   {"body":"from a deleted account","createdAt":"2026-11-17T12:03:00Z","url":"https://github.com/a/b/pull/7#c3","author":null}]},
 "reviews":{"nodes":[{"state":"CHANGES_REQUESTED","body":"split it","submittedAt":"2026-11-17T13:00:00Z","createdAt":"2026-11-17T09:00:00Z","url":"https://github.com/a/b/pull/7#r1","author":{"login":"sam","__typename":"User"}},
   {"state":"PENDING","body":"not sent yet","submittedAt":null,"url":"https://github.com/a/b/pull/7#r2","author":{"login":"me","__typename":"User"}},
   {"state":"APPROVED","body":"","submittedAt":"2026-11-17T14:00:00Z","url":"https://github.com/a/b/pull/7#r3","author":{"login":"kim","__typename":"User"}}]},
 "reviewThreads":{"nodes":[{"isResolved":true,"isOutdated":true,"path":"a.go","line":null,"originalLine":41,
   "comments":{"nodes":[{"body":"why?","createdAt":"2026-11-17T12:30:00Z","url":"https://github.com/a/b/pull/7#t1","author":{"login":"sam","__typename":"User"}}]}},
   {"isResolved":false,"isOutdated":false,"path":"b.go","line":9,"originalLine":8,"comments":{"nodes":[]}}]},
 "reviewRequests":{"nodes":[{"requestedReviewer":{"login":"lee"}},{"requestedReviewer":{"name":"platform"}},{"requestedReviewer":null},{"requestedReviewer":{}}]},
 "commits":{"totalCount":9,"nodes":[{"commit":{"abbreviatedOid":"abc1234","messageHeadline":"Add it","committedDate":"2026-11-17T11:00:00Z","author":{"name":"Sam Smith","user":{"login":"sam"}}}},
   {"commit":{"abbreviatedOid":"def5678","messageHeadline":"Fix it","committedDate":"2026-11-17T11:30:00Z","author":{"name":"Someone Unlinked","user":null}}}]},
 "tip":{"nodes":[{"commit":{"statusCheckRollup":{"contexts":{"nodes":[{"name":"unit","status":"COMPLETED","conclusion":"SUCCESS"},{"name":"lint","status":"COMPLETED","conclusion":"FAILURE"},
   {"context":"deploy/preview","state":"ERROR"},{"name":"slow","status":"IN_PROGRESS","conclusion":null},{"context":"legal","state":"PENDING"},{"name":"e2e","status":"COMPLETED","conclusion":"TIMED_OUT"}]}}}}]}
}}}}`

func TestGitHubDiscussion(t *testing.T) {
	gh := &answering{out: discussed}
	d, err := githubDiscussion(gh.run)(context.Background(), "a/b", 7)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"gh", "api", "graphql", "-f", "query=" + discussionQuery, "-f", "owner=a", "-f", "name=b", "-F", "number=7"}
	if len(gh.calls) != 1 || !reflect.DeepEqual(gh.calls[0], want) {
		t.Errorf("asked %q; want the one fixed query, with only the repository and number filled in", gh.calls)
	}
	if strings.Contains(discussionQuery, "mutation") || !strings.HasPrefix(discussionQuery, "query(") {
		t.Error("the discussion is not asked for with a read-only query")
	}

	if d.Viewer != "me" || d.Body != "What it does." || d.Additions != 12 || d.Deletions != 3 || d.Files != 2 || d.CommitCount != 9 {
		t.Errorf("discussion = %+v", d)
	}
	at := func(clock string) time.Time {
		return time.Date(2026, 11, 17, int(clock[0]-'0')*10+int(clock[1]-'0'), int(clock[3]-'0')*10+int(clock[4]-'0'), 0, 0, time.UTC)
	}
	comments := []Comment{{Author: "sam", Body: "hello", At: at("12:01"), URL: "https://github.com/a/b/pull/7#c1"}, {Author: "ci", Bot: true, Body: "coverage", At: at("12:02")},
		{Author: "someone", Body: "from a deleted account", At: at("12:03"), URL: "https://github.com/a/b/pull/7#c3"}}
	if !reflect.DeepEqual(d.Comments, comments) {
		t.Errorf("comments = %+v\nwant %+v", d.Comments, comments)
	}
	reviews := []Review{{Comment: Comment{Author: "sam", Body: "split it", At: at("13:00"), URL: "https://github.com/a/b/pull/7#r1"}, State: "changes requested"},
		{Comment: Comment{Author: "kim", At: at("14:00"), URL: "https://github.com/a/b/pull/7#r3"}, State: "approved"}}
	if !reflect.DeepEqual(d.Reviews, reviews) {
		t.Errorf("reviews = %+v\nwant those given, each timed by when it was given, and none still being written", d.Reviews)
	}
	threads := []Thread{{Path: "a.go", Line: 41, Resolved: true, Outdated: true, Comments: []Comment{{Author: "sam", Body: "why?", At: at("12:30"), URL: "https://github.com/a/b/pull/7#t1"}}},
		{Path: "b.go", Line: 9, Comments: []Comment{}}}
	if !reflect.DeepEqual(d.Threads, threads) {
		t.Errorf("threads = %+v\nwant %+v", d.Threads, threads)
	}
	commits := []Commit{{ID: "abc1234", Headline: "Add it", Author: "sam", At: at("11:00")}, {ID: "def5678", Headline: "Fix it", Author: "Someone Unlinked", At: at("11:30")}}
	if !reflect.DeepEqual(d.Commits, commits) {
		t.Errorf("commits = %+v\nwant %+v", d.Commits, commits)
	}
	if !reflect.DeepEqual(d.Requested, []string{"lee", "platform"}) || !reflect.DeepEqual(d.Failing, []string{"lint", "deploy/preview", "e2e"}) {
		t.Errorf("requested %v, failing %v; want the people and teams still asked, and the checks that failed", d.Requested, d.Failing)
	}
	if raw, _ := json.Marshal(d); strings.Contains(string(raw), "null") {
		t.Errorf("a read discussion has lists the page cannot walk: %s", raw)
	}
}

func TestGitHubDiscussionRefusesWhatIsNoPullRequest(t *testing.T) {
	gh := &answering{out: discussed}
	read := githubDiscussion(gh.run)
	for _, repo := range []string{"", "a", "a/b/c", "a/b c", "a/b;rm", "a/b\"", "$a/b"} {
		if _, err := read(context.Background(), repo, 7); err == nil {
			t.Errorf("read a discussion from %q", repo)
		}
	}
	if _, err := read(context.Background(), "a/b", 0); err == nil {
		t.Error("read the discussion of pull request 0")
	}
	if len(gh.calls) != 0 {
		t.Errorf("gh was run %d times for what names no pull request", len(gh.calls))
	}

	gh = &answering{out: `{"data":{"viewer":{"login":"me"},"repository":{"pullRequest":null}}}`}
	if _, err := githubDiscussion(gh.run)(context.Background(), "a/b", 7); err == nil {
		t.Error("a pull request GitHub does not have read as an empty discussion")
	}
	gh = &answering{out: discussed, fail: 1}
	if d, err := githubDiscussion(gh.run)(context.Background(), "a/b", 7); err != nil || d.Viewer != "me" || len(gh.calls) != 2 {
		t.Errorf("after one failure: %v, asked %d times; want it asked once more", err, len(gh.calls))
	}
	gh = &answering{out: discussed, fail: 2}
	if _, err := githubDiscussion(gh.run)(context.Background(), "a/b", 7); err == nil || len(gh.calls) != 2 {
		t.Errorf("after two failures: %v, asked %d times; want it given up on", err, len(gh.calls))
	}
}

func TestManagerDiscuss(t *testing.T) {
	asked := 0
	discusses := Integration{ID: "talks", Check: func(context.Context) (bool, string) { return true, "" },
		Discuss: func(_ context.Context, repo string, number int) (Discussion, error) {
			asked++
			return Discussion{Body: repo + "#" + string(rune('0'+number))}, nil
		}}
	silent := Integration{ID: "silent", Check: func(context.Context) (bool, string) { return true, "" }}
	down := discusses
	down.ID, down.Check = "down", func(context.Context) (bool, string) { return false, "" }
	on := map[string]bool{"talks": true, "silent": true, "down": true}
	m := NewManager(func() map[string]bool { return on }, discusses, silent, down)

	if d, err := m.Discuss(context.Background(), "talks", "a/b", 7); err != nil || d.Body != "a/b#7" {
		t.Errorf("Discuss = %+v, %v", d, err)
	}
	for _, id := range []string{"silent", "down", "nobody"} {
		if _, err := m.Discuss(context.Background(), id, "a/b", 7); err == nil {
			t.Errorf("read a discussion through %q", id)
		}
	}
	on["talks"] = false
	if _, err := m.Discuss(context.Background(), "talks", "a/b", 7); err == nil || asked != 1 {
		t.Errorf("an integration that is turned off was asked: %v, %d times in all", err, asked)
	}
}

// reviewing stands in for gh on a pull request the user reviewed: it gives
// the discussion pr describes, and compared when asked how two commits
// differ.
type reviewing struct {
	pr       string
	compared string
	fail     bool
	calls    [][]string
}

func (r *reviewing) run(_ context.Context, dir string, argv ...string) ([]byte, error) {
	r.calls = append(r.calls, argv)
	if len(argv) > 2 && argv[2] == "graphql" {
		return []byte(`{"data":{"viewer":{"login":"me"},"repository":{"pullRequest":{` + r.pr + `}}}}`), nil
	}
	if r.fail {
		return nil, errors.New("HTTP 404: Not Found")
	}
	return []byte(r.compared), nil
}

const (
	seen = "aaaaaaa111111111111111111111111111111111"
	head = "bbbbbbb222222222222222222222222222222222"
)

// reviewedAt is a pull request by who, now at head, that the user reviewed
// at the commit at.
func reviewedAt(who, at string) string {
	return `"headRefOid":"` + head + `","author":{"login":"` + who + `"},"files":{"totalCount":3,"nodes":[{"path":"a.go"},{"path":"b.go"},{"path":"c.go"}]},
	 "reviews":{"nodes":[{"state":"COMMENTED","submittedAt":"2026-11-17T10:00:00Z","author":{"login":"Me"},"commit":{"oid":"0000000000"}},
	   {"state":"APPROVED","submittedAt":"2026-11-17T11:00:00Z","author":{"login":"kim"},"commit":{"oid":"cccccccccc"}},
	   {"state":"CHANGES_REQUESTED","submittedAt":"2026-11-17T12:00:00Z","author":{"login":"me"},"commit":{"oid":"` + at + `"}},
	   {"state":"PENDING","author":{"login":"me"},"commit":{"oid":"dddddddddd"}}]}`
}

func TestGitHubDiscussionSaysWhatWasPushedSinceAReview(t *testing.T) {
	compared := `{"html_url":"https://github.com/a/b/compare/x...y","status":"ahead","total_commits":2,"files":[
	  {"filename":"a.go","status":"modified","additions":3,"deletions":1,"patch":"@@ a"},
	  {"filename":"merged/from/main.go","status":"added","additions":90,"deletions":0,"patch":"@@ noise"},
	  {"filename":"c.go","status":"removed","additions":0,"deletions":7}]}`
	gh := &reviewing{pr: reviewedAt("sam", seen), compared: compared}
	d, err := githubDiscussion(gh.run)(context.Background(), "a/b", 7)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"gh", "api", "repos/a/b/compare/" + seen + "..." + head}; len(gh.calls) != 2 || !reflect.DeepEqual(gh.calls[1], want) {
		t.Fatalf("asked %q; want the discussion, and then one read of how the commit I last reviewed differs from the newest", gh.calls)
	}
	want := &Changes{From: "aaaaaaa", To: "bbbbbbb", URL: "https://github.com/a/b/compare/x...y", Commits: 2,
		Files: []FileChange{{Path: "a.go", Status: "modified", Additions: 3, Deletions: 1, Patch: "@@ a"}, {Path: "c.go", Status: "removed", Deletions: 7}}}
	if !reflect.DeepEqual(d.Since, want) {
		t.Errorf("since = %+v\nwant %+v, without the file the pull request does not change", d.Since, want)
	}

	none := []struct{ name, pr string }{
		{"my own pull request", reviewedAt("ME", seen)},
		{"nothing pushed since", reviewedAt("sam", head)},
		{"a commit id that is none", reviewedAt("sam", "HEAD~1")},
		{"no review of mine", `"headRefOid":"` + head + `","author":{"login":"sam"},"reviews":{"nodes":[{"state":"APPROVED","author":{"login":"kim"},"commit":{"oid":"` + seen + `"}}]}`},
		{"only a review I have not sent", `"headRefOid":"` + head + `","author":{"login":"sam"},"reviews":{"nodes":[{"state":"PENDING","author":{"login":"me"},"commit":{"oid":"` + seen + `"}}]}`},
		{"no newest commit", strings.Replace(reviewedAt("sam", seen), head, "", 1)},
	}
	for _, tt := range none {
		gh := &reviewing{pr: tt.pr, compared: compared}
		if d, err := githubDiscussion(gh.run)(context.Background(), "a/b", 7); err != nil || d.Since != nil || len(gh.calls) != 1 {
			t.Errorf("%s: since %+v, %v, after %d requests; want nothing compared", tt.name, d.Since, err, len(gh.calls))
		}
	}

	gh = &reviewing{pr: reviewedAt("sam", seen), fail: true}
	if d, err := githubDiscussion(gh.run)(context.Background(), "a/b", 7); err != nil || d.Since != nil || d.Viewer != "me" || len(d.Reviews) != 3 {
		t.Errorf("when the commits cannot be compared: %+v, %v; want the discussion without it", d, err)
	}

	gh = &reviewing{pr: strings.Replace(reviewedAt("sam", seen), `"totalCount":3`, `"totalCount":4`, 1),
		compared: `{"html_url":"javascript:alert(1)","status":"diverged","total_commits":5,"files":[{"filename":"elsewhere.go","status":"added","additions":1}]}`}
	d, _ = githubDiscussion(gh.run)(context.Background(), "a/b", 7)
	if d.Since == nil || !d.Since.Rebuilt || d.Since.URL != "" || len(d.Since.Files) != 1 || d.Since.Commits != 5 {
		t.Errorf("since = %+v; want a branch rebuilt since to be told as one, every file kept when not all the pull request's are known, and no link that is not https", d.Since)
	}
}

func TestChangesAreCutToSize(t *testing.T) {
	var files []string
	for i := range changedFiles + 3 {
		patch := strings.Repeat("é", 10)
		switch i {
		case 0:
			patch = strings.Repeat("é", patchRunes+500)
		case 2:
			patch = strings.Repeat("x", patchesRunes)
		}
		files = append(files, `{"filename":"f`+strconv.Itoa(i)+`.go","status":"modified","additions":1,"deletions":1,"patch":"`+patch+`"}`)
	}
	gh := &reviewing{compared: `{"status":"ahead","total_commits":1,"files":[` + strings.Join(files, ",") + `]}`}
	c, err := githubChanges(context.Background(), gh.run, "a/b", seen, head, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Files) != changedFiles || c.More != 3 || c.Files[changedFiles-1].Path != "f"+strconv.Itoa(changedFiles-1)+".go" {
		t.Fatalf("kept %d files and counted %d more; want the first %d kept and the rest counted", len(c.Files), c.More, changedFiles)
	}
	if got := c.Files[0].Patch; got != strings.Repeat("é", patchRunes)+"…" {
		t.Errorf("a long difference is kept as %d runes; want it cut to %d and marked", len([]rune(got)), patchRunes)
	}
	if c.Files[1].Patch == "" || c.Files[2].Patch != "" || c.Files[2].Additions != 1 || c.Files[3].Patch == "" {
		t.Errorf("patches kept: %v, %v, %v; want a difference there is no room for left out, and smaller ones after it kept", c.Files[1].Patch != "", c.Files[2].Patch != "", c.Files[3].Patch != "")
	}
	total := 0
	for _, f := range c.Files {
		total += len([]rune(f.Patch))
	}
	if total > patchesRunes+changedFiles {
		t.Errorf("kept %d runes of differences in all; want at most %d", total, patchesRunes)
	}
}
