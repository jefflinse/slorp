// Package kb is the user's knowledge base: the work efforts they have
// declared and how sessions are filed under them.
package kb

import (
	"encoding/json"
	"errors"
	"fmt"
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
	// Shelved marks an effort the user has put aside: it keeps everything
	// filed under it, and the model is no longer offered it.
	Shelved bool `json:"shelved,omitempty"`
	// Color is one of Colors and Glyph a character or two, which the page
	// marks the effort with. An effort is given the colour the fewest
	// others have until the user chooses one; Glyph is empty until they do.
	Color string `json:"color,omitempty"`
	Glyph string `json:"glyph,omitempty"`
}

// Colors are the colours an effort can be marked with.
var Colors = []string{"red", "orange", "olive", "green", "teal", "blue", "violet", "pink"}

// Changes are what Edit gives an effort; whatever is nil is left alone.
type Changes struct {
	Name, About, Color, Glyph *string
}

// How a session, or something from a repository, came to be filed under an
// effort: by the user, by the user's filing of its repository, or by a
// directory rule.
const (
	ViaUser = "you"
	ViaRepo = "repo"
	ViaDir  = "rule"
)

// Persistence is where the knowledge base is kept between runs.
type Persistence interface {
	Get(kind, id string, v any) (bool, error)
	Put(kind, id string, v any) error
}

const docKind, docID = "kb", "main"

// ErrNotSaved is wrapped by the error of a change that could not be saved,
// which is then not made.
var ErrNotSaved = errors.New("the knowledge base could not be saved")

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
	// SessionRoles maps a session ID to the role the user gave it; an empty
	// role means the user chose to give it none.
	SessionRoles map[string]string `json:"sessionRoles"`
	RoleRevision int               `json:"roleRevision"`
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
	// Efforts made before they had colours are given one each.
	if !slices.ContainsFunc(s.f.Efforts, func(e Effort) bool { return e.Color == "" }) {
		return s, nil
	}
	return s, s.update(func(f *file) error {
		for i := range f.Efforts {
			if f.Efforts[i].Color == "" {
				f.Efforts[i].Color = f.spareColor()
			}
		}
		return nil
	})
}

// spareColor returns the colour the fewest efforts have, the first of them
// in Colors.
func (f *file) spareColor() string {
	used := map[string]int{}
	for _, e := range f.Efforts {
		used[e.Color]++
	}
	return slices.MinFunc(Colors, func(a, b string) int { return used[a] - used[b] })
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
	var e Effort
	err := s.update(func(f *file) error {
		id := base
		for n := 2; f.index(id) >= 0; n++ {
			id = base + "-" + strconv.Itoa(n)
		}
		e = Effort{ID: id, Name: strings.TrimSpace(name), About: strings.TrimSpace(about), Dirs: []string{}, Color: f.spareColor()}
		f.Efforts = append(f.Efforts, e)
		f.Revision++
		return nil
	})
	if err != nil {
		return Effort{}, err
	}
	return e, nil
}

// Edit gives an effort another name, description, colour or glyph. Its ID
// stays as it was made.
func (s *Store) Edit(id string, c Changes) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.update(func(f *file) error {
		i := f.index(id)
		if i < 0 {
			return errors.New("no such effort")
		}
		e := f.Efforts[i]
		if c.Name != nil {
			if e.Name = strings.TrimSpace(*c.Name); e.Name == "" {
				return errors.New("an effort needs a name")
			}
		}
		if c.About != nil {
			e.About = strings.TrimSpace(*c.About)
		}
		if c.Color != nil {
			if e.Color = strings.TrimSpace(*c.Color); e.Color != "" && !slices.Contains(Colors, e.Color) {
				return errors.New("no such colour")
			}
		}
		if c.Glyph != nil {
			if e.Glyph = strings.TrimSpace(*c.Glyph); len([]rune(e.Glyph)) > 8 {
				return errors.New("a glyph is a character or two")
			}
		}
		if e.Name != f.Efforts[i].Name || e.About != f.Efforts[i].About {
			f.Revision++
		}
		f.Efforts[i] = e
		return nil
	})
}

