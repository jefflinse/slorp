package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var ctx = context.Background()

// recorder is a Runner that remembers what it was asked to run.
type recorder struct {
	dir  string
	argv []string
	out  string
	err  error
}

func (r *recorder) run(_ context.Context, dir string, argv ...string) ([]byte, error) {
	r.dir, r.argv = dir, argv
	return []byte(r.out), r.err
}

// workspace makes a directory holding the named git repositories.
func workspace(t *testing.T, repos ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range repos {
		if err := os.MkdirAll(filepath.Join(dir, name, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	os.MkdirAll(filepath.Join(dir, "not-a-repo"), 0o700)
	return dir
}

func tool(t *testing.T, in Integration, name string) Tool {
	t.Helper()
	for _, tl := range in.Tools {
		if tl.Name == name {
			return tl
		}
	}
	t.Fatalf("no tool %q", name)
	return Tool{}
}

func TestGitHubChecksTheCommand(t *testing.T) {
	tests := []struct {
		name string
		err  error
		ok   bool
		want string
	}{
		{"signed in", nil, true, "signed in"},
		{"not installed", ErrNotInstalled, false, "not installed"},
		{"not signed in", errors.New("exit status 1"), false, "not signed in"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &recorder{err: tt.err}
			ok, detail := GitHub(r.run).Check(ctx)
			if ok != tt.ok || !strings.Contains(detail, tt.want) {
				t.Errorf("Check = %v, %q", ok, detail)
			}
			if got := strings.Join(r.argv, " "); got != "gh auth status" {
				t.Errorf("ran %q", got)
			}
		})
	}
}

func TestGitHubScope(t *testing.T) {
	gh := GitHub((&recorder{}).run)
	repo := workspace(t)
	os.MkdirAll(filepath.Join(repo, ".git"), 0o700)

	if _, ok := gh.Scope(workspace(t)); ok {
		t.Error("a directory with no repository was offered lookups")
	}
	if _, ok := gh.Scope(""); ok {
		t.Error("a session with no directory was offered lookups")
	}
	if note, ok := gh.Scope(repo); !ok || strings.Contains(note, "name") {
		t.Errorf("a single repository: %q, %v; the model should not be asked to name it", note, ok)
	}
	note, ok := gh.Scope(workspace(t, "estoria", "estoria-contrib"))
	if !ok || !strings.Contains(note, "estoria, estoria-contrib") || strings.Contains(note, "not-a-repo") {
		t.Errorf("several repositories: %q, %v; want exactly the repositories listed", note, ok)
	}
}

func TestGitHubLookupRunsAFixedCommand(t *testing.T) {
	many := workspace(t, "estoria", "estoria-contrib")
	one := workspace(t)
	os.MkdirAll(filepath.Join(one, ".git"), 0o700)

	tests := []struct {
		name, tool, dir, args string
		wantDir               string
		wantArgv              string
		wantErr               string
	}{
		{"pull request in the only repository", "github_pr", one, `{"number":59}`, one, "gh pr view 59 --json " + prFields, ""},
		{"issue in a named repository", "github_issue", many, `{"number":7,"repository":"estoria"}`, filepath.Join(many, "estoria"), "gh issue view 7 --json " + issueFields, ""},
		{"several repositories and none named", "github_pr", many, `{"number":59}`, "", "", "estoria, estoria-contrib"},
		{"a repository that is not there", "github_pr", many, `{"number":59,"repository":"../../etc"}`, "", "", "name one of"},
		{"an absolute path as the repository", "github_pr", many, `{"number":59,"repository":"/tmp"}`, "", "", "name one of"},
		{"a directory that is not a repository", "github_pr", many, `{"number":59,"repository":"not-a-repo"}`, "", "", "name one of"},
		{"no repository at all", "github_pr", workspace(t), `{"number":59}`, "", "", "not working in a git repository"},
		{"no number", "github_pr", one, `{}`, "", "", "needs a number"},
		{"a flag where the number goes", "github_pr", one, `{"number":"--web"}`, "", "", "needs a number"},
		{"a negative number", "github_pr", one, `{"number":-1}`, "", "", "needs a number"},
		{"arguments that are not JSON", "github_pr", one, `59; rm -rf /`, "", "", "needs a number"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &recorder{out: `{"number":59,"title":"T","state":"OPEN"}`}
			_, err := tool(t, GitHub(r.run), tt.tool).Run(ctx, tt.dir, json.RawMessage(tt.args))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want one mentioning %q", err, tt.wantErr)
				}
				if r.argv != nil {
					t.Fatalf("a refused lookup still ran %v", r.argv)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(r.argv, " "); got != tt.wantArgv || r.dir != tt.wantDir {
				t.Errorf("ran %q in %q; want %q in %q", got, r.dir, tt.wantArgv, tt.wantDir)
			}
		})
	}
}

func TestGitHubLabels(t *testing.T) {
	gh := GitHub((&recorder{}).run)
	tests := []struct{ tool, args, want string }{
		{"github_pr", `{"number":59}`, "GitHub PR #59"},
		{"github_pr", `{"number":59,"repository":"estoria-contrib"}`, "GitHub PR estoria-contrib#59"},
		{"github_issue", `{"number":7,"repository":"."}`, "GitHub issue #7"},
		{"github_issue", `nonsense`, "GitHub issue (no number)"},
	}
	for _, tt := range tests {
		if got := tool(t, gh, tt.tool).Label(json.RawMessage(tt.args)); got != tt.want {
			t.Errorf("Label(%s) = %q, want %q", tt.args, got, tt.want)
		}
	}
}

func TestPullRequestIsCondensed(t *testing.T) {
	raw := `{"number":59,"title":"Add a Postgres snapshot store","state":"OPEN","isDraft":true,"mergeable":"MERGEABLE",
	  "reviewDecision":"CHANGES_REQUESTED","headRefName":"snap","baseRefName":"main","updatedAt":"2026-11-17T04:16:00Z",
	  "statusCheckRollup":[
	    {"name":"lint","status":"COMPLETED","conclusion":"SUCCESS"},
	    {"name":"unit","status":"COMPLETED","conclusion":"FAILURE"},
	    {"name":"integration","status":"IN_PROGRESS","conclusion":""},
	    {"name":"docs","status":"COMPLETED","conclusion":"SKIPPED"},
	    {"context":"ci/legacy","state":"ERROR"},
	    {"context":"ci/old","state":"SUCCESS"}]}`
	got, err := pullRequest([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`#59 "Add a Postgres snapshot store"`, "open, draft", "changes requested", "3 passed, 2 failed, 1 still running", "failing: unit, ci/legacy", "mergeable", "snap into main"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary lacks %q:\n%s", want, got)
		}
	}

	bare, _ := pullRequest([]byte(`{"number":1,"title":"x","state":"MERGED"}`))
	for _, want := range []string{"merged", "no checks", "none required or none yet"} {
		if !strings.Contains(bare, want) {
			t.Errorf("summary of a PR with nothing set lacks %q:\n%s", want, bare)
		}
	}
	if _, err := pullRequest([]byte("not json")); err == nil {
		t.Error("unreadable output was not reported")
	}
}

func TestIssueIsCondensed(t *testing.T) {
	got, err := issue([]byte(`{"number":7,"title":"Retention","state":"CLOSED","updatedAt":"2026-01-01T00:00:00Z","labels":[{"name":"bug"},{"name":"p1"}],"assignees":[{"login":"octo"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`#7 "Retention"`, "closed", "bug, p1", "octo"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary lacks %q:\n%s", want, got)
		}
	}
	bare, _ := issue([]byte(`{"number":8,"title":"x","state":"OPEN"}`))
	if !strings.Contains(bare, "Labels: none") || !strings.Contains(bare, "Assigned to: nobody") {
		t.Errorf("summary of a bare issue: %s", bare)
	}
}

func TestLookupReportsACommandFailure(t *testing.T) {
	dir := workspace(t)
	os.MkdirAll(filepath.Join(dir, ".git"), 0o700)
	r := &recorder{err: errors.New("no pull requests found")}
	if _, err := tool(t, GitHub(r.run), "github_pr").Run(ctx, dir, json.RawMessage(`{"number":999}`)); err == nil || !strings.Contains(err.Error(), "no pull requests found") {
		t.Errorf("err = %v", err)
	}
}

func TestExecReportsAMissingCommand(t *testing.T) {
	if _, err := Exec(ctx, "", "slorp-no-such-command-xyz"); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("err = %v, want ErrNotInstalled", err)
	}
	if out, err := Exec(ctx, "", "sh", "-c", "echo hello"); err != nil || strings.TrimSpace(string(out)) != "hello" {
		t.Errorf("Exec = %q, %v", out, err)
	}
	if _, err := Exec(ctx, "", "sh", "-c", "echo first line >&2; echo second >&2; exit 3"); err == nil || err.Error() != "first line" {
		t.Errorf("a failing command's error = %v, want its first line of stderr", err)
	}
}

