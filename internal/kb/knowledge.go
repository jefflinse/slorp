package kb

import (
	"cmp"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Watch is a Slack person, channel or DM worth checking for an effort.
type Watch struct {
	ID string `json:"id"`
	// Type is "person", "channel" or "dm".
	Type    string   `json:"type"`
	Name    string   `json:"name"`
	Why     string   `json:"why"`
	Efforts []string `json:"efforts"`
}

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

// What a role lets through to the user's attention.
const (
	AttentionAll     = "all"
	AttentionStopped = "stopped"
	AttentionNone    = "none"
)

// Knowledge is everything in the knowledge base besides the efforts.
type Knowledge struct {
	Watch     []Watch    `json:"watch"`
	Questions []Question `json:"questions"`
	// Context is read by the model before it describes any session.
	Context []Fact `json:"context"`
	// Routing is read by the model when it files a session under an effort.
	Routing []Fact `json:"routing"`
	Roles   []Role `json:"roles"`
}

// Sections of the knowledge base, as Add and Remove name them.
const (
	SectionWatch     = "watch"
	SectionQuestions = "questions"
	SectionContext   = "context"
	SectionRouting   = "routing"
	SectionRoles     = "roles"
)

// Entry is what Add takes; each section reads the fields it has. A role's
// Type is its attention and its Text what it is about.
type Entry struct {
	Type   string `json:"type"`
	Name   string `json:"name"`
	Why    string `json:"why"`
	Effort string `json:"effort"`
	Text   string `json:"text"`
	Origin string `json:"origin"`
}

func (s *Store) Knowledge() Knowledge {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.f.Knowledge
	return Knowledge{
		Watch:     append([]Watch{}, k.Watch...),
		Questions: append([]Question{}, k.Questions...),
		Context:   append([]Fact{}, k.Context...),
		Routing:   append([]Fact{}, k.Routing...),
		Roles:     append([]Role{}, k.Roles...),
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

// SetRole gives a session a role on the user's say-so. An empty roleID
// records that the user wants it to have none.
func (s *Store) SetRole(sessionID, roleID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if roleID != "" && s.role(roleID) == nil {
		return errors.New("no such role")
	}
	if s.f.SessionRoles == nil {
		s.f.SessionRoles = map[string]string{}
	}
	s.f.SessionRoles[sessionID] = roleID
	return s.save()
}

// RoleOf reports the role the user gave a session, empty for none. ok is
// false when the user has not said.
func (s *Store) RoleOf(sessionID string) (role Role, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, found := s.f.SessionRoles[sessionID]
	if r := s.role(id); r != nil {
		return *r, true
	}
	return Role{}, found
}

// HasRole reports whether a role with that ID exists.
func (s *Store) HasRole(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.role(id) != nil
}

// SetRoleAttention changes which of a role's sessions may ask for the user.
func (s *Store) SetRoleAttention(roleID, attention string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.role(roleID)
	if r == nil || !validAttention(attention) {
		return errors.New("needs an existing role and an attention of all, stopped or none")
	}
	r.Attention = attention
	return s.save()
}

func (s *Store) role(id string) *Role {
	for i := range s.f.Knowledge.Roles {
		if s.f.Knowledge.Roles[i].ID == id {
			return &s.f.Knowledge.Roles[i]
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
	text, name := strings.TrimSpace(e.Text), strings.TrimSpace(e.Name)
	if e.Effort != "" && s.index(e.Effort) < 0 {
		return errors.New("no such effort")
	}
	s.f.Seq++
	id := section[:1] + strconv.Itoa(s.f.Seq)
	k := &s.f.Knowledge
	switch {
	case section == SectionWatch && name != "" && (e.Type == "person" || e.Type == "channel" || e.Type == "dm"):
		w := Watch{ID: id, Type: e.Type, Name: name, Why: strings.TrimSpace(e.Why), Efforts: []string{}}
		if e.Effort != "" {
			w.Efforts = []string{e.Effort}
		}
		k.Watch = append(k.Watch, w)
	case section == SectionQuestions && text != "":
		k.Questions = append(k.Questions, Question{ID: id, Text: text, Effort: e.Effort, Origin: strings.TrimSpace(e.Origin), At: time.Now()})
	case section == SectionContext && text != "":
		k.Context = append(k.Context, Fact{ID: id, Text: text})
		s.f.ContextRevision++
	case section == SectionRouting && text != "":
		k.Routing = append(k.Routing, Fact{ID: id, Text: text})
		s.f.Revision++
	case section == SectionRoles && name != "" && (e.Type == "" || validAttention(e.Type)):
		k.Roles = append(k.Roles, Role{ID: id, Name: name, About: text, Attention: cmp.Or(e.Type, AttentionAll)})
		s.f.RoleRevision++
	default:
		return errors.New("that entry is missing something, or the section does not exist")
	}
	return s.save()
}

// Remove deletes one entry from a section.
func (s *Store) Remove(section, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := &s.f.Knowledge
	found := false
	switch section {
	case SectionWatch:
		k.Watch, found = without(k.Watch, func(w Watch) bool { return w.ID == id })
	case SectionQuestions:
		k.Questions, found = without(k.Questions, func(q Question) bool { return q.ID == id })
	case SectionContext:
		k.Context, found = without(k.Context, func(f Fact) bool { return f.ID == id })
		s.f.ContextRevision++
	case SectionRouting:
		k.Routing, found = without(k.Routing, func(f Fact) bool { return f.ID == id })
		s.f.Revision++
	case SectionRoles:
		k.Roles, found = without(k.Roles, func(r Role) bool { return r.ID == id })
		for session, role := range s.f.SessionRoles {
			if role == id {
				delete(s.f.SessionRoles, session)
			}
		}
		s.f.RoleRevision++
	}
	if !found {
		return errors.New("no such entry")
	}
	return s.save()
}

// forget drops every reference to a deleted effort.
func (k *Knowledge) forget(effortID string) {
	for i := range k.Questions {
		if k.Questions[i].Effort == effortID {
			k.Questions[i].Effort = ""
		}
	}
	for i := range k.Watch {
		k.Watch[i].Efforts, _ = without(k.Watch[i].Efforts, func(id string) bool { return id == effortID })
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