// Merge moves everything filed under one effort to another and removes the
// first: what the user filed, its directories and its open questions.
func (s *Store) Merge(from, into string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.update(func(f *file) error {
		i, j := f.index(from), f.index(into)
		if i < 0 || j < 0 {
			return errors.New("no such effort")
		}
		if i == j {
			return errors.New("an effort cannot be merged into itself")
		}
		f.Efforts[j].Dirs = append(f.Efforts[j].Dirs, f.Efforts[i].Dirs...)
		for key, effort := range f.Assignments {
			if effort == from {
				f.Assignments[key] = into
			}
		}
		f.Knowledge.move(from, into)
		f.Efforts = slices.Delete(f.Efforts, i, i+1)
		f.Revision++
		return nil
	})
}

// Shelve puts an effort aside, or brings it back.
func (s *Store) Shelve(id string, shelved bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.f.index(id)
	if i < 0 {
		return errors.New("no such effort")
	}
	if s.f.Efforts[i].Shelved == shelved {
		return nil
	}
	return s.update(func(f *file) error {
		f.Efforts[i].Shelved = shelved
		f.Revision++
		return nil
	})
}

// Active reports whether id is an effort the model may file under: one that
// exists and is not shelved.
func (s *Store) Active(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.f.index(id)
	return i >= 0 && !s.f.Efforts[i].Shelved
}

// Move puts an effort just ahead of the effort named by before, or last when
// before is empty. The order is the user's own and is not read by the model.
func (s *Store) Move(id, before string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.f.index(id)
	if i < 0 || (before != "" && s.f.index(before) < 0) {
		return errors.New("no such effort")
	}
	if id == before {
		return nil
	}
	return s.update(func(f *file) error {
		e := f.Efforts[i]
		f.Efforts = slices.Delete(f.Efforts, i, i+1)
		at := len(f.Efforts)
		if before != "" {
			at = f.index(before)
		}
		f.Efforts = slices.Insert(f.Efforts, at, e)
		return nil
	})
}

// Delete removes an effort and every filing under it.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.update(func(f *file) error {
		i := f.index(id)
		if i < 0 {
			return errors.New("no such effort")
		}
		f.Efforts = append(f.Efforts[:i], f.Efforts[i+1:]...)
		for session, effort := range f.Assignments {
			if effort == id {
				delete(f.Assignments, session)
			}
		}
		f.Knowledge.forget(id)
		f.Revision++
		return nil
	})
}

// Assign files a session under an effort on the user's say-so. An empty
// effortID records that the user wants it left unsorted.
func (s *Store) Assign(sessionID, effortID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.update(func(f *file) error {
		if effortID != "" && f.index(effortID) < 0 {
			return errors.New("no such effort")
		}
		f.Assignments[sessionID] = effortID
		return nil
	})
}

// repoKey prefixes a repository's entry among the assignments.
const repoKey = "repo:"

// AssignRepo files everything from a repository under an effort on the
// user's say-so, but for the pull requests filed on their own. An empty
// effortID records that the user wants it left unsorted.
func (s *Store) AssignRepo(repo, effortID string) error {
	if strings.TrimSpace(repo) == "" {
		return errors.New("needs a repository")
	}
	return s.Assign(repoKey+strings.ToLower(repo), effortID)
}

// pullKey prefixes a pull request's entry among the assignments.
const pullKey = "pull:"

// AssignPull files one pull request, named as "owner/name#number", under an
// effort on the user's say-so, wherever its repository is filed. An empty
// effortID records that the user wants it left unsorted.
func (s *Store) AssignPull(pull, effortID string) error {
	repo, number, _ := strings.Cut(pull, "#")
	if n, err := strconv.Atoi(number); err != nil || n <= 0 || strings.TrimSpace(repo) == "" {
		return errors.New("needs a pull request")
	}
	return s.Assign(pullKey+strings.ToLower(pull), effortID)
}

