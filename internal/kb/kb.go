// Package kb is the user's knowledge base: the work efforts they have
// declared and how sessions are filed under them. It is one JSON file meant
// to be readable and editable by hand.
package kb

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Effort is a piece of work the user tracks across sessions and tools.
type Effort struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	About string `json:"about"`
	// Dirs files every session working in or under one of these directories here.
	Dirs []string `json:"dirs"`
}

// How a session came to be filed under an effort.
const (
	ViaUser = "you"
	ViaDir  = "rule"
)

type file struct {
	Efforts []Effort `json:"efforts"`
	// Assignments maps a session ID to the effort the user filed it under;
	// an empty effort means the user chose to leave it unsorted.
	Assignments map[string]string `json:"assignments"`
}

type Store struct {
	mu       sync.Mutex
	path     string
	f        file
	revision int
}

// Open reads the knowledge base at path, starting empty if there is none.
func Open(path string) (*Store, error) {
	s := &Store{path: path, f: file{Assignments: map[string]string{}}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.f); err != nil {
		return nil, err
	}
	if s.f.Assignments == nil {
		s.f.Assignments = map[string]string{}
	}
	return s, nil
}

func (s *Store) Efforts() []Effort {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Effort, len(s.f.Efforts))
	for i, e := range s.f.Efforts {
		e.Dirs = append([]string{}, e.Dirs...)
		out[i] = e
	}
	return out
}

// Revision changes whenever the set of efforts or their descriptions change.
func (s *Store) Revision() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revision
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func (s *Store) Create(name, about string) (Effort, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	base := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if base == "" {
		return Effort{}, errors.New("an effort needs a name")
	}
	id := base
	for n := 2; s.index(id) >= 0; n++ {
		id = base + "-" + strconv.Itoa(n)
	}
	e := Effort{ID: id, Name: strings.TrimSpace(name), About: strings.TrimSpace(about), Dirs: []string{}}
	s.f.Efforts = append(s.f.Efforts, e)
	s.revision++
	return e, s.save()
}

// Delete removes an effort and every filing under it.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(id)
	if i < 0 {
		return errors.New("no such effort")
	}
	s.f.Efforts = append(s.f.Efforts[:i], s.f.Efforts[i+1:]...)
	for session, effort := range s.f.Assignments {
		if effort == id {
			delete(s.f.Assignments, session)
		}
	}
	s.revision++
	return s.save()
}

// Assign files a session under an effort on the user's say-so. An empty
// effortID records that the user wants it left unsorted.
func (s *Store) Assign(sessionID, effortID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if effortID != "" && s.index(effortID) < 0 {
		return errors.New("no such effort")
	}
	s.f.Assignments[sessionID] = effortID
	return s.save()
}

// AddDir makes dir belong to one effort, taking it from any other.
func (s *Store) AddDir(effortID, dir string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(effortID)
	dir = filepath.Clean(dir)
	if i < 0 || !filepath.IsAbs(dir) {
		return errors.New("needs an existing effort and an absolute directory")
	}
	for j := range s.f.Efforts {
		kept := s.f.Efforts[j].Dirs[:0]
		for _, d := range s.f.Efforts[j].Dirs {
			if d != dir {
				kept = append(kept, d)
			}
		}
		s.f.Efforts[j].Dirs = kept
	}
	s.f.Efforts[i].Dirs = append(s.f.Efforts[i].Dirs, dir)
	return s.save()
}

// Resolve reports where the user's own filing or a directory rule puts a
// session. ok is false when neither applies. The deepest matching directory
// wins.
func (s *Store) Resolve(sessionID, cwd string) (effortID, via string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, found := s.f.Assignments[sessionID]; found && (id == "" || s.index(id) >= 0) {
		return id, ViaUser, true
	}
	best := ""
	for _, e := range s.f.Efforts {
		for _, d := range e.Dirs {
			if (cwd == d || strings.HasPrefix(cwd, d+string(filepath.Separator))) && len(d) > len(best) {
				best, effortID = d, e.ID
			}
		}
	}
	return effortID, ViaDir, best != ""
}

func (s *Store) Has(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.index(id) >= 0
}

func (s *Store) index(id string) int {
	for i, e := range s.f.Efforts {
		if e.ID == id {
			return i
		}
	}
	return -1
}

func (s *Store) save() error {
	b, err := json.MarshalIndent(s.f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
