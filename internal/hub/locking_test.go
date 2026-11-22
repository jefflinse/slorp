package hub

import (
	"sync"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/kb"
	"github.com/jefflinse/slorp/internal/store"
	"github.com/jefflinse/slorp/internal/testdb"
)

// heldWrites keeps the knowledge base in a store, holding a write under the
// knowledge base's lock, once held is set, until let is closed.
type heldWrites struct {
	*store.Store
	held chan struct{}
	let  chan struct{}
}

func (p *heldWrites) Put(kind, id string, v any) error {
	if p.held != nil {
		p.held <- struct{}{}
		<-p.let
	}
	return p.Store.Put(kind, id, v)
}

// watchedScans keeps the hub in a store, closing scanning when a scan of
// that kind first calls back, once scanning is set.
type watchedScans struct {
	*store.Store
	kind     string
	scanning chan struct{}
	once     sync.Once
}

func (c *watchedScans) Each(kind string, fn func(id string, body []byte)) error {
	return c.Store.Each(kind, func(id string, body []byte) {
		if c.scanning != nil && kind == c.kind {
			c.once.Do(func() { close(c.scanning) })
		}
		fn(id, body)
	})
}

// finishes fails the test unless done is closed within a few seconds.
func finishes(t *testing.T, what string, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never finished", what)
	}
}

func TestJournalAndEffortWriteBothFinish(t *testing.T) {
	st := testdb.Open(t)
	p := &heldWrites{Store: st}
	k, err := kb.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	c := &watchedScans{Store: st, kind: "session"}
	h := New(k, c)
	must(t, st.Put("session", "closed-session", sessionDoc{Agent: agent, Label: "closed", Effort: "gone", Ended: time.Now()}))

	held, let := make(chan struct{}), make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(let) }) })
	p.held, p.let = held, let
	created := make(chan struct{})
	var createErr error
	go func() {
		defer close(created)
		_, createErr = k.Create("Another", "")
	}()
	<-held
	p.held = nil

	// The journal reads the closed sessions while the knowledge base holds
	// its lock, waiting to write.
	c.scanning = make(chan struct{})
	journal := make(chan struct{})
	go func() {
		defer close(journal)
		h.AskWrapup(time.Now().Format(dayLayout))
	}()
	finishes(t, "reaching the journal's reading of the closed sessions", c.scanning)
	release.Do(func() { close(let) })

	finishes(t, "the effort write", created)
	must(t, createErr)
	finishes(t, "the journal", journal)
	state := make(chan struct{})
	go func() {
		defer close(state)
		h.State()
	}()
	finishes(t, "reading the state", state)
}
