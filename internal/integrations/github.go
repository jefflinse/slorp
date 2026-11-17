package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Runner runs a command in dir and returns its standard output.
type Runner func(ctx context.Context, dir string, argv ...string) ([]byte, error)

// ErrNotInstalled is what a Runner returns when the command does not exist.
var ErrNotInstalled = errors.New("not installed")

// Exec runs commands for real, without a shell, for at most 15 seconds.
func Exec(ctx context.Context, dir string, argv ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "NO_COLOR=1")
	out, err := cmd.Output()
	var missing *exec.Error
	if errors.As(err, &missing) {
		return nil, ErrNotInstalled
	}
	var failed *exec.ExitError
	if errors.As(err, &failed) {
		return out, errors.New(firstLine(string(failed.Stderr)))
	}
	return out, err
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return "the command failed"
	}
	return s
}

const (
	prFields     = "number,title,state,isDraft,mergeable,reviewDecision,statusCheckRollup,headRefName,baseRefName,updatedAt,url"
	issueFields  = "number,title,state,labels,assignees,updatedAt,url"
	lookupParams = `{"type":"object","properties":{"number":{"type":"integer","description":"Its number, without the #"},"repository":{"type":"string","description":"Which repository, by the name listed. Leave out when there is only one."}},"required":["number"],"additionalProperties":false}`

	maxRepos = 24
)

// repos finds the git repositories a session in dir is working with: dir
// itself, or else the repositories directly inside it.
func repos(dir string) []string {
	if dir == "" {
		return nil
	}
	if isRepo(dir) {
		return []string{"."}
	}
	entries, _ := os.ReadDir(dir)
	var out []string
	for _, e := range entries {
		if e.IsDir() && len(out) < maxRepos && isRepo(filepath.Join(dir, e.Name())) {
			out = append(out, e.Name())
		}
	}
	return out
}

func isRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// GitHub looks up pull requests and issues through the gh command, in the
// repository of the session being described.
func GitHub(run Runner) Integration {
	return Integration{
		ID:    "github",
		Name:  "GitHub",
		About: "Reads pull requests and issues with the gh command, signed in as you.",
		Check: func(ctx context.Context) (bool, string) {
			_, err := run(ctx, "", "gh", "auth", "status")
			switch {
			case errors.Is(err, ErrNotInstalled):
				return false, "The gh command is not installed."
			case err != nil:
				return false, "The gh command is installed but not signed in."
			}
			return true, "The gh command is installed and signed in."
		},
		Scope: func(dir string) (string, bool) {
			switch found := repos(dir); {
			case len(found) == 0:
				return "", false
			case len(found) == 1:
				return "GitHub lookups read the session's one repository.", true
			default:
				return "GitHub lookups read one of the repositories in the session's directory, which you must name: " + strings.Join(found, ", ") + ".", true
			}
		},
		Feed:    githubFeed(run),
		Watches: githubWatches,
		Clones:  clones,
		Tools: []Tool{
			{
				Name:        "github_pr",
				Description: "Look up a pull request in the session's repository: whether it is open or merged, its review decision, and its checks.",
				Params:      json.RawMessage(lookupParams),
				Label:       func(args json.RawMessage) string { return "GitHub PR " + label(args) },
				Run: func(ctx context.Context, dir string, args json.RawMessage) (string, error) {
					n, in, err := target(dir, args)
					if err != nil {
						return "", err
					}
					out, err := run(ctx, in, "gh", "pr", "view", strconv.Itoa(n), "--json", prFields)
					if err != nil {
						return "", err
					}
					return pullRequest(out)
				},
			},
			{
				Name:        "github_issue",
				Description: "Look up an issue in the session's repository: whether it is open, its labels and who it is assigned to.",
				Params:      json.RawMessage(lookupParams),
				Label:       func(args json.RawMessage) string { return "GitHub issue " + label(args) },
				Run: func(ctx context.Context, dir string, args json.RawMessage) (string, error) {
					n, in, err := target(dir, args)
					if err != nil {
						return "", err
					}
					out, err := run(ctx, in, "gh", "issue", "view", strconv.Itoa(n), "--json", issueFields)
					if err != nil {
						return "", err
					}
					return issue(out)
				},
			},
		},
	}
}

type lookup struct {
	Number     *int   `json:"number"`
	Repository string `json:"repository"`
}

