package hub

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/integrations"
)

// tell has the model told global ahead of every task, and tasks in place
// of Slorp's own instructions for each.
func tell(h *Hub, global string, tasks map[string]string) {
	s := h.Settings()
	s.Prompts = Prompts{Global: global, Tasks: tasks}
	h.SetSettings(s)
}

func TestPromptsPersistAndAreHandedOutAsACopy(t *testing.T) {
	h, k, st := newHub(t)
	if got := h.Settings().Prompts; got.Global != "" || got.Tasks == nil || len(got.Tasks) != 0 {
		t.Fatalf("a new hub's prompts = %+v, want none, as an empty set", got)
	}
	tell(h, "Write in British English.", map[string]string{ActNote: "Say where the effort stands."})

	got := h.Settings()
	got.Prompts.Tasks[ActNote] = "changed by the caller"
	got.Integrations["github"] = true
	if kept := h.Settings(); kept.Prompts.Tasks[ActNote] != "Say where the effort stands." || kept.Integrations["github"] {
		t.Errorf("changing what Settings returned changed what the hub holds: %+v", kept)
	}
	if kept := New(k, st).Settings().Prompts; kept.Global != "Write in British English." || kept.Tasks[ActNote] != "Say where the effort stands." {
		t.Errorf("after a restart: %+v", kept)
	}
}

func TestChangedInstructionsHaveTheModelWriteAgain(t *testing.T) {
	// settled is a hub in which the model has nothing left to write: a
	// session filed by its directory and one filed by a guess, each
	// described and given a role, and a note on the effort.
	settled := func(t *testing.T) (*Hub, func() string) {
		h, k, _ := newHub(t)
		a := effort(t, k, "A")
		must(t, k.AddDir(a, "/work/a"))
		r := role(t, k, "Reviewer", "")
		h.SetRoster(agent, []Session{idle("filed", "/work/a", "done"), idle("loose", "/w", "done")})
		waiting := func() string {
			for _, id := range []string{"filed", "loose"} {
				quieten(h, id)
			}
			return fmt.Sprintf("describe %d, file %d, role %d, note %d", len(h.Undescribed()), len(h.Unrouted()), len(h.Unroled()), len(h.Unnoted()))
		}
		describe(h, "filed", KindReview)
		describe(h, "loose", KindReview)
		h.SetGuess("loose", h.Unrouted()[0].Key, "")
		for _, w := range h.Unroled() {
			h.SetRoleGuess(w.Session.ID, w.Key, r)
		}
		h.SetNote(a, h.Unnoted()[0].Key, Note{Line: "line"})
		if got := waiting(); got != "describe 0, file 0, role 0, note 0" {
			t.Fatalf("setup: %s is waiting on the model", got)
		}
		return h, waiting
	}

	tests := []struct {
		name, global string
		tasks        map[string]string
		want         string
	}{
		{"a task that is only done when asked for", "", map[string]string{ActWrapup: "x", ActBackfill: "x"}, "describe 0, file 0, role 0, note 0"},
		{"describing", "", map[string]string{ActDescribe: "x"}, "describe 2, file 0, role 0, note 0"},
		{"choosing what to look up", "", map[string]string{PromptLookup: "x"}, "describe 2, file 0, role 0, note 0"},
		{"filing", "", map[string]string{ActFile: "x"}, "describe 0, file 1, role 0, note 0"},
		{"guessing roles", "", map[string]string{ActRole: "x"}, "describe 0, file 0, role 2, note 0"},
		{"noting", "", map[string]string{ActNote: "x"}, "describe 0, file 0, role 0, note 1"},
		{"what goes ahead of every task", "x", nil, "describe 2, file 0, role 0, note 0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, waiting := settled(t)
			tell(h, tt.global, tt.tasks)
			if got := waiting(); got != tt.want {
				t.Errorf("after the change: %s; want %s", got, tt.want)
			}
			if len(h.State().Items) != 2 {
				t.Error("what was written before is gone while it waits to be written again")
			}
			tell(h, "", nil)
			if got := waiting(); got != "describe 0, file 0, role 0, note 0" {
				t.Errorf("back to Slorp's own instructions: %s; want what was written under them to stand", got)
			}
		})
	}

	t.Run("saving the same again", func(t *testing.T) {
		h, waiting := settled(t)
		tell(h, "x", map[string]string{ActFile: "y"})
		if got := waiting(); got != "describe 2, file 0, role 0, note 0" {
			t.Fatalf("after the change: %s", got)
		}
		describe(h, "filed", KindReview)
		describe(h, "loose", KindReview)
		before := waiting()
		tell(h, "x", map[string]string{ActFile: "y"})
		if got := waiting(); got != before || !strings.HasPrefix(got, "describe 0,") {
			t.Errorf("after saving what was already there: %s, was %s; want nothing more to write", got, before)
		}
	})
}

func TestChangedInstructionsHaveABriefWrittenAgain(t *testing.T) {
	h, _, _ := newHub(t)
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	mine := pull("acme/widgets", 1, false, at)
	mine.Mine = true
	h.SetFeed("github", nil, []integrations.PullRequest{mine}, nil)
	h.SetDiscussion(h.Undiscussed(1)[0], talked(at, "why"), nil)
	first := h.Unbriefed()[0]
	h.SetBrief(first.Ref, PullBrief{Stands: "Under Slorp's own."}, nil)
	if len(h.Unbriefed()) != 0 || listed(t, h, 1).Behind {
		t.Fatal("setup: the pull request should be written about")
	}

	tell(h, "", map[string]string{ActNote: "x"})
	if len(h.Unbriefed()) != 0 {
		t.Error("a change to another task's instructions has the brief written again")
	}
	for _, change := range []func(){
		func() { tell(h, "", map[string]string{ActBrief: "x"}) },
		func() { tell(h, "Write in British English.", map[string]string{ActBrief: "x"}) },
	} {
		change()
		waiting := h.Unbriefed()
		if p := listed(t, h, 1); len(waiting) != 1 || !p.Behind || p.Brief == nil {
			t.Fatalf("after the change: %d waiting, %+v; want the brief kept, marked behind, and written again", len(waiting), p)
		}
		h.SetBrief(first.Ref, PullBrief{Stands: "Written under what the model was told before."}, nil)
		if p := listed(t, h, 1); !p.Behind || strings.Contains(p.Brief.Stands, "before") {
			t.Errorf("a brief written under the instructions before stood as current: %+v", p.Brief)
		}
		first = waiting[0]
		h.SetBrief(first.Ref, PullBrief{Stands: "Under the user's."}, nil)
		if p := listed(t, h, 1); len(h.Unbriefed()) != 0 || p.Behind || p.Brief.Stands != "Under the user's." {
			t.Errorf("written again: %+v; want it current", p)
		}
	}
}
