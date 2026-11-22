package hub

import (
	"encoding/json"
	"sync"
	"testing"
)

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
