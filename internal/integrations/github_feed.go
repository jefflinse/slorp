package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Kinds of feed item GitHub produces.
const (
	FeedReview  = "review-requested"
	FeedChecks  = "checks-failing"
	FeedChanges = "changes-requested"
	FeedReady   = "ready-to-merge"
)

// feedQuery asks, in one read-only request, for the user's open pull
// requests and the open ones waiting on their review.
const feedQuery = `query {
  mine: search(query: "is:pr is:open author:@me archived:false", type: ISSUE, first: 50) {
    nodes { ... on PullRequest { number title url isDraft updatedAt reviewDecision mergeable headRefOid
      repository { nameWithOwner viewerPermission }
      commits(last: 1) { nodes { commit { statusCheckRollup { state } } } } } }
  }
  review: search(query: "is:pr is:open review-requested:@me archived:false", type: ISSUE, first: 50) {
    nodes { ... on PullRequest { number title url isDraft updatedAt
      author { login __typename }
      repository { nameWithOwner } } }
  }
}`

var githubWatches = []string{
	"Pull requests where your review is requested.",
	"Your pull requests whose checks are failing.",
	"Your pull requests where a reviewer asked for changes.",
	"Your pull requests that are ready to merge.",
}

var (
	checkWords = map[string]string{"SUCCESS": "passing", "FAILURE": "failing", "ERROR": "failing", "": "none"}
	mergeWords = map[string]string{"MERGEABLE": " No conflicts with its base branch.", "CONFLICTING": " It conflicts with its base branch."}
)

