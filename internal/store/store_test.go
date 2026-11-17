package store_test

import (
	"path/filepath"
	"testing"

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
