package opencode

import (
	"context"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/hub"
	"github.com/jefflinse/slorp/internal/testdb"
)

func TestSessionsLastAsLongAsTheirPluginReports(t *testing.T) {
	st := testdb.Open(t)
	h := hub.New(testdb.KB(t, st), st)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		(&Adapter{Hub: h, Interval: 5 * time.Millisecond, Silence: 150 * time.Millisecond}).Run(ctx)
		close(done)
	}()

	h.Apply(Agent, "quit", nil)
	h.Apply("claude", "other", nil)
	listed := func() map[string]bool {
		out := map[string]bool{}
		for _, s := range h.Sessions() {
			out[s.ID] = true
		}
		return out
	}
	for deadline := time.Now().Add(5 * time.Second); listed()["quit"]; {
		h.Apply(Agent, "open", nil)
		if time.Now().After(deadline) {
			t.Fatal("a session whose plugin stopped reporting is still listed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := listed(); !got["open"] || !got["other"] {
		t.Errorf("sessions = %v, want the one still reporting and the other agent's kept", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return when its context ended")
	}
}
