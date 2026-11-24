package kb

import (
	"cmp"
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Question is something the user has not decided yet.
type Question struct {
	ID     string    `json:"id"`
	Text   string    `json:"text"`
	Effort string    `json:"effort"`
	Origin string    `json:"origin"`
	At     time.Time `json:"at"`
}

// Fact is one plain sentence the user wants the model to know.
type Fact struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// Role is a kind of session the user tells apart, such as one that reviews
// pull requests on a loop.
type Role struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// About says what sessions in the role do; the model reads it to guess a
	// session's role.
	About string `json:"about"`
	// Attention is which of the role's sessions may ask for the user: every
	// one that needs them, only one that is stopped, or none.
	Attention string `json:"attention"`
}

// Workspace is a directory the user works in, by a name of their own.
type Workspace struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Dir  string `json:"dir"`
}

// What a role lets through to the user's attention.
const (
	AttentionAll     = "all"
	AttentionStopped = "stopped"
	AttentionNone    = "none"
)

// Knowledge is everything in the knowledge base besides the efforts.
type Knowledge struct {
	Questions []Question `json:"questions"`
	// Context is read by the model before it describes any session.
	Context []Fact `json:"context"`
	// Routing is read by the model when it files a session under an effort.
	Routing []Fact `json:"routing"`
	Roles   []Role `json:"roles"`
	// Workspaces are offered wherever a directory is asked for.
	Workspaces []Workspace `json:"workspaces"`
	// Profiles are offered when a session is started.
	Profiles []Profile `json:"profiles"`
	// Handoffs are offered when what a session said is sent to another.
	Handoffs []Handoff `json:"handoffs"`
}

// Sections of the knowledge base, as Add and Remove name them.
const (
	SectionQuestions  = "questions"
	SectionContext    = "context"
	SectionRouting    = "routing"
	SectionRoles      = "roles"
	SectionWorkspaces = "workspaces"
)

// Entry is what Add takes; each section reads the fields it has. A role's
// Type is its attention and its Text what it is about. A workspace's Dir is
// an absolute directory no other workspace has.
type Entry struct {
	Type   string `json:"type"`
	Name   string `json:"name"`
	Effort string `json:"effort"`
	Text   string `json:"text"`
	Origin string `json:"origin"`
	Dir    string `json:"dir"`
}

func (s *Store) Knowledge() Knowledge {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.f.Knowledge
	profiles := append([]Profile{}, k.Profiles...)
	for i, p := range profiles {
		if p.Temperature != nil {
			t := *p.Temperature
			profiles[i].Temperature = &t
		}
	}
	return Knowledge{
		Questions:  append([]Question{}, k.Questions...),
		Context:    append([]Fact{}, k.Context...),
		Routing:    append([]Fact{}, k.Routing...),
		Roles:      append([]Role{}, k.Roles...),
		Workspaces: append([]Workspace{}, k.Workspaces...),
		Profiles:   profiles,
		Handoffs:   append([]Handoff{}, k.Handoffs...),
	}
}

// RoleRevision changes whenever the roles the model chooses among change.
func (s *Store) RoleRevision() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.RoleRevision
}

func validAttention(a string) bool {
	return a == AttentionAll || a == AttentionStopped || a == AttentionNone
}

// File files a session under an effort and gives it a role on the user's
// say-so, both in one change or neither. An empty effortID or roleID leaves
// that as it is.
func (s *Store) File(sessionID, effortID, roleID string) error {
	if effortID == "" && roleID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.update(func(f *file) error {
		if effortID != "" {
			if f.index(effortID) < 0 {
				return errors.New("no such effort")
			}
			f.Assignments[sessionID] = effortID
		}
		if roleID != "" {
			if f.role(roleID) == nil {
				return errors.New("no such role")
			}
			if f.SessionRoles == nil {
				f.SessionRoles = map[string]string{}
			}
			f.SessionRoles[sessionID] = roleID
		}
		return nil
	})
}

// SetRole gives a session a role on the user's say-so. An empty roleID
// records that the user wants it to have none.
func (s *Store) SetRole(sessionID, roleID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.update(func(f *file) error {
		if roleID != "" && f.role(roleID) == nil {
			return errors.New("no such role")
		}
		if f.SessionRoles == nil {
			f.SessionRoles = map[string]string{}
		}
		f.SessionRoles[sessionID] = roleID
		return nil
	})
}

// RoleOf reports the role the user gave a session, empty for none. ok is
// false when the user has not said.
func (s *Store) RoleOf(sessionID string) (role Role, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, found := s.f.SessionRoles[sessionID]
	if r := s.f.role(id); r != nil {
		return *r, true
	}
	return Role{}, found
}

// Carry hands what the user said of one session, its effort and its role, to
// another the user has not said the same of, and forgets it for the first.
func (s *Store) Carry(from, to string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.f.said(from) {
		return nil
	}
	return s.update(func(f *file) error {
		f.carry(from, to)
		return nil
	})
}

// Said reports whether the user filed a session or gave it a role.
func (s *Store) Said(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.said(sessionID)
}

func (f *file) said(sessionID string) bool {
	_, filed := f.Assignments[sessionID]
	_, given := f.SessionRoles[sessionID]
	return filed || given
}

func (f *file) carry(from, to string) {
	for _, said := range []map[string]string{f.Assignments, f.SessionRoles} {
		v, ok := said[from]
		if !ok {
			continue
		}
		if _, has := said[to]; !has {
			said[to] = v
		}
		delete(said, from)
	}
}

// Staged is the knowledge base as a change to it would leave it, read
// before the change is saved.
type Staged struct{ f *file }

