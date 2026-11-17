package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Kinds of feed item GitHub produces.
const (
	FeedReview    = "review-requested"
	FeedChecks    = "checks-failing"
	FeedChanges   = "changes-requested"
	FeedConflicts = "conflicts"
	FeedReady     = "ready-to-merge"
)

// The pull requests the feed is about, as GitHub's search names them.
const (
	mineSearch   = "is:pr is:open author:@me archived:false"
	reviewSearch = "is:pr is:open review-requested:@me archived:false"
)

// listQuery asks, in one read-only request, which pull requests those are,
// leaving out everything GitHub has to work out for each one.
const listQuery = `query {
  mine: search(query: "` + mineSearch + `", type: ISSUE, first: 50) { nodes { ...pr } }
  review: search(query: "` + reviewSearch + `", type: ISSUE, first: 50) { nodes { ...pr } }
}
fragment pr on PullRequest { number title url isDraft updatedAt reviewDecision headRefOid
  author { login __typename }
  repository { nameWithOwner viewerPermission } }`

// detailQuery asks how one page of a search's pull requests stand. GitHub
// works these fields out as it answers, and gives up on a request that asks
// for too many at once.
const detailQuery = `query($q: String!, $after: String) {
  search(query: $q, type: ISSUE, first: 10, after: $after) {
    pageInfo { hasNextPage endCursor }
    nodes { ... on PullRequest { number mergeable
      repository { nameWithOwner }
      reviewRequests { totalCount }
      latestOpinionatedReviews(first: 50) { nodes { state } }
      commits(last: 1) { nodes { commit { statusCheckRollup { state } } } } } }
  }
}`

// detailPages caps how many pages of one search are asked about.
const detailPages = 5

