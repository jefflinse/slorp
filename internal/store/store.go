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

// eachPage is how many documents Each reads before it calls back.
const eachPage = 128

// Each calls fn with every document of a kind, oldest first. The store holds
// no connection while fn runs, so fn may use the store and take other locks
// whose holders are waiting on it.
func (s *Store) Each(kind string, fn func(id string, body []byte)) error {
	type doc struct {
		id   string
		body []byte
	}
	var after int64
	for {
		page := make([]doc, 0, eachPage)
		err := func() error {
			rows, err := s.db.Query(`SELECT rowid, id, body FROM docs WHERE kind = ? AND rowid > ? ORDER BY rowid LIMIT ?`, kind, after, eachPage)
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
}

func (s *Store) Delete(kind, id string) error {
	_, err := s.db.Exec(`DELETE FROM docs WHERE kind = ? AND id = ?`, kind, id)
	return err
}
