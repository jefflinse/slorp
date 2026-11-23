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
	err := s.update(func(f *file) error {
		var err error
		if h, err = f.checkedHandoff(h); err != nil {
			return err
		}
		f.Seq++
		h.ID = "h" + strconv.Itoa(f.Seq)
		f.Knowledge.Handoffs = append(f.Knowledge.Handoffs, h)
		return nil
	})
	if err != nil {
		return "", err
	}
	return h.ID, nil
}

// EditHandoff changes the handoff with that ID as edit says, and keeps what
// comes of it as AddHandoff would. Nothing is kept when edit fails.
func (s *Store) EditHandoff(id string, edit func(*Handoff) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.update(func(f *file) error {
		i := slices.IndexFunc(f.Knowledge.Handoffs, func(o Handoff) bool { return o.ID == id })
		if i < 0 {
			return errors.New("no such handoff")
		}
		h := f.Knowledge.Handoffs[i]
		if err := edit(&h); err != nil {
			return err
		}
		h.ID = id
		h, err := f.checkedHandoff(h)
		if err != nil {
			return err
		}
		f.Knowledge.Handoffs[i] = h
		return nil
	})
}

// RemoveHandoff deletes a handoff.
func (s *Store) RemoveHandoff(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.update(func(f *file) error {
		kept, found := without(f.Knowledge.Handoffs, func(h Handoff) bool { return h.ID == id })
		if !found {
			return errors.New("no such handoff")
		}
		f.Knowledge.Handoffs = kept
		return nil
	})
}

// checkedHandoff returns h as it is kept, or why it cannot be.
func (f *file) checkedHandoff(h Handoff) (Handoff, error) {
	h.Name, h.Text = strings.TrimSpace(h.Name), strings.TrimSpace(h.Text)
	switch {
	case h.Name == "":
		return h, errors.New("a handoff needs a name")
	case slices.ContainsFunc(f.Knowledge.Handoffs, func(o Handoff) bool { return o.ID != h.ID && strings.EqualFold(o.Name, h.Name) }):
		return h, errors.New("a handoff already has that name")
	case h.Text == "":
		return h, errors.New("a handoff needs a message")
	case utf8.RuneCountInString(h.Text) > HandoffLimit:
		return h, errors.New("a handoff's message is too long")
	}
	return h, nil
}