// UnassignPull forgets the user's filing of a pull request, which is then
// filed with its repository.
func (s *Store) UnassignPull(pull string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.update(func(f *file) error {
		delete(f.Assignments, pullKey+strings.ToLower(pull))
		return nil
	})
}

// ResolvePull reports where a pull request, named as "owner/name#number", is
// filed: where the user filed it, or else where ResolveRepo puts its
// repository.
func (s *Store) ResolvePull(pull string, held []string) (effortID, via string, ok bool) {
	s.mu.Lock()
	id, found := s.f.Assignments[pullKey+strings.ToLower(pull)]
	found = found && (id == "" || s.f.index(id) >= 0)
	s.mu.Unlock()
	if found {
		return id, ViaUser, true
	}
	repo, _, _ := strings.Cut(pull, "#")
	return s.ResolveRepo(repo, held)
}

// ResolveRepo reports where the user's own filing or a directory rule puts
// what comes from a repository. held lists the directories known to hold a
// checkout of it; the rule applies when exactly one effort has any of them.
// ok is false when neither applies.
func (s *Store) ResolveRepo(repo string, held []string) (effortID, via string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, found := s.f.Assignments[repoKey+strings.ToLower(repo)]; found && (id == "" || s.f.index(id) >= 0) {
		return id, ViaRepo, true
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
	return s.update(func(f *file) error {
		i := f.index(effortID)
		dir = filepath.Clean(dir)
		if i < 0 || !filepath.IsAbs(dir) {
			return errors.New("needs an existing effort and an absolute directory")
		}
		for j := range f.Efforts {
			kept := f.Efforts[j].Dirs[:0]
			for _, d := range f.Efforts[j].Dirs {
				if d != dir {
					kept = append(kept, d)
				}
			}
			f.Efforts[j].Dirs = kept
		}
		f.Efforts[i].Dirs = append(f.Efforts[i].Dirs, dir)
		return nil
	})
}

// RemoveDir stops dir filing sessions under an effort.
func (s *Store) RemoveDir(effortID, dir string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.update(func(f *file) error {
		i := f.index(effortID)
		if i < 0 {
			return errors.New("no such effort")
		}
		at := slices.Index(f.Efforts[i].Dirs, filepath.Clean(dir))
		if at < 0 {
			return errors.New("no such directory in that effort")
		}
		f.Efforts[i].Dirs = slices.Delete(f.Efforts[i].Dirs, at, at+1)
		return nil
	})
}

// Resolve reports where the user's own filing or a directory rule puts a
// session. ok is false when neither applies. The deepest matching directory
// wins.
func (s *Store) Resolve(sessionID, cwd string) (effortID, via string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, found := s.f.Assignments[sessionID]; found && (id == "" || s.f.index(id) >= 0) {
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
	return s.f.index(id) >= 0
}

func (f *file) index(id string) int {
	for i, e := range f.Efforts {
		if e.ID == id {
			return i
		}
	}
	return -1
}

// update has change edit a copy of the knowledge base, and keeps the copy
// only once it is saved, so that a change that fails or is not saved is not
// made at all. The caller holds s.mu.
func (s *Store) update(change func(*file) error) error {
	next, err := s.f.clone()
	if err != nil {
		return err
	}
	if err := change(&next); err != nil {
		return err
	}
	if err := s.p.Put(docKind, docID, next); err != nil {
		return fmt.Errorf("%w: %w", ErrNotSaved, err)
	}
	s.f = next
	return nil
}

// clone returns a copy of f that shares nothing with it, read back as it
// is saved.
func (f *file) clone() (file, error) {
	var out file
	b, err := json.Marshal(f)
	if err != nil {
		return out, err
	}
	return out, json.Unmarshal(b, &out)
}
