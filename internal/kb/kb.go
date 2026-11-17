// Package kb is the user's knowledge base: the work efforts they have
// declared and how sessions are filed under them.
package kb

import (
	"errors"
	"path/filepath"
	"regexp"
	"slices"
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

// Persistence is where the knowledge base is kept between runs.
type Persistence interface {
	Get(kind, id string, v any) (bool, error)
	Put(kind, id string, v any) error
}

const docKind, docID = "kb", "main"

type file struct {
	Efforts []Effort `json:"efforts"`
	// Revision counts changes to what the model reads when it files a
	// session: the efforts, their descriptions and the routing hints.
	Revision        int `json:"revision"`
	ContextRevision int `json:"contextRevision"`
	// Seq numbers knowledge entries.
	Seq       int       `json:"seq"`
	Knowledge Knowledge `json:"knowledge"`
	// Assignments maps a session ID to the effort the user filed it under;
	// an empty effort means the user chose to leave it unsorted.
	Assignments map[string]string `json:"assignments"`
}

type Store struct {
	mu sync.Mutex
	p  Persistence
	f  file
}

// Open reads the knowledge base from p, starting empty if there is none.
func Open(p Persistence) (*Store, error) {
	s := &Store{p: p}
	if _, err := p.Get(docKind, docID, &s.f); err != nil {
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

// Revision changes whenever what the model reads to file a session changes.
func (s *Store) Revision() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Revision
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
	s.f.Revision++
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
	s.f.Knowledge.forget(id)
	s.f.Revision++
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

// repoKey prefixes a repository's entry among the assignments.
const repoKey = "repo:"

// AssignRepo files everything from a repository under an effort on the
// user's say-so. An empty effortID records that the user wants it left
// unsorted.
func (s *Store) AssignRepo(repo, effortID string) error {
	if strings.TrimSpace(repo) == "" {
		return errors.New("needs a repository")
	}
	return s.Assign(repoKey+strings.ToLower(repo), effortID)
}

// ResolveRepo reports where the user's own filing or a directory rule puts a
// repository. held lists the directories known to hold a checkout of it; the
// rule applies when exactly one effort has any of them. ok is false when
// neither applies.
func (s *Store) ResolveRepo(repo string, held []string) (effortID, via string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, found := s.f.Assignments[repoKey+strings.ToLower(repo)]; found && (id == "" || s.index(id) >= 0) {
		return id, ViaUser, true
	}
	for _, e := range s.f.Efforts {
		for _, d := range e.Dirs {
			if !slices.Contains(held, d) {
				continue
			}
			if effortID != "" && effortID != e.ID {
				return "", "", false
			}
			effortID = e.ID
		}
	}
	return effortID, ViaDir, effortID != ""
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
	return s.p.Put(docKind, docID, s.f)
}