var githubWatches = []string{
	"Pull requests where your review is requested.",
	"Your pull requests whose checks are failing.",
	"Your pull requests where a reviewer asked for changes.",
	"Your pull requests that conflict with their base branch.",
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
	ReviewRequests struct {
		TotalCount int `json:"totalCount"`
	} `json:"reviewRequests"`
	Reviews struct {
		Nodes []struct {
			State string `json:"state"`
		} `json:"nodes"`
	} `json:"latestOpinionatedReviews"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				Rollup *struct {
					State string `json:"state"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`

	// detailed is true once GitHub has said how the pull request stands.
	detailed bool
}

func (p feedPR) key() string { return fmt.Sprintf("%s#%d", p.Repository.Name, p.Number) }

// checks is the state of the pull request's newest commit, empty when it has
// no checks.
func (p feedPR) checks() string {
	if n := len(p.Commits.Nodes); n > 0 && p.Commits.Nodes[n-1].Commit.Rollup != nil {
		return p.Commits.Nodes[n-1].Commit.Rollup.State
	}
	return ""
}

func githubFeed(run Runner) func(ctx context.Context) (Found, error) {
	return func(ctx context.Context) (Found, error) {
		var listed struct {
			Data struct {
				Mine   struct{ Nodes []feedPR } `json:"mine"`
				Review struct{ Nodes []feedPR } `json:"review"`
			} `json:"data"`
		}
		if err := ask(ctx, run, &listed, listQuery); err != nil {
			return Found{}, err
		}
		mine, review := listed.Data.Mine.Nodes, listed.Data.Review.Nodes
		detail(ctx, run, mineSearch, mine)
		detail(ctx, run, reviewSearch, review)
		return survey(mine, review), nil
	}
}

// ask runs one read-only GraphQL query through gh, a second time if the
// first fails, and decodes the answer into v. vars are "name=value".
func ask(ctx context.Context, run Runner, v any, query string, vars ...string) error {
	argv := []string{"gh", "api", "graphql", "-f", "query=" + query}
	for _, kv := range vars {
		argv = append(argv, "-f", kv)
	}
	out, err := run(ctx, "", argv...)
	if err != nil {
		out, err = run(ctx, "", argv...)
	}
	if err != nil {
		return err
	}
	if json.Unmarshal(out, v) != nil {
		return errors.New("gh returned something unreadable")
	}
	return nil
}

// detail fills in how the listed pull requests of one search stand, a page
// at a time. Those on a page GitHub will not answer for, and on the pages
// after it, are left undetailed.
func detail(ctx context.Context, run Runner, search string, listed []feedPR) {
	byKey := map[string]*feedPR{}
	for i := range listed {
		byKey[listed[i].key()] = &listed[i]
	}
	vars := []string{"q=" + search}
	for page := 0; page < detailPages && len(listed) > 0; page++ {
		var res struct {
			Data struct {
				Search struct {
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []feedPR `json:"nodes"`
				} `json:"search"`
			} `json:"data"`
		}
		if err := ask(ctx, run, &res, detailQuery, vars...); err != nil {
			log.Printf("github: how some pull requests stand is unknown: %v", err)
			return
		}
		for _, d := range res.Data.Search.Nodes {
			if p := byKey[d.key()]; p != nil {
				p.Mergeable, p.ReviewRequests, p.Reviews, p.Commits, p.detailed = d.Mergeable, d.ReviewRequests, d.Reviews, d.Commits, true
			}
		}
		if !res.Data.Search.PageInfo.HasNextPage {
			return
		}
		vars = []string{"q=" + search, "after=" + res.Data.Search.PageInfo.EndCursor}
	}
}

// survey lists every pull request and picks out, as items, those waiting on
// the user. Drafts, and a pull request of the user's still waiting on
// someone else, are listed but are not items. Nothing is concluded from
// checks or mergeability GitHub did not report.
func survey(mine, review []feedPR) Found {
	found := Found{Items: []FeedItem{}, PullRequests: []PullRequest{}}
	for _, p := range mine {
		if p.Number == 0 {
			continue
		}
		failing := p.checks() == "FAILURE" || p.checks() == "ERROR"
		passing := p.checks() == "SUCCESS" || p.checks() == ""
		approved := p.ReviewDecision == "APPROVED" || p.ReviewDecision == ""
		canMerge := p.Repository.Permission == "ADMIN" || p.Repository.Permission == "MAINTAIN" || p.Repository.Permission == "WRITE"
		pr := p.pull(true)
		it := p.item("opened by you")
		it.Summary += fmt.Sprintf(" Review: %s.", orWord(strings.ToLower(strings.ReplaceAll(p.ReviewDecision, "_", " ")), "none required"))
		if p.detailed {
			it.Summary += fmt.Sprintf(" Checks: %s.%s", orWord(checkWords[p.checks()], "still running"), mergeWords[p.Mergeable])
		}
		switch {
		case p.IsDraft:
			pr.Needs = "Still a draft."
		case !p.detailed && p.ReviewDecision != "CHANGES_REQUESTED":
			pr.Needs = "GitHub did not say how this one stands."
		case failing:
			it.Kind, it.Urgency, pr.Needs = FeedChecks, "soon", "Checks are failing."
			it.Title, it.Ask = "Checks failing: "+p.Title, "Checks are failing on your pull request. Fix them or run them again."
			it.Key += ":" + p.Head
		case p.ReviewDecision == "CHANGES_REQUESTED":
			it.Kind, it.Urgency, pr.Needs = FeedChanges, "soon", "A reviewer asked for changes."
			it.Title, it.Ask = "Changes requested: "+p.Title, "A reviewer asked for changes on your pull request."
		case pr.Conflicts:
			it.Kind, it.Urgency, pr.Needs = FeedConflicts, "soon", "It conflicts with its base branch."
			it.Title, it.Ask = "Conflicts: "+p.Title, "Your pull request conflicts with its base branch. Resolve the conflicts so it can merge."
			it.Key += ":" + p.Head
		case passing && approved && canMerge && p.Mergeable == "MERGEABLE":
			it.Kind, it.Urgency, pr.Needs = FeedReady, "whenever", "Ready to merge."
			it.Title, it.Ask = "Ready to merge: "+p.Title, "Your pull request has nothing left in its way. Merge it when you are ready."
		case !failing && !passing:
			pr.Needs = "Checks are still running."
		case p.ReviewDecision == "REVIEW_REQUIRED":
			pr.Needs = "Waiting on review."
		default:
			pr.Needs = "Waiting on someone else."
		}
		if it.Kind != "" {
			pr.OnYou = true
			it.Key += ":" + it.Kind
			found.Items = append(found.Items, it)
		}
		found.PullRequests = append(found.PullRequests, pr)
	}
	for _, p := range review {
		if p.Number == 0 {
			continue
		}
		pr := p.pull(false)
		pr.Needs = "Your review is requested."
		if p.IsDraft {
			pr.Needs = "Your review is requested, but it is still a draft."
			found.PullRequests = append(found.PullRequests, pr)
			continue
		}
		pr.OnYou = true
		found.PullRequests = append(found.PullRequests, pr)
		who := orWord(p.Author.Login, "someone")
		it := p.item("opened by " + who)
		it.Kind, it.Urgency = FeedReview, "soon"
		it.Title, it.Ask = "Review requested: "+p.Title, "Your review is requested on "+who+"'s pull request."
		if p.Author.Type == "Bot" {
			it.Urgency = "whenever"
		}
		it.Key += ":" + it.Kind
		found.Items = append(found.Items, it)
	}
	return found
}

// pull is the pull request as the user's list of them shows it, before
// anything is said about whose move it is.
func (p feedPR) pull(mine bool) PullRequest {
	pr := PullRequest{Repo: p.Repository.Name, Number: p.Number, Title: p.Title, Author: p.Author.Login, Mine: mine, Draft: p.IsDraft,
		At: p.UpdatedAt, Awaiting: p.ReviewRequests.TotalCount, Checks: orWord(checkWords[p.checks()], "running"), Conflicts: p.Mergeable == "CONFLICTING"}
	if !p.detailed {
		pr.Checks = "unknown"
	}
	if strings.HasPrefix(p.URL, "https://") {
		pr.URL = p.URL
	}
	for _, r := range p.Reviews.Nodes {
		switch r.State {
		case "APPROVED":
			pr.Approvals++
		case "CHANGES_REQUESTED":
			pr.ChangesRequested++
		}
	}
	return pr
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
