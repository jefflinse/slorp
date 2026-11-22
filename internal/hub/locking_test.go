package hub

import (
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
	h := New(k, st)
	must(t, st.Put("session", "closed-session", sessionDoc{Agent: agent, Label: "closed", Effort: "gone", Ended: time.Now()}))

	p.held, p.let = make(chan struct{}), make(chan struct{})
	created := make(chan struct{})
	var createErr error
	go func() {
		defer close(created)
		_, createErr = k.Create("Another", "")
	}()
	<-p.held
	p.held = nil

	// The journal reads the closed sessions while the knowledge base holds
	// its lock, waiting to write.
	journal := make(chan struct{})
	go func() {
		defer close(journal)
		h.AskWrapup(time.Now().Format(dayLayout))
	}()
	time.Sleep(100 * time.Millisecond)
	close(p.let)

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
