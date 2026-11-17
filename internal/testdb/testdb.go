// Package testdb opens throwaway stores for tests.
package testdb

import (
	"path/filepath"
	"testing"

	"github.com/jefflinse/slorp/internal/kb"
	"github.com/jefflinse/slorp/internal/store"
)

// Open returns a store in a temporary directory, closed when the test ends.
func Open(t *testing.T) *store.Store {
	t.Helper()
	return OpenAt(t, filepath.Join(t.TempDir(), "slorp.db"))
}

// OpenAt opens the store at path, closed when the test ends.
func OpenAt(t *testing.T, path string) *store.Store {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// KB returns a knowledge base kept in st.
func KB(t *testing.T, st *store.Store) *kb.Store {
	t.Helper()
	k, err := kb.Open(st)
	if err != nil {
		t.Fatalf("open knowledge base: %v", err)
	}
	return k
}
