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

// Each calls fn with every document of a kind, oldest first.
func (s *Store) Each(kind string, fn func(id string, body []byte)) error {
	rows, err := s.db.Query(`SELECT id, body FROM docs WHERE kind = ? ORDER BY rowid`, kind)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, body string
		if err := rows.Scan(&id, &body); err != nil {
			return err
		}
		fn(id, []byte(body))
	}
	return rows.Err()
}

func (s *Store) Delete(kind, id string) error {
	_, err := s.db.Exec(`DELETE FROM docs WHERE kind = ? AND id = ?`, kind, id)
	return err
}
