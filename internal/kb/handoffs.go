package kb

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// HandoffLimit is how long a handoff's text may be, in runes.
const HandoffLimit = 20000

// Handoff is a named message that carries what one session said to another.
type Handoff struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Text is the message as the user wrote it. The page fills in what it
	// names of the session it is sent from, such as {{response}}.
	Text string `json:"text"`
}

// AddHandoff keeps a new handoff and returns its ID. It needs a name no other
// handoff has, whatever the case, and a text of at most HandoffLimit runes.
func (s *Store) AddHandoff(h Handoff) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h.ID = ""
	h, err := s.checkedHandoff(h)
	if err != nil {
		return "", err
	}
	s.f.Seq++
	h.ID = "h" + strconv.Itoa(s.f.Seq)
	s.f.Knowledge.Handoffs = append(s.f.Knowledge.Handoffs, h)
	return h.ID, s.save()
}

// EditHandoff changes the handoff with that ID as edit says, and keeps what
// comes of it as AddHandoff would. Nothing is kept when edit fails.
func (s *Store) EditHandoff(id string, edit func(*Handoff) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.f.Knowledge.Handoffs, func(o Handoff) bool { return o.ID == id })
	if i < 0 {
		return errors.New("no such handoff")
	}
	h := s.f.Knowledge.Handoffs[i]
	if err := edit(&h); err != nil {
		return err
	}
	h.ID = id
	h, err := s.checkedHandoff(h)
	if err != nil {
		return err
	}
	s.f.Knowledge.Handoffs[i] = h
	return s.save()
}

// RemoveHandoff deletes a handoff.
func (s *Store) RemoveHandoff(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept, found := without(s.f.Knowledge.Handoffs, func(h Handoff) bool { return h.ID == id })
	if !found {
		return errors.New("no such handoff")
	}
	s.f.Knowledge.Handoffs = kept
	return s.save()
}

// checkedHandoff returns h as it is kept, or why it cannot be.
func (s *Store) checkedHandoff(h Handoff) (Handoff, error) {
	h.Name, h.Text = strings.TrimSpace(h.Name), strings.TrimSpace(h.Text)
	switch {
	case h.Name == "":
		return h, errors.New("a handoff needs a name")
	case slices.ContainsFunc(s.f.Knowledge.Handoffs, func(o Handoff) bool { return o.ID != h.ID && strings.EqualFold(o.Name, h.Name) }):
		return h, errors.New("a handoff already has that name")
	case h.Text == "":
		return h, errors.New("a handoff needs a message")
	case utf8.RuneCountInString(h.Text) > HandoffLimit:
		return h, errors.New("a handoff's message is too long")
	}
	return h, nil
}