// target turns a lookup's arguments into a number and the directory to run
// in. Only a positive integer and a repository found under dir get through,
// so the model cannot aim a lookup anywhere else.
func target(dir string, args json.RawMessage) (n int, in string, err error) {
	var l lookup
	if json.Unmarshal(args, &l) != nil || l.Number == nil || *l.Number < 1 || *l.Number > 99_999_999 {
		return 0, "", errors.New("needs a number")
	}
	found := repos(dir)
	if l.Repository == "" && len(found) == 1 {
		return *l.Number, filepath.Join(dir, found[0]), nil
	}
	for _, name := range found {
		if name == l.Repository {
			return *l.Number, filepath.Join(dir, name), nil
		}
	}
	if len(found) == 0 {
		return 0, "", errors.New("the session is not working in a git repository")
	}
	return 0, "", errors.New("name one of these repositories: " + strings.Join(found, ", "))
}

func label(args json.RawMessage) string {
	var l lookup
	if json.Unmarshal(args, &l) != nil || l.Number == nil {
		return "(no number)"
	}
	if l.Repository == "" || l.Repository == "." {
		return "#" + strconv.Itoa(*l.Number)
	}
	return l.Repository + "#" + strconv.Itoa(*l.Number)
}

// pullRequest condenses gh's JSON to what bears on a session's description.
func pullRequest(raw []byte) (string, error) {
	var pr struct {
		Number         int    `json:"number"`
		Title          string `json:"title"`
		State          string `json:"state"`
		IsDraft        bool   `json:"isDraft"`
		Mergeable      string `json:"mergeable"`
		ReviewDecision string `json:"reviewDecision"`
		Head           string `json:"headRefName"`
		Base           string `json:"baseRefName"`
		UpdatedAt      string `json:"updatedAt"`
		Checks         []struct {
			Name       string `json:"name"`
			Context    string `json:"context"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			State      string `json:"state"`
		} `json:"statusCheckRollup"`
	}
	if err := json.Unmarshal(raw, &pr); err != nil {
		return "", errors.New("gh returned something unreadable")
	}
	var passed, pending int
	var failing []string
	for _, c := range pr.Checks {
		name := c.Name
		if name == "" {
			name = c.Context
		}
		switch result := strings.ToUpper(c.Conclusion + c.State); {
		case c.Status != "" && c.Status != "COMPLETED", result == "PENDING", result == "EXPECTED":
			pending++
		case result == "SUCCESS", result == "NEUTRAL", result == "SKIPPED":
			passed++
		default:
			failing = append(failing, name)
		}
	}
	checks := "no checks"
	if len(pr.Checks) > 0 {
		checks = fmt.Sprintf("%d passed, %d failed, %d still running", passed, len(failing), pending)
		if len(failing) > 0 {
			checks += " (failing: " + strings.Join(failing, ", ") + ")"
		}
	}
	state := strings.ToLower(pr.State)
	if pr.IsDraft {
		state += ", draft"
	}
	return fmt.Sprintf("Pull request #%d %q: %s. Review: %s. Checks: %s. Mergeable: %s. Branch %s into %s. Last updated %s.",
		pr.Number, pr.Title, state, orWord(strings.ToLower(strings.ReplaceAll(pr.ReviewDecision, "_", " ")), "none required or none yet"),
		checks, orWord(strings.ToLower(pr.Mergeable), "unknown"), pr.Head, pr.Base, pr.UpdatedAt), nil
}

func issue(raw []byte) (string, error) {
	var is struct {
		Number    int                      `json:"number"`
		Title     string                   `json:"title"`
		State     string                   `json:"state"`
		UpdatedAt string                   `json:"updatedAt"`
		Labels    []struct{ Name string }  `json:"labels"`
		Assignees []struct{ Login string } `json:"assignees"`
	}
	if err := json.Unmarshal(raw, &is); err != nil {
		return "", errors.New("gh returned something unreadable")
	}
	var labels, who []string
	for _, l := range is.Labels {
		labels = append(labels, l.Name)
	}
	for _, a := range is.Assignees {
		who = append(who, a.Login)
	}
	return fmt.Sprintf("Issue #%d %q: %s. Labels: %s. Assigned to: %s. Last updated %s.",
		is.Number, is.Title, strings.ToLower(is.State), orWord(strings.Join(labels, ", "), "none"), orWord(strings.Join(who, ", "), "nobody"), is.UpdatedAt), nil
}

func orWord(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
