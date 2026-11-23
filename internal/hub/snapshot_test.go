package hub

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/jefflinse/slorp/internal/store"
	"github.com/jefflinse/slorp/internal/testdb"
)

func TestSettingsKeptShareNothingWithWhatTheyWereMadeFrom(t *testing.T) {
	h, k, st := newHub(t)
	unchanged := func(what, task, want string) {
		t.Helper()
		for name, s := range map[string]Settings{"kept": h.Settings(), "reopened": New(k, st).Settings()} {
			if s.Prompts.Tasks[task] != want || s.Integrations["github"] || s.Rewritten[ActFile] != 0 {
				t.Errorf("%s settings changed with %s: %+v", name, what, s)
			}
		}
	}

	var retained *Settings
	_, err := h.UpdateSettings(func(s *Settings) error {
		s.Prompts.Tasks = map[string]string{ActNote: "Say where it stands."}
		retained = s
		return nil
	})
	must(t, err)
	retained.Prompts.Tasks[ActNote] = "changed through the change"
	retained.Integrations["github"] = true
	retained.Rewritten[ActFile] = 9
	unchanged("the copy a change was made to", ActNote, "Say where it stands.")

	given := Settings{AnswerPermissions: true, Prompts: Prompts{Tasks: map[string]string{ActFile: "File it."}}}
	_, err = h.SetSettings(given)
	must(t, err)
	given.Prompts.Tasks[ActFile] = "changed by the caller"
	unchanged("what they were replaced with", ActFile, "File it.")
}

// unsavedSettings keeps the hub in a store, failing every write of the
// settings.
type unsavedSettings struct{ *store.Store }

func (c unsavedSettings) Put(kind, id string, v any) error {
	if kind == "setting" {
		return errors.New("disk full")
	}
	return c.Store.Put(kind, id, v)
}

func TestSettingsReplacedButNotSavedAreNotKept(t *testing.T) {
	st := testdb.Open(t)
	h := New(testdb.KB(t, st), unsavedSettings{st})
	if _, err := h.SetSettings(Settings{AnswerPermissions: false}); !errors.Is(err, ErrNotSaved) {
		t.Fatalf("replacing settings that cannot be saved = %v, want ErrNotSaved", err)
	}
	if !h.Settings().AnswerPermissions {
		t.Error("settings that could not be saved were kept")
	}
}

func TestStateKeepsTheSettingsItWasTakenWith(t *testing.T) {
	h, _, _ := newHub(t)
	must(t, h.SetIntegration("github", true))
	before, settings := h.State(), h.Settings()

	must(t, h.SetIntegration("github", false))
	must(t, h.SetIntegration("other", true))
	must(t, h.Rewrite(ActDescribe))

	for name, s := range map[string]Settings{"state": before.Settings, "settings": settings} {
		if !s.Integrations["github"] || s.Integrations["other"] || s.Rewritten[ActDescribe] != 0 {
			t.Errorf("earlier %s changed with later edits: integrations %v, rewritten %v", name, s.Integrations, s.Rewritten)
		}
	}
	if after := h.State().Settings; after.Integrations["github"] || !after.Integrations["other"] || after.Rewritten[ActDescribe] != 1 {
		t.Errorf("state after the edits: integrations %v, rewritten %v; want them", after.Integrations, after.Rewritten)
	}
}

func TestWrapupKeepsTheEffortsItWasTakenWith(t *testing.T) {
	h, k, _ := newHub(t)
	from, into := effort(t, k, "From"), effort(t, k, "Into")
	day := midnight().Format(dayLayout)
	write(t, h, day, "A day", []WrapupEffort{{ID: from, Name: "From"}})
	before, _ := h.Wrapup(day)

	must(t, h.MergeEfforts(from, into))

	if got := before.Efforts[0].ID; got != from {
		t.Errorf("an earlier wrap-up's effort became %q after a merge, want %q", got, from)
	}
	if after, _ := h.Wrapup(day); after.Efforts[0].ID != into {
		t.Errorf("the wrap-up after a merge has effort %q, want %q", after.Efforts[0].ID, into)
	}
}

func TestStateEncodesWhileSettingsChange(t *testing.T) {
	h, _, _ := newHub(t)
	must(t, h.SetIntegration("github", true))
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 200 {
			if err := h.SetIntegration("github", i%2 == 0); err != nil {
				t.Error(err)
			}
			if err := h.Rewrite(ActDescribe); err != nil {
				t.Error(err)
			}
		}
	})
	for range 200 {
		if _, err := json.Marshal(h.State()); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}
