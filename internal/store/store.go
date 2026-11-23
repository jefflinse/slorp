// Package store persists Slorp's state in one SQLite file as JSON documents
// addressed by kind and id.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

// Open opens the database at path, creating it and its directory if needed.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS docs (
		kind TEXT NOT NULL, id TEXT NOT NULL, body TEXT NOT NULL,
		updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (kind, id))`)
	if err == nil {
		_, err = db.Exec(`CREATE INDEX IF NOT EXISTS docs_kind ON docs (kind)`)
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Put stores v as the document of that kind and id, replacing any other.
func (s *Store) Put(kind, id string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO docs (kind, id, body) VALUES (?, ?, ?)
		ON CONFLICT (kind, id) DO UPDATE SET body = excluded.body, updated_at = CURRENT_TIMESTAMP`, kind, id, string(b))
	return err
}

// PutAll stores each of docs as the document of that kind and its id,
// replacing any other, all of them or none.
func (s *Store) PutAll(kind string, docs map[string]any) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for id, v := range docs {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO docs (kind, id, body) VALUES (?, ?, ?)
			ON CONFLICT (kind, id) DO UPDATE SET body = excluded.body, updated_at = CURRENT_TIMESTAMP`, kind, id, string(b)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Get decodes the document into v and reports whether it exists.
func (s *Store) Get(kind, id string, v any) (bool, error) {
	var body string
	err := s.db.QueryRow(`SELECT body FROM docs WHERE kind = ? AND id = ?`, kind, id).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal([]byte(body), v)
}

// eachPage is how many documents Each reads before it calls back, and
// eachQuery how it reads them: docs_kind orders a kind's documents by rowid,
// so each page is a seek rather than a sort.
const (
	eachPage  = 128
	eachQuery = `SELECT rowid, id, body FROM docs WHERE kind = ? AND rowid > ? AND rowid <= ? ORDER BY rowid LIMIT ?`
)

// Each calls fn with the documents of a kind, oldest first, read a page at a
// time up to the highest rowid the kind had when Each was called. The store
// holds no connection while fn runs, so fn may use the store and take other
// locks whose holders are waiting on it. Each is bounded but not a snapshot:
// a document is seen as it is when its page is read, one deleted before then
// is not seen, and one added since is seen only if it reuses a rowid within
// the bound.
func (s *Store) Each(kind string, fn func(id string, body []byte)) error {
	type doc struct {
		id   string
		body []byte
	}
	var after, last int64
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(rowid), 0) FROM docs WHERE kind = ?`, kind).Scan(&last); err != nil {
		return err
	}
	for after < last {
		page := make([]doc, 0, eachPage)
		err := func() error {
			rows, err := s.db.Query(eachQuery, kind, after, last, eachPage)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var d doc
				var body string
				if err := rows.Scan(&after, &d.id, &body); err != nil {
					return err
				}
				d.body = []byte(body)
				page = append(page, d)
			}
			return rows.Err()
		}()
		if err != nil {
			return err
		}
		for _, d := range page {
			fn(d.id, d.body)
		}
		if len(page) < eachPage {
			return nil
		}
	}
	return nil
}

func (s *Store) Delete(kind, id string) error {
	_, err := s.db.Exec(`DELETE FROM docs WHERE kind = ? AND id = ?`, kind, id)
	return err
}