func fake(id string, available bool, scoped bool, checks *int) Integration {
	return Integration{
		ID: id, Name: strings.ToUpper(id), About: "about " + id,
		Check: func(context.Context) (bool, string) {
			*checks++
			return available, "detail " + id
		},
		Scope: func(dir string) (string, bool) { return "note " + id + " in " + dir, scoped },
		Tools: []Tool{{Name: id + "_lookup", Description: "Look up " + id}},
	}
}

func TestManagerOffersOnlyWhatIsOnAvailableAndInScope(t *testing.T) {
	var checks int
	on := map[string]bool{"ready": true, "missing": true, "elsewhere": true}
	m := NewManager(func() map[string]bool { return on },
		fake("ready", true, true, &checks), fake("off", true, true, &checks), fake("missing", false, true, &checks), fake("elsewhere", true, false, &checks))

	tools, notes := m.Offer(ctx, "/w")
	if len(tools) != 1 || tools[0].Name != "ready_lookup" || len(notes) != 1 || notes[0] != "note ready in /w" {
		t.Errorf("Offer = %+v, %v", tools, notes)
	}

	on["off"] = true
	if tools, _ := m.Offer(ctx, "/w"); len(tools) != 2 {
		t.Errorf("after turning one on, Offer = %d tools, want 2", len(tools))
	}
}

func TestManagerStatus(t *testing.T) {
	var checks int
	m := NewManager(func() map[string]bool { return map[string]bool{"a": true} }, fake("a", true, true, &checks), fake("b", false, true, &checks))

	got := m.Status(ctx)
	if len(got) != 2 {
		t.Fatalf("Status = %d entries", len(got))
	}
	a, b := got[0], got[1]
	if a.ID != "a" || a.Name != "A" || !a.Enabled || !a.Available || a.Detail != "detail a" || len(a.Lookups) != 1 || a.Lookups[0] != "Look up a" {
		t.Errorf("a = %+v", a)
	}
	if b.Enabled || b.Available {
		t.Errorf("b = %+v", b)
	}
	if !m.Has("a") || m.Has("nope") {
		t.Error("Has is wrong")
	}

	before := checks
	m.Status(ctx)
	m.Offer(ctx, "/w")
	if checks != before {
		t.Errorf("availability was checked %d more times within a minute; each check runs a command", checks-before)
	}
	m.Recheck()
	m.Status(ctx)
	if checks != before+2 {
		t.Errorf("after Recheck, %d checks ran, want 2", checks-before)
	}
}
