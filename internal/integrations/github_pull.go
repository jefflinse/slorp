package integrations

import (
	"cmp"
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// discussionQuery asks, in one read-only request, what has been said and
// done on one pull request: the latest of its comments, reviews, line
// conversations and commits, who is still asked to review, which files it
// changes, and how the checks on its newest commit came out.
const discussionQuery = `query($owner: String!, $name: String!, $number: Int!) {
  viewer { login }
  repository(owner: $owner, name: $name) { pullRequest(number: $number) {
    body additions deletions changedFiles headRefOid
    author { login }
    files(first: 100) { totalCount nodes { path } }
    comments(last: 40) { nodes { body createdAt url author { login __typename } } }
    reviews(last: 40) { nodes { state body submittedAt url author { login __typename } commit { oid } } }
    reviewThreads(last: 50) { nodes { isResolved isOutdated path line originalLine
      comments(last: 10) { nodes { body createdAt url author { login __typename } } } } }
    reviewRequests(first: 20) { nodes { requestedReviewer { ... on User { login } ... on Bot { login } ... on Mannequin { login } ... on Team { name } } } }
    commits(last: 30) { totalCount nodes { commit { abbreviatedOid messageHeadline committedDate author { name user { login } } } } }
    tip: commits(last: 1) { nodes { commit { statusCheckRollup { contexts(first: 60) { nodes {
      ... on CheckRun { name status conclusion } ... on StatusContext { context state } } } } } } }
  } }
}`

var (
	repoName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	commitID = regexp.MustCompile(`^[0-9a-f]{7,64}$`)
)

const (
	// changedFiles is how many changed files are kept, patchRunes how much
	// of one file's difference, and patchesRunes how much of them all.
	changedFiles = 40
	patchRunes   = 4000
	patchesRunes = 16000
)

var reviewWords = map[string]string{"APPROVED": "approved", "CHANGES_REQUESTED": "changes requested", "COMMENTED": "commented", "DISMISSED": "dismissed"}

// said is a comment as GitHub gives it.
type said struct {
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
	URL       string    `json:"url"`
	Author    struct {
		Login string `json:"login"`
		Type  string `json:"__typename"`
	} `json:"author"`
}

func (s said) comment() Comment {
	c := Comment{Author: cmp.Or(s.Author.Login, "someone"), Bot: s.Author.Type == "Bot", Body: strings.TrimSpace(s.Body), At: s.CreatedAt}
	if strings.HasPrefix(s.URL, "https://") {
		c.URL = s.URL
	}
	return c
}

// githubDiscussion reads the discussion of pull request number of repo,
// which is "owner/name".
func githubDiscussion(run Runner) func(ctx context.Context, repo string, number int) (Discussion, error) {
	return func(ctx context.Context, repo string, number int) (Discussion, error) {
		owner, name, _ := strings.Cut(repo, "/")
		if !repoName.MatchString(repo) || number < 1 {
			return Discussion{}, errors.New("no such pull request")
		}
		var res struct {
			Data struct {
				Viewer     struct{ Login string }
				Repository struct {
					PullRequest *struct {
						Body         string
						Additions    int
						Deletions    int
						ChangedFiles int
						HeadRefOid   string
						Author       struct{ Login string }
						Files        struct {
							TotalCount int
							Nodes      []struct{ Path string }
						}
						Comments struct{ Nodes []said }
						Reviews  struct {
							Nodes []struct {
								said
								State       string
								SubmittedAt time.Time
								Commit      *struct{ Oid string }
							}
						}
						ReviewThreads struct {
							Nodes []struct {
								IsResolved   bool
								IsOutdated   bool
								Path         string
								Line         int
								OriginalLine int
								Comments     struct{ Nodes []said }
							}
						}
						ReviewRequests struct {
							Nodes []struct {
								RequestedReviewer *struct{ Login, Name string }
							}
						}
						Commits struct {
							TotalCount int
							Nodes      []struct {
								Commit struct {
									AbbreviatedOid  string
									MessageHeadline string
									CommittedDate   time.Time
									Author          struct {
										Name string
										User *struct{ Login string }
									}
								}
							}
						}
						Tip struct {
							Nodes []struct {
								Commit struct {
									StatusCheckRollup *struct {
										Contexts struct {
											Nodes []struct{ Name, Status, Conclusion, Context, State string }
										}
									}
								}
							}
						}
					}
				}
			}
		}
		argv := []string{"gh", "api", "graphql", "-f", "query=" + discussionQuery, "-f", "owner=" + owner, "-f", "name=" + name, "-F", "number=" + strconv.Itoa(number)}
		if err := answer(ctx, run, &res, argv); err != nil {
			return Discussion{}, err
		}
		pr := res.Data.Repository.PullRequest
		if pr == nil {
			return Discussion{}, errors.New("no such pull request")
		}
		d := Discussion{Viewer: res.Data.Viewer.Login, Body: strings.TrimSpace(pr.Body), Additions: pr.Additions, Deletions: pr.Deletions, Files: pr.ChangedFiles,
			Comments: []Comment{}, Reviews: []Review{}, Threads: []Thread{}, Commits: []Commit{}, CommitCount: pr.Commits.TotalCount, Requested: []string{}, Failing: []string{}}
		for _, c := range pr.Comments.Nodes {
			d.Comments = append(d.Comments, c.comment())
		}
		reviewed := ""
		for _, r := range pr.Reviews.Nodes {
			// A review still being written has no verdict to report.
			if state := reviewWords[r.State]; state != "" {
				c := r.comment()
				c.At = r.SubmittedAt
				d.Reviews = append(d.Reviews, Review{Comment: c, State: state})
				if sameLogin(c.Author, d.Viewer) && r.Commit != nil {
					reviewed = r.Commit.Oid
				}
			}
		}
		if !sameLogin(pr.Author.Login, d.Viewer) && reviewed != pr.HeadRefOid && commitID.MatchString(reviewed) && commitID.MatchString(pr.HeadRefOid) {
			var within map[string]bool
			if pr.Files.TotalCount <= len(pr.Files.Nodes) {
				within = map[string]bool{}
				for _, f := range pr.Files.Nodes {
					within[f.Path] = true
				}
			}
			// The discussion is worth having without it.
			d.Since, _ = githubChanges(ctx, run, repo, reviewed, pr.HeadRefOid, within)
		}
		for _, t := range pr.ReviewThreads.Nodes {
			th := Thread{Path: t.Path, Line: cmp.Or(t.Line, t.OriginalLine), Resolved: t.IsResolved, Outdated: t.IsOutdated, Comments: []Comment{}}
			for _, c := range t.Comments.Nodes {
				th.Comments = append(th.Comments, c.comment())
			}
			d.Threads = append(d.Threads, th)
		}
		for _, r := range pr.ReviewRequests.Nodes {
			if who := r.RequestedReviewer; who != nil && who.Login+who.Name != "" {
				d.Requested = append(d.Requested, cmp.Or(who.Login, who.Name))
			}
		}
		for _, n := range pr.Commits.Nodes {
			c := n.Commit
			who := c.Author.Name
			if c.Author.User != nil {
				who = cmp.Or(c.Author.User.Login, who)
			}
			d.Commits = append(d.Commits, Commit{ID: c.AbbreviatedOid, Headline: c.MessageHeadline, Author: who, At: c.CommittedDate})
		}
		if n := len(pr.Tip.Nodes); n > 0 && pr.Tip.Nodes[n-1].Commit.StatusCheckRollup != nil {
			for _, c := range pr.Tip.Nodes[n-1].Commit.StatusCheckRollup.Contexts.Nodes {
				switch strings.ToUpper(c.Conclusion + c.State) {
				case "FAILURE", "ERROR", "TIMED_OUT", "STARTUP_FAILURE", "ACTION_REQUIRED":
					d.Failing = append(d.Failing, cmp.Or(c.Name, c.Context))
				}
			}
		}
		return d, nil
	}
}

// githubChanges reads how repo's code differs between two commits of a pull
// request, keeping the files named in within, or every file when within is
// nil. A branch merged into, or rebuilt since, the first commit differs in
// files the pull request does not touch.
func githubChanges(ctx context.Context, run Runner, repo, from, to string, within map[string]bool) (*Changes, error) {
	var res struct {
		URL     string `json:"html_url"`
		Status  string `json:"status"`
		Commits int    `json:"total_commits"`
		Files   []struct {
			Filename  string `json:"filename"`
			Status    string `json:"status"`
			Additions int    `json:"additions"`
			Deletions int    `json:"deletions"`
			Patch     string `json:"patch"`
		} `json:"files"`
	}
	if err := answer(ctx, run, &res, []string{"gh", "api", "repos/" + repo + "/compare/" + from + "..." + to}); err != nil {
		return nil, err
	}
	c := &Changes{From: from[:7], To: to[:7], Commits: res.Commits, Rebuilt: res.Status == "diverged", Files: []FileChange{}}
	if strings.HasPrefix(res.URL, "https://") {
		c.URL = res.URL
	}
	room := patchesRunes
	for _, f := range res.Files {
		if within != nil && !within[f.Filename] {
			continue
		}
		if len(c.Files) == changedFiles {
			c.More++
			continue
		}
		fc := FileChange{Path: f.Filename, Status: f.Status, Additions: f.Additions, Deletions: f.Deletions}
		if n := len([]rune(f.Patch)); n <= room {
			fc.Patch = clip(f.Patch, patchRunes)
			room -= min(n, patchRunes)
		}
		c.Files = append(c.Files, fc)
	}
	return c, nil
}