// Resolve is Store.Resolve as the change would leave it.
func (v Staged) Resolve(sessionID, cwd string) (effortID, via string, ok bool) {
	return v.f.resolve(sessionID, cwd)
}

// Active is Store.Active as the change would leave it.
func (v Staged) Active(id string) bool { return v.f.active(id) }

// Carrying carries what the user said of from to to, as Carry does, saved
// only by save: it is handed the kind, id and value of the knowledge base's
// document as the carry leaves it, and the knowledge base as it would then
// read, and saves that document with whatever else it writes. Nothing else
// changes the knowledge base meanwhile, and nothing changes unless save
// succeeds. save must not call the Store.
func (s *Store) Carrying(from, to string, save func(kind, id string, doc any, then Staged) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next, err := s.f.clone()
	if err != nil {
		return err
	}
	next.carry(from, to)
	if err := save(docKind, docID, next, Staged{&next}); err != nil {
		return err
	}
	s.f = next
	return nil
}

// HasRole reports whether a role with that ID exists.
func (s *Store) HasRole(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.role(id) != nil
}

// SetRoleAttention changes which of a role's sessions may ask for the user.
func (s *Store) SetRoleAttention(roleID, attention string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.update(func(f *file) error {
		r := f.role(roleID)
		if r == nil || !validAttention(attention) {
			return errors.New("needs an existing role and an attention of all, stopped or none")
		}
		r.Attention = attention
		return nil
	})
}

func (f *file) role(id string) *Role {
	for i := range f.Knowledge.Roles {
		if f.Knowledge.Roles[i].ID == id {
			return &f.Knowledge.Roles[i]
		}
	}
	return nil
}

// ContextRevision changes whenever the standing context changes.
func (s *Store) ContextRevision() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.ContextRevision
}

// Add appends an entry to a section.
func (s *Store) Add(section string, e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.update(func(f *file) error {
		text, name := strings.TrimSpace(e.Text), strings.TrimSpace(e.Name)
		if e.Effort != "" && f.index(e.Effort) < 0 {
			return errors.New("no such effort")
		}
		f.Seq++
		id := section[:1] + strconv.Itoa(f.Seq)
		k := &f.Knowledge
		switch {
		case section == SectionQuestions && text != "":
			k.Questions = append(k.Questions, Question{ID: id, Text: text, Effort: e.Effort, Origin: strings.TrimSpace(e.Origin), At: time.Now()})
		case section == SectionContext && text != "":
			k.Context = append(k.Context, Fact{ID: id, Text: text})
			f.ContextRevision++
		case section == SectionRouting && text != "":
			k.Routing = append(k.Routing, Fact{ID: id, Text: text})
			f.Revision++
		case section == SectionRoles && name != "" && (e.Type == "" || validAttention(e.Type)):
			k.Roles = append(k.Roles, Role{ID: id, Name: name, About: text, Attention: cmp.Or(e.Type, AttentionAll)})
			f.RoleRevision++
		case section == SectionWorkspaces && name != "" && filepath.IsAbs(e.Dir):
			dir := filepath.Clean(e.Dir)
			if slices.ContainsFunc(k.Workspaces, func(w Workspace) bool { return w.Dir == dir }) {
				return errors.New("a workspace already has that directory")
			}
			k.Workspaces = append(k.Workspaces, Workspace{ID: id, Name: name, Dir: dir})
		default:
			return errors.New("that entry is missing something, or the section does not exist")
		}
		return nil
	})
}

// Remove deletes one entry from a section.
func (s *Store) Remove(section, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.update(func(f *file) error {
		k := &f.Knowledge
		found := false
		switch section {
		case SectionQuestions:
			k.Questions, found = without(k.Questions, func(q Question) bool { return q.ID == id })
		case SectionContext:
			k.Context, found = without(k.Context, func(f Fact) bool { return f.ID == id })
			f.ContextRevision++
		case SectionRouting:
			k.Routing, found = without(k.Routing, func(f Fact) bool { return f.ID == id })
			f.Revision++
		case SectionRoles:
			k.Roles, found = without(k.Roles, func(r Role) bool { return r.ID == id })
			for i := range k.Profiles {
				if k.Profiles[i].Role == id {
					k.Profiles[i].Role = ""
				}
			}
			for session, role := range f.SessionRoles {
				if role == id {
					delete(f.SessionRoles, session)
				}
			}
			f.RoleRevision++
		case SectionWorkspaces:
			k.Workspaces, found = without(k.Workspaces, func(w Workspace) bool { return w.ID == id })
		}
		if !found {
			return errors.New("no such entry")
		}
		return nil
	})
}

// forget drops every reference to a deleted effort.
func (k *Knowledge) forget(effortID string) {
	for i := range k.Questions {
		if k.Questions[i].Effort == effortID {
			k.Questions[i].Effort = ""
		}
	}
	for i := range k.Profiles {
		if k.Profiles[i].Effort == effortID {
			k.Profiles[i].Effort = ""
		}
	}
}

// move files under one effort what was filed under another.
func (k *Knowledge) move(from, into string) {
	for i := range k.Questions {
		if k.Questions[i].Effort == from {
			k.Questions[i].Effort = into
		}
	}
	for i := range k.Profiles {
		if k.Profiles[i].Effort == from {
			k.Profiles[i].Effort = into
		}
	}
}

func without[T any](in []T, drop func(T) bool) (out []T, found bool) {
	out = make([]T, 0, len(in))
	for _, v := range in {
		if drop(v) {
			found = true
			continue
		}
		out = append(out, v)
	}
	return out, found
}
