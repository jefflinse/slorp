package kb

import (
	"cmp"
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// What a profile's session runs on.
const (
	EngineClaude   = "claude"
	EngineOpenCode = "opencode"
	EngineLocal    = "local"
)

// Profile is a named set of what a new session starts with.
type Profile struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Default marks the one profile a new session starts from when no other
	// is chosen.
	Default bool `json:"default"`
	// Engine is EngineClaude, EngineOpenCode or EngineLocal.
	Engine string `json:"engine"`
	// Dir, Effort and Role are each empty when the profile leaves them open.
	Dir    string `json:"dir"`
	Effort string `json:"effort"`
	Role   string `json:"role"`
	// Terminal is whether the session is also opened in a terminal.
	Terminal bool `json:"terminal"`
	// Model and EffortLevel are a Claude Code session's; empty leaves each to
	// Claude Code.
	Model       string `json:"model"`
	EffortLevel string `json:"effortLevel"`
	// Thinking and Temperature are the local model's; a nil temperature is
	// the model's own.
	Thinking    bool     `json:"thinking"`
	Temperature *float64 `json:"temperature"`
	// OpenCodeAgent, OpenCodeModel and OpenCodeVariant are an OpenCode
	// session's; empty leaves each to OpenCode.
	OpenCodeAgent   string `json:"opencodeAgent"`
	OpenCodeModel   string `json:"opencodeModel"`
	OpenCodeVariant string `json:"opencodeVariant"`
}

// AddProfile keeps a new profile and returns its ID. It needs a name no other
// profile has, whatever the case; a directory that is absolute, an effort and
// a role that exist, where it has them; and a temperature from 0 to 2. Made
// the default, it takes that from the profile that was.
func (s *Store) AddProfile(p Profile) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p.ID = ""
	p, err := s.checked(p)
	if err != nil {
		return "", err
	}
	s.f.Seq++
	p.ID = "p" + strconv.Itoa(s.f.Seq)
	s.f.Knowledge.Profiles = append(s.f.Knowledge.Profiles, p)
	s.soleDefault(p)
	return p.ID, s.save()
}

// EditProfile changes the profile with that ID as edit says, and keeps what
// comes of it as AddProfile would. Nothing is kept when edit fails.
func (s *Store) EditProfile(id string, edit func(*Profile) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.f.Knowledge.Profiles, func(o Profile) bool { return o.ID == id })
	if i < 0 {
		return errors.New("no such profile")
	}
	p := s.f.Knowledge.Profiles[i]
	if err := edit(&p); err != nil {
		return err
	}
	p.ID = id
	p, err := s.checked(p)
	if err != nil {
		return err
	}
	s.f.Knowledge.Profiles[i] = p
	s.soleDefault(p)
	return s.save()
}

// RemoveProfile deletes a profile.
func (s *Store) RemoveProfile(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept, found := without(s.f.Knowledge.Profiles, func(p Profile) bool { return p.ID == id })
	if !found {
		return errors.New("no such profile")
	}
	s.f.Knowledge.Profiles = kept
	return s.save()
}

// checked returns p as it is kept, or why it cannot be.
func (s *Store) checked(p Profile) (Profile, error) {
	p.Name = strings.TrimSpace(p.Name)
	p.Engine = cmp.Or(p.Engine, EngineClaude)
	p.Model, p.EffortLevel = strings.TrimSpace(p.Model), strings.TrimSpace(p.EffortLevel)
	p.OpenCodeAgent, p.OpenCodeModel, p.OpenCodeVariant = strings.TrimSpace(p.OpenCodeAgent), strings.TrimSpace(p.OpenCodeModel), strings.TrimSpace(p.OpenCodeVariant)
	switch {
	case p.Name == "":
		return p, errors.New("a profile needs a name")
	case slices.ContainsFunc(s.f.Knowledge.Profiles, func(o Profile) bool { return o.ID != p.ID && strings.EqualFold(o.Name, p.Name) }):
		return p, errors.New("a profile already has that name")
	case p.Engine != EngineClaude && p.Engine != EngineOpenCode && p.Engine != EngineLocal:
		return p, errors.New("a profile runs on claude, opencode or local")
	case p.Dir != "" && !filepath.IsAbs(p.Dir):
		return p, errors.New("a profile's directory must be an absolute path")
	case p.Effort != "" && s.index(p.Effort) < 0:
		return p, errors.New("no such effort")
	case p.Role != "" && s.role(p.Role) == nil:
		return p, errors.New("no such role")
	case p.Temperature != nil && (*p.Temperature < 0 || *p.Temperature > 2):
		return p, errors.New("a profile's temperature is from 0 to 2")
	}
	if p.Dir != "" {
		p.Dir = filepath.Clean(p.Dir)
	}
	return p, nil
}

// soleDefault leaves p the only default profile when it is one.
func (s *Store) soleDefault(p Profile) {
	if !p.Default {
		return
	}
	for i := range s.f.Knowledge.Profiles {
		s.f.Knowledge.Profiles[i].Default = s.f.Knowledge.Profiles[i].ID == p.ID
	}
}
