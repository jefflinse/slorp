package store_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/store"
	"github.com/jefflinse/slorp/internal/testdb"
)

type doc struct {
	N int `json:"n"`
}

func TestPutGetRoundTrip(t *testing.T) {
	st := testdb.Open(t)

	var got doc
	if found, err := st.Get("thing", "a", &got); err != nil || found {
		t.Fatalf("Get before Put = found %v, err %v; want not found", found, err)
	}
	if err := st.Put("thing", "a", doc{1}); err != nil {
		t.Fatal(err)
	}
	if found, err := st.Get("thing", "a", &got); err != nil || !found || got.N != 1 {
		t.Fatalf("Get = %+v, found %v, err %v; want {1}", got, found, err)
	}
	if err := st.Put("thing", "a", doc{2}); err != nil {
		t.Fatal(err)
	}
	if _, _ = st.Get("thing", "a", &got); got.N != 2 {
		t.Fatalf("Put did not replace: got %+v", got)
	}
	if found, _ := st.Get("other", "a", &got); found {
		t.Fatal("a document leaked across kinds")
	}
}

func TestEachIsOldestFirstWithinAKind(t *testing.T) {
	st := testdb.Open(t)
	for _, id := range []string{"c", "a", "b"} {
		if err := st.Put("thing", id, doc{}); err != nil {
			t.Fatal(err)
		}
	}
	st.Put("other", "z", doc{})
	st.Put("thing", "c", doc{9}) // replacing keeps its place

	var ids []string
	if err := st.Each("thing", func(id string, _ []byte) { ids = append(ids, id) }); err != nil {
		t.Fatal(err)
	}
	if got := len(ids); got != 3 || ids[0] != "c" || ids[1] != "a" || ids[2] != "b" {
		t.Fatalf("Each order = %v, want [c a b]", ids)
	}
}

func TestEachLetsGoOfTheStoreWhileCallingBack(t *testing.T) {
	st := testdb.Open(t)
	const n = 300
	for i := range n {
		if err := st.Put("thing", strconv.Itoa(i), doc{i}); err != nil {
			t.Fatal(err)
		}
	}
	type result struct {
		seen []int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		var r result
		var inside error
		err := st.Each("thing", func(id string, body []byte) {
			var d doc
			if found, err := st.Get("thing", id, &d); inside == nil && (err != nil || !found) {
				inside = fmt.Errorf("Get %s inside Each = %v, %v", id, found, err)
			}
			if err := st.Put("copy", id, d); inside == nil && err != nil {
				inside = err
			}
			r.seen = append(r.seen, d.N)
		})
		r.err = errors.Join(err, inside)
		done <- r
	}()
	select {
	case r := <-done:
		seen := r.seen
		if r.err != nil {
			t.Fatal(r.err)
		}
		if len(seen) != n {
			t.Fatalf("Each called back %d times, want %d", len(seen), n)
		}
		for i, got := range seen {
			if got != i {
				t.Fatalf("Each order at %d = %d, want oldest first", i, got)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading and writing the store from within Each never finished")
	}
}

func TestEachIsNotProlongedByWhatIsAdded(t *testing.T) {
	st := testdb.Open(t)
	const n = 200
	for i := range n {
		if err := st.Put("thing", strconv.Itoa(i), doc{i}); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan error, 1)
	seen := 0
	go func() {
		var inside error
		err := st.Each("thing", func(id string, _ []byte) {
			seen++
			if err := st.Put("thing", "new-"+id, doc{}); inside == nil {
				inside = err
			}
		})
		done <- errors.Join(err, inside)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
		if seen != n {
			t.Errorf("Each called back %d times while adding documents of its kind, want the %d there when it began", seen, n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Each never finished while adding documents of its kind")
	}
}

func TestEachDirectoryKeepsItsOwnDatabase(t *testing.T) {
	root := t.TempDir()
	names := []string{"example space", "example%20space", "what?", "what%3F", "hash#tag", "hash%23tag"}
	stores := map[string]*store.Store{}
	for _, name := range names {
		stores[name] = testdb.OpenAt(t, filepath.Join(root, name, "slorp.db"))
		if err := stores[name].Put("marker", "only", doc{len(stores)}); err != nil {
			t.Fatal(err)
		}
	}
	for i, name := range names {
		var got doc
		if found, err := stores[name].Get("marker", "only", &got); err != nil || !found || got.N != i+1 {
			t.Errorf("%q holds marker %+v, %v, %v; want its own, %d", name, got, found, err, i+1)
		}
		if _, err := os.Stat(filepath.Join(root, name, "slorp.db")); err != nil {
			t.Errorf("%q has no database of its own: %v", name, err)
		}
	}
}

func TestPutAllKeepsAllOrNone(t *testing.T) {
	st := testdb.Open(t)
	if err := st.PutAll(store.Doc{Kind: "thing", ID: "a", Value: doc{1}}, store.Doc{Kind: "other", ID: "b", Value: doc{2}}, store.Doc{Kind: "gone", ID: "c", Value: doc{3}}); err != nil {
		t.Fatal(err)
	}
	// The first two are done before the third fails.
	if err := st.PutAll(store.Doc{Kind: "gone", ID: "c", Delete: true}, store.Doc{Kind: "thing", ID: "a", Value: doc{4}}, store.Doc{Kind: "other", ID: "b", Value: func() {}}); err == nil {
		t.Fatal("storing what cannot be encoded succeeded")
	}
	for kind, want := range map[[2]string]int{{"thing", "a"}: 1, {"other", "b"}: 2, {"gone", "c"}: 3} {
		var got doc
		if found, err := st.Get(kind[0], kind[1], &got); err != nil || !found || got.N != want {
			t.Errorf("%s %s = %+v, %v, %v after a failed PutAll; want %d", kind[0], kind[1], got, found, err, want)
		}
	}
	if err := st.PutAll(store.Doc{Kind: "gone", ID: "c", Delete: true}); err != nil {
		t.Fatal(err)
	}
	if found, err := st.Get("gone", "c", &doc{}); err != nil || found {
		t.Errorf("a document PutAll deleted is found %v, %v", found, err)
	}
}

func TestARelativePathIsUnderTheWorkingDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	st, err := store.Open(filepath.Join("data dir%20", "slorp.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put("thing", "a", doc{7}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if _, err := os.Stat(filepath.Join("data dir%20", "slorp.db")); err != nil {
		t.Fatalf("no database where the relative path names: %v", err)
	}
	var got doc
	if found, err := testdb.OpenAt(t, filepath.Join("data dir%20", "slorp.db")).Get("thing", "a", &got); err != nil || !found || got.N != 7 {
		t.Errorf("reopened = %+v, %v, %v; want what was written", got, found, err)
	}
}

func TestDelete(t *testing.T) {
	st := testdb.Open(t)
	st.Put("thing", "a", doc{1})
	if err := st.Delete("thing", "a"); err != nil {
		t.Fatal(err)
	}
	var got doc
	if found, _ := st.Get("thing", "a", &got); found {
		t.Fatal("document survived Delete")
	}
}

func TestDocumentsSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "slorp.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st.Put("thing", "a", doc{7})
	st.Close()

	var got doc
	if found, err := testdb.OpenAt(t, path).Get("thing", "a", &got); err != nil || !found || got.N != 7 {
		t.Fatalf("after reopen: %+v, found %v, err %v", got, found, err)
	}
}
