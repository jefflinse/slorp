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
	uncoloured := false
	for i := range s.f.Efforts {
		if s.f.Efforts[i].Color == "" {
			s.f.Efforts[i].Color, uncoloured = s.spareColor(), true
		}
	}
	if uncoloured {
		return s, s.save()
	}
	return s, nil
}

// spareColor returns the colour the fewest efforts have, the first of them
// in Colors.
func (s *Store) spareColor() string {
	used := map[string]int{}
	for _, e := range s.f.Efforts {
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
	id := base
	for n := 2; s.index(id) >= 0; n++ {
		id = base + "-" + strconv.Itoa(n)
	}
	e := Effort{ID: id, Name: strings.TrimSpace(name), About: strings.TrimSpace(about), Dirs: []string{}, Color: s.spareColor()}
	s.f.Efforts = append(s.f.Efforts, e)
	s.f.Revision++
	return e, s.save()
}

// Edit gives an effort another name, description, colour or glyph. Its ID
// stays as it was made.
func (s *Store) Edit(id string, c Changes) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(id)
	if i < 0 {
		return errors.New("no such effort")
	}
	e := s.f.Efforts[i]
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
	if e.Name != s.f.Efforts[i].Name || e.About != s.f.Efforts[i].About {
		s.f.Revision++
	}
	s.f.Efforts[i] = e
	return s.save()
}

// Merge moves everything filed under one effort to another and removes the
// first: what the user filed, its directories, its open questions and what
// it watches.
func (s *Store) Merge(from, into string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, j := s.index(from), s.index(into)
	if i < 0 || j < 0 {
		return errors.New("no such effort")
	}
	if i == j {
		return errors.New("an effort cannot be merged into itself")
	}
	s.f.Efforts[j].Dirs = append(s.f.Efforts[j].Dirs, s.f.Efforts[i].Dirs...)
	for key, effort := range s.f.Assignments {
		if effort == from {
			s.f.Assignments[key] = into
		}
	}
	s.f.Knowledge.move(from, into)
	s.f.Efforts = slices.Delete(s.f.Efforts, i, i+1)
	s.f.Revision++
	return s.save()
}

// Shelve puts an effort aside, or brings it back.
func (s *Store) Shelve(id string, shelved bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(id)
	if i < 0 {
		return errors.New("no such effort")
	}
	if s.f.Efforts[i].Shelved == shelved {
		return nil
	}
	s.f.Efforts[i].Shelved = shelved
	s.f.Revision++
	return s.save()
}

// Active reports whether id is an effort the model may file under: one that
// exists and is not shelved.
func (s *Store) Active(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(id)
	return i >= 0 && !s.f.Efforts[i].Shelved
}

// Move puts an effort just ahead of the effort named by before, or last when
// before is empty. The order is the user's own and is not read by the model.
func (s *Store) Move(id, before string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(id)
	if i < 0 || (before != "" && s.index(before) < 0) {
		return errors.New("no such effort")
	}
	if id == before {
		return nil
	}
	e := s.f.Efforts[i]
	s.f.Efforts = slices.Delete(s.f.Efforts, i, i+1)
	at := len(s.f.Efforts)
	if before != "" {
		at = s.index(before)
	}
	s.f.Efforts = slices.Insert(s.f.Efforts, at, e)
	return s.save()
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
	delete(s.f.Assignments, pullKey+strings.ToLower(pull))
	return s.save()
}

// ResolvePull reports where a pull request, named as "owner/name#number", is
// filed: where the user filed it, or else where ResolveRepo puts its
// repository.
func (s *Store) ResolvePull(pull string, held []string) (effortID, via string, ok bool) {
	s.mu.Lock()
	id, found := s.f.Assignments[pullKey+strings.ToLower(pull)]
	found = found && (id == "" || s.index(id) >= 0)
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
	if id, found := s.f.Assignments[repoKey+strings.ToLower(repo)]; found && (id == "" || s.index(id) >= 0) {
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

// RemoveDir stops dir filing sessions under an effort.
func (s *Store) RemoveDir(effortID, dir string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(effortID)
	if i < 0 {
		return errors.New("no such effort")
	}
	at := slices.Index(s.f.Efforts[i].Dirs, filepath.Clean(dir))
	if at < 0 {
		return errors.New("no such directory in that effort")
	}
	s.f.Efforts[i].Dirs = slices.Delete(s.f.Efforts[i].Dirs, at, at+1)
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