type feedPR struct {
	Number         int       `json:"number"`
	Title          string    `json:"title"`
	URL            string    `json:"url"`
	IsDraft        bool      `json:"isDraft"`
	UpdatedAt      time.Time `json:"updatedAt"`
	ReviewDecision string    `json:"reviewDecision"`
	Mergeable      string    `json:"mergeable"`
	Head           string    `json:"headRefOid"`
	Author         struct {
		Login string `json:"login"`
		Type  string `json:"__typename"`
	} `json:"author"`
	Repository struct {
		Name       string `json:"nameWithOwner"`
		Permission string `json:"viewerPermission"`
	} `json:"repository"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				Rollup *struct {
					State string `json:"state"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

// checks is the state of the pull request's newest commit, empty when it has
// no checks.
func (p feedPR) checks() string {
	if n := len(p.Commits.Nodes); n > 0 && p.Commits.Nodes[n-1].Commit.Rollup != nil {
		return p.Commits.Nodes[n-1].Commit.Rollup.State
	}
	return ""
}

func githubFeed(run Runner) func(ctx context.Context) ([]FeedItem, error) {
	return func(ctx context.Context) ([]FeedItem, error) {
		out, err := run(ctx, "", "gh", "api", "graphql", "-f", "query="+feedQuery)
		if err != nil {
			return nil, err
		}
		var res struct {
			Data struct {
				Mine   struct{ Nodes []feedPR } `json:"mine"`
				Review struct{ Nodes []feedPR } `json:"review"`
			} `json:"data"`
		}
		if json.Unmarshal(out, &res) != nil {
			return nil, errors.New("gh returned something unreadable")
		}
		return needs(res.Data.Mine.Nodes, res.Data.Review.Nodes), nil
	}
}

// needs picks out the pull requests that are waiting on the user. Drafts,
// and the user's own pull requests still waiting on someone else, are not.
func needs(mine, review []feedPR) []FeedItem {
	out := []FeedItem{}
	for _, p := range mine {
		if p.Number == 0 || p.IsDraft {
			continue
		}
		failing := p.checks() == "FAILURE" || p.checks() == "ERROR"
		passing := p.checks() == "SUCCESS" || p.checks() == ""
		approved := p.ReviewDecision == "APPROVED" || p.ReviewDecision == ""
		canMerge := p.Repository.Permission == "ADMIN" || p.Repository.Permission == "MAINTAIN" || p.Repository.Permission == "WRITE"
		it := p.item("opened by you")
		it.Summary += fmt.Sprintf(" Review: %s. Checks: %s.%s",
			orWord(strings.ToLower(strings.ReplaceAll(p.ReviewDecision, "_", " ")), "none required"),
			orWord(checkWords[p.checks()], "still running"), mergeWords[p.Mergeable])
		switch {
		case failing:
			it.Kind, it.Urgency = FeedChecks, "soon"
			it.Title, it.Ask = "Checks failing: "+p.Title, "Checks are failing on your pull request. Fix them or run them again."
			it.Key += ":" + p.Head
		case p.ReviewDecision == "CHANGES_REQUESTED":
			it.Kind, it.Urgency = FeedChanges, "soon"
			it.Title, it.Ask = "Changes requested: "+p.Title, "A reviewer asked for changes on your pull request."
		case passing && approved && canMerge && p.Mergeable == "MERGEABLE":
			it.Kind, it.Urgency = FeedReady, "whenever"
			it.Title, it.Ask = "Ready to merge: "+p.Title, "Your pull request has nothing left in its way. Merge it when you are ready."
		default:
			continue
		}
		it.Key += ":" + it.Kind
		out = append(out, it)
	}
	for _, p := range review {
		if p.Number == 0 || p.IsDraft {
			continue
		}
		who := orWord(p.Author.Login, "someone")
		it := p.item("opened by " + who)
		it.Kind, it.Urgency = FeedReview, "soon"
		it.Title, it.Ask = "Review requested: "+p.Title, "Your review is requested on "+who+"'s pull request."
		if p.Author.Type == "Bot" {
			it.Urgency = "whenever"
		}
		it.Key += ":" + it.Kind
		out = append(out, it)
	}
	return out
}

func (p feedPR) item(by string) FeedItem {
	it := FeedItem{
		Key:     fmt.Sprintf("%s#%d", p.Repository.Name, p.Number),
		Repo:    p.Repository.Name,
		At:      p.UpdatedAt,
		Summary: fmt.Sprintf("%s#%d, %s.", p.Repository.Name, p.Number, by),
	}
	if strings.HasPrefix(p.URL, "https://") {
		it.URL = p.URL
	}
	return it
}

// maxScanned is how many entries of a directory are searched for clones.
const maxScanned = 200

// clones names, as "owner/name" in lower case, the repositories a git
// checkout at dir, around dir, or directly inside dir was cloned from.
func clones(dir string) []string {
	if dir == "" {
		return nil
	}
	for at := dir; ; at = filepath.Dir(at) {
		if isRepo(at) {
			return remotes(at)
		}
		if at == filepath.Dir(at) {
			break
		}
	}
	entries, _ := os.ReadDir(dir)
	seen := map[string]bool{}
	var out []string
	for i, e := range entries {
		if i >= maxScanned {
			break
		}
		if !e.IsDir() {
			continue
		}
		for _, r := range remotes(filepath.Join(dir, e.Name())) {
			if !seen[r] {
				seen[r] = true
				out = append(out, r)
			}
		}
	}
	return out
}

var remoteRepo = regexp.MustCompile(`[:/]([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+?)(?:\.git)?/?$`)

// remotes reads the repositories a checkout's remotes point at. A linked
// worktree is read through the checkout it belongs to.
func remotes(root string) []string {
	gitDir := filepath.Join(root, ".git")
	if b, err := os.ReadFile(gitDir); err == nil {
		linked := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "gitdir:"))
		if !filepath.IsAbs(linked) {
			linked = filepath.Join(root, linked)
		}
		gitDir = linked
		if common, err := os.ReadFile(filepath.Join(linked, "commondir")); err == nil {
			gitDir = strings.TrimSpace(string(common))
			if !filepath.IsAbs(gitDir) {
				gitDir = filepath.Join(linked, gitDir)
			}
		}
	}
	config, err := os.ReadFile(filepath.Join(gitDir, "config"))
	if err != nil {
		return nil
	}
	var out []string
	inRemote := false
	for _, line := range strings.Split(string(config), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			inRemote = strings.HasPrefix(line, `[remote "`)
			continue
		}
		key, url, found := strings.Cut(line, "=")
		if !inRemote || !found || strings.TrimSpace(key) != "url" {
			continue
		}
		url = strings.TrimSpace(url)
		if !strings.Contains(url, "://") && !strings.Contains(url, "@") {
			continue
		}
		if m := remoteRepo.FindStringSubmatch(url); m != nil {
			out = append(out, strings.ToLower(m[1]+"/"+m[2]))
		}
	}
	return out
}
