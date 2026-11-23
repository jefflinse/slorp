package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestEachSeeksEachPageWithoutSorting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slorp.db")
	// A database made before docs_kind existed is given it when opened.
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE docs (kind TEXT NOT NULL, id TEXT NOT NULL, body TEXT NOT NULL,
		updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP, PRIMARY KEY (kind, id))`); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	rows, err := s.db.Query(`EXPLAIN QUERY PLAN `+eachQuery, "thing", 0, 1000, eachPage)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(plan, "; ")
	if !strings.Contains(got, "docs_kind (kind=? AND rowid>? AND rowid<?)") || strings.Contains(got, "TEMP B-TREE") {
		t.Errorf("a page is read by %q, want a rowid range seek on docs_kind with no sort", got)
	}
}
