package kb_test

import (
	"errors"
	"testing"

	"github.com/jefflinse/slorp/internal/kb"
	"github.com/jefflinse/slorp/internal/store"
	"github.com/jefflinse/slorp/internal/testdb"
)

// failing keeps the knowledge base in a store, but fails every write while
// fail is set.
type failing struct {
	*store.Store
	fail bool
}

func (p *failing) Put(kind, id string, v any) error {
	if p.fail {
		return errors.New("disk full")
	}
	return p.Store.Put(kind, id, v)
}

func temperature(k *kb.Store, id string) float64 {
	for _, p := range k.Knowledge().Profiles {
		if p.ID == id && p.Temperature != nil {
			return *p.Temperature
		}
	}
	return -1
}

func TestARejectedProfileEditChangesNothing(t *testing.T) {
	st := testdb.Open(t)
	k := testdb.KB(t, st)
	warm := 0.5
	id, err := k.AddProfile(kb.Profile{Name: "Local", Engine: kb.EngineLocal, Temperature: &warm})
	must(t, err)
	before := k.Knowledge()

	err = k.EditProfile(id, func(p *kb.Profile) error {
		*p.Temperature = 7
		return nil
	})
	if err == nil || errors.Is(err, kb.ErrNotSaved) {
		t.Fatalf("a temperature of 7 = %v, want it rejected as invalid", err)
	}
	if got := temperature(k, id); got != 0.5 {
		t.Errorf("temperature after a rejected edit = %v, want 0.5", got)
	}
	if got := *before.Profiles[0].Temperature; got != 0.5 {
		t.Errorf("an earlier snapshot's temperature became %v, want 0.5", got)
	}
	if got := temperature(testdb.KB(t, st), id); got != 0.5 {
		t.Errorf("reopened temperature = %v, want 0.5", got)
	}
}

func TestAKnowledgeSnapshotSharesNothingWithTheStore(t *testing.T) {
	k := open(t)
	warm := 0.5
	id, err := k.AddProfile(kb.Profile{Name: "Local", Engine: kb.EngineLocal, Temperature: &warm})
	must(t, err)
	warm = 1.5
	snapshot := k.Knowledge()
	*snapshot.Profiles[0].Temperature = 1.9
	if got := temperature(k, id); got != 0.5 {
		t.Errorf("temperature = %v after changing what was given and what was returned, want 0.5", got)
	}
}

func TestAFailedWriteChangesNothing(t *testing.T) {
	st := testdb.Open(t)
	p := &failing{Store: st}
	k, err := kb.Open(p)
	must(t, err)
	doomed := create(t, k, "Doomed")
	must(t, k.Assign("session", doomed))
	revision := k.Revision()

	p.fail = true
	if err := k.Delete(doomed); !errors.Is(err, kb.ErrNotSaved) {
		t.Fatalf("deleting with the store failing = %v, want ErrNotSaved", err)
	}
	if _, err := k.Create("Never", ""); !errors.Is(err, kb.ErrNotSaved) {
		t.Fatalf("creating with the store failing = %v, want ErrNotSaved", err)
	}
	if _, err := k.AddProfile(kb.Profile{Name: "Never"}); !errors.Is(err, kb.ErrNotSaved) {
		t.Fatalf("adding a profile with the store failing = %v, want ErrNotSaved", err)
	}
	check := func(name string, k *kb.Store) {
		t.Helper()
		if !k.Has(doomed) || k.Has("never") || len(k.Knowledge().Profiles) != 0 {
			t.Errorf("%s: efforts %+v, profiles %+v; want only what was saved", name, k.Efforts(), k.Knowledge().Profiles)
		}
		if effort, _, ok := k.Resolve("session", "/"); !ok || effort != doomed {
			t.Errorf("%s: session filed under %q, want %q", name, effort, doomed)
		}
	}
	check("after the failed writes", k)
	if got := k.Revision(); got != revision {
		t.Errorf("revision after the failed writes = %d, want %d", got, revision)
	}

	// The next write that succeeds keeps only what it changes.
	p.fail = false
	create(t, k, "Kept")
	check("after a later write", k)
	reopened := testdb.KB(t, st)
	check("reopened", reopened)
	if !reopened.Has("kept") {
		t.Error("reopened without the effort written after the failures")
	}
}
