package hub

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/integrations"
)

// tell has the model told global ahead of every task, and tasks in place
// of Slorp's own instructions for each, and returns the tasks left behind.
func tell(t *testing.T, h *Hub, global string, tasks map[string]string) []string {
	t.Helper()
	s := h.Settings()
	s.Prompts.Global, s.Prompts.Tasks = global, tasks
	behind, err := h.SetSettings(s)
	must(t, err)
	return behind
}

// rewriting sets whether what the model wrote is written again when its
// instructions change.
func rewriting(t *testing.T, h *Hub, mode string) {
	t.Helper()
	s := h.Settings()
	s.Prompts.Rewrite = mode
	if _, err := h.SetSettings(s); err != nil {
		t.Fatal(err)
	}
}

func TestPromptsPersistAndAreHandedOutAsACopy(t *testing.T) {
	h, k, st := newHub(t)
	if got := h.Settings(); got.Prompts.Global != "" || got.Prompts.Tasks == nil || len(got.Prompts.Tasks) != 0 || got.Prompts.Rewrite != RewriteAsk || got.Rewritten == nil {
		t.Fatalf("a new hub's settings = %+v, want no prompts, as an empty set, and rewriting left to be asked", got)
	}
	if _, err := h.SetSettings(Settings{}); err != nil {
		t.Fatal(err)
	}
	if got := h.Settings().Prompts.Rewrite; got != RewriteAsk {
		t.Errorf("settings that do not say whether to rewrite = %q, want it left to be asked", got)
	}
	rewriting(t, h, RewriteNever)
	tell(t, h, "Write in British English.", map[string]string{ActNote: "Say where the effort stands."})
	must(t, h.Rewrite(ActFile))

	got := h.Settings()
	got.Prompts.Tasks[ActNote] = "changed by the caller"
	got.Integrations["github"] = true
	got.Rewritten[ActFile] = 9
	if kept := h.Settings(); kept.Prompts.Tasks[ActNote] != "Say where the effort stands." || kept.Integrations["github"] || kept.Rewritten[ActFile] != 1 {
		t.Errorf("changing what Settings returned changed what the hub holds: %+v", kept)
	}
	if _, err := h.SetSettings(got); err != nil {
		t.Fatal(err)
	}
	if kept := h.Settings().Rewritten[ActFile]; kept != 1 {
		t.Errorf("settings counted %d rewrites of filings, want the count left to Rewrite", kept)
	}
	if kept := New(k, st).Settings(); kept.Prompts.Global != "Write in British English." || kept.Prompts.Tasks[ActNote] != "changed by the caller" || kept.Prompts.Rewrite != RewriteNever || kept.Rewritten[ActFile] != 1 {
		t.Errorf("after a restart: %+v", kept)
	}
}

// settled is a hub in which the model has nothing left to write: a session
// filed by its directory and one filed by a guess, each described and given
// a role, a note on the effort, and a brief of a pull request. It returns
// with it what is waiting on the model.
func settled(t *testing.T) (*Hub, func() string) {
	t.Helper()
	h, k, _ := newHub(t)
	a := effort(t, k, "A")
	must(t, k.AddDir(a, "/work/a"))
	r := role(t, k, "Reviewer", "")
	h.SetRoster(agent, []Session{idle("filed", "/work/a", "done"), idle("loose", "/w", "done")})
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	mine := pull("acme/widgets", 1, false, at)
	mine.Mine = true
	h.SetFeed("github", nil, []integrations.PullRequest{mine}, nil)
	h.SetDiscussion(h.Undiscussed(1)[0], talked(at, "why"), nil)
	waiting := func() string {
		for _, id := range []string{"filed", "loose"} {
			quieten(h, id)
		}
		return fmt.Sprintf("describe %d, file %d, role %d, note %d, brief %d", len(h.Undescribed()), len(h.Unrouted()), len(h.Unroled()), len(h.Unnoted()), len(h.Unbriefed()))
	}
	describe(h, "filed", KindReview)
	describe(h, "loose", KindReview)
	h.SetGuess("loose", h.Unrouted()[0].Key, "")
	for _, w := range h.Unroled() {
		h.SetRoleGuess(w.Session.ID, w.Key, r)
	}
	h.SetNote(a, h.Unnoted()[0].Key, Note{Line: "line"})
	h.SetBrief(h.Unbriefed()[0].Ref, PullBrief{Stands: "As first written."}, nil)
	if got := waiting(); got != nothing {
		t.Fatalf("setup: %s is waiting on the model", got)
	}
	return h, waiting
}

const nothing = "describe 0, file 0, role 0, note 0, brief 0"

func TestChangedInstructionsLeaveWritingBehindUntilItIsRewritten(t *testing.T) {
	tests := []struct {
		name, global string
		tasks        map[string]string
		behind, want string
	}{
		{"tasks that are only done when asked for", "", map[string]string{ActWrapup: "x", ActBackfill: "x"}, "", nothing},
		{"describing", "", map[string]string{ActDescribe: "x"}, "describe", "describe 2, file 0, role 0, note 0, brief 0"},
		{"choosing what to look up", "", map[string]string{PromptLookup: "x"}, "describe", "describe 2, file 0, role 0, note 0, brief 0"},
		{"filing", "", map[string]string{ActFile: "x"}, "file", "describe 0, file 1, role 0, note 0, brief 0"},
		{"guessing roles", "", map[string]string{ActRole: "x"}, "role", "describe 0, file 0, role 2, note 0, brief 0"},
		{"noting", "", map[string]string{ActNote: "x"}, "note", "describe 0, file 0, role 0, note 1, brief 0"},
		{"briefing", "", map[string]string{ActBrief: "x"}, "brief", "describe 0, file 0, role 0, note 0, brief 1"},
		{"filing and noting", "", map[string]string{ActFile: "x", ActNote: "x"}, "file note", "describe 0, file 1, role 0, note 1, brief 0"},
		{"describing and what follows from it", "", map[string]string{ActDescribe: "x", ActFile: "x", ActNote: "x"}, "describe", "describe 2, file 0, role 0, note 0, brief 0"},
		{"what goes ahead of every task", "x", nil, "describe brief", "describe 2, file 0, role 0, note 0, brief 1"},
	}
	for _, tt := range tests {
		for _, mode := range []string{RewriteAsk, RewriteNever} {
			t.Run(tt.name+", with rewriting set to "+mode, func(t *testing.T) {
				h, waiting := settled(t)
				rewriting(t, h, mode)
				behind := tell(t, h, tt.global, tt.tasks)
				if got := strings.Join(behind, " "); got != tt.behind {
					t.Errorf("left behind: %q, want %q", got, tt.behind)
				}
				if got := waiting(); got != nothing {
					t.Errorf("after the change: %s; want what was written to stand until it is asked for again", got)
				}
				if again := tell(t, h, tt.global, tt.tasks); len(again) != 0 {
					t.Errorf("saving the same again left %v behind", again)
				}
				if len(behind) == 0 {
					return
				}
				must(t, h.Rewrite(behind...))
				if got := waiting(); got != tt.want {
					t.Errorf("once rewritten: %s; want %s", got, tt.want)
				}
				if len(h.State().Items) != 2 {
					t.Error("what was written before is gone while it waits to be written again")
				}
			})
		}
		t.Run(tt.name+", with rewriting set to always", func(t *testing.T) {
			h, waiting := settled(t)
			rewriting(t, h, RewriteAlways)
			if behind := tell(t, h, tt.global, tt.tasks); len(behind) != 0 {
				t.Errorf("left behind: %v, want nothing, with it all written again", behind)
			}
			if got := waiting(); got != tt.want {
				t.Errorf("after the change: %s; want %s", got, tt.want)
			}
			before := h.Settings().Rewritten
			tell(t, h, tt.global, tt.tasks)
			if after := h.Settings().Rewritten; fmt.Sprint(after) != fmt.Sprint(before) {
				t.Errorf("saving the same again counted more rewrites: %v, was %v", after, before)
			}
		})
	}
}

func TestRewriteAll(t *testing.T) {
	h, waiting := settled(t)
	if got := waiting(); got != nothing {
		t.Fatal(got)
	}
	must(t, h.Rewrite())
	if got := waiting(); got != "describe 2, file 0, role 0, note 0, brief 1" {
		t.Fatalf("asked to rewrite it all: %s; want every session described again and the pull request briefed", got)
	}
	describe(h, "filed", KindReview)
	describe(h, "loose", KindReview)
	if got := waiting(); got != "describe 0, file 1, role 2, note 1, brief 1" {
		t.Errorf("once described again: %s; want the filing, roles and note to follow", got)
	}
}

func TestABriefWrittenBeforeARewriteWasAskedForDoesNotStand(t *testing.T) {
	h, _ := settled(t)
	must(t, h.Rewrite(ActNote))
	if p := listed(t, h, 1); len(h.Unbriefed()) != 0 || p.Behind {
		t.Fatalf("after another task was rewritten: %+v; want the brief to stand", p)
	}
	must(t, h.Rewrite(ActBrief))
	waiting := h.Unbriefed()
	if p := listed(t, h, 1); len(waiting) != 1 || !p.Behind || p.Brief == nil {
		t.Fatalf("asked for again: %d waiting, %+v; want the brief kept, marked behind, and written again", len(waiting), p)
	}
	must(t, h.Rewrite(ActBrief))
	h.SetBrief(waiting[0].Ref, PullBrief{Stands: "Asked for before the last rewrite."}, nil)
	if p := listed(t, h, 1); !p.Behind || p.Brief.Stands != "As first written." {
		t.Errorf("a brief asked for before the last rewrite stood as current: %+v", p.Brief)
	}
	h.SetBrief(h.Unbriefed()[0].Ref, PullBrief{Stands: "Written again."}, nil)
	if p := listed(t, h, 1); len(h.Unbriefed()) != 0 || p.Behind || p.Brief.Stands != "Written again." {
		t.Errorf("written again: %+v; want it current", p)
	}
}
