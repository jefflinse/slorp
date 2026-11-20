package hub

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestOrdinaryPermissionReferencesAreReleased(t *testing.T) {
	for _, mode := range []string{"done", "decide-await", "retire"} {
		t.Run(mode, func(t *testing.T) {
			h, _, _ := newHub(t)
			for range 1000 {
				p := h.Ask("claude", "ordinary", Pending{Tool: "Bash"}, true)
				switch mode {
				case "done":
					h.Done(p.ID)
				case "decide-await":
					if !h.Decide(p.ID, Decision{Behavior: Allow}) {
						t.Fatal("decision rejected")
					}
					if _, ok := h.permissionRoots[p.ID]; !ok {
						t.Fatal("ownership removed before delivery")
					}
					if dec, ok := h.Await(context.Background(), p.ID, time.Second); !ok || dec.Behavior != Allow {
						t.Fatal(dec, ok)
					}
				case "retire":
					h.Retire("claude", -time.Second)
				}
			}
			h.Retire("claude", -time.Second)
			if len(h.permissionRoots) != 0 || len(h.waiters) != 0 {
				t.Fatalf("completed ordinary requests retained: roots=%d waiters=%d", len(h.permissionRoots), len(h.waiters))
			}
		})
	}
}

func TestUndeliveredDecisionKeepsOwnershipThroughDoneAndRetire(t *testing.T) {
	h, _, _ := newHub(t)
	p := h.Ask("claude", "ordinary", Pending{Tool: "Bash"}, true)
	if !h.Decide(p.ID, Decision{Behavior: Deny, Message: "keep this decision"}) {
		t.Fatal("decision rejected")
	}
	h.Done(p.ID)
	h.Retire("claude", -time.Second)
	if _, ok := h.permissionRoots[p.ID]; !ok {
		t.Fatal("undelivered decision lost ownership")
	}
	ctx, release, err := h.BridgeLease(context.Background(), "", "", "", p.ID, "/api/bridge/permission/wait")
	must(t, err)
	dec, ok := h.Await(ctx, p.ID, time.Second)
	release()
	if !ok || dec.Behavior != Deny || dec.Message != "keep this decision" {
		t.Fatal(dec, ok)
	}
	if len(h.permissionRoots) != 0 || len(h.waiters) != 0 {
		t.Fatal("delivered decision retained")
	}
}

func TestManagedCompletedReferencesAreBoundedAndGenerationScoped(t *testing.T) {
	h, _, _ := newHub(t)
	must(t, h.OpenManaged("opencode", "root", "J1", "T1", nil))
	pinned := h.Ask("opencode", "root", Pending{Tool: "shell"}, true)
	if !h.Decide(pinned.ID, Decision{Behavior: Allow}) {
		t.Fatal("decision rejected")
	}
	var first, last, reply string
	for i := range 1000 {
		p := h.Ask("opencode", "root", Pending{Tool: "shell"}, true)
		if i == 0 {
			first = p.ID
		}
		last = p.ID
		h.Done(p.ID)
		r, ok := h.Enqueue("root", "reply")
		if !ok {
			t.Fatal("enqueue")
		}
		reply = r.ID
		h.Claim("root")
		state := Delivered
		if i%2 == 1 {
			state = Dropped
		}
		h.Ack(r.ID, state)
	}
	if len(h.permissionRoots) != 1 || len(h.waiters) != 1 || len(h.replyRuns) != 0 || len(h.runs["T1"].completed) != completedReferenceLimit {
		t.Fatalf("unbounded completed references: roots=%d waiters=%d", len(h.permissionRoots), len(h.waiters))
	}
	if _, release, err := h.BridgeLease(context.Background(), "T1", "", "", first, "/api/bridge/permission/done"); err == nil {
		release()
		t.Fatal("old completed reference was never evicted")
	}
	for _, ref := range []struct{ id, path string }{{last, "/api/bridge/permission/done"}, {reply, "/api/bridge/ack"}, {pinned.ID, "/api/bridge/permission/wait"}} {
		_, release, err := h.BridgeLease(context.Background(), "T1", "", "", ref.id, ref.path)
		must(t, err)
		release()
	}
	if dec, ok := h.Await(context.Background(), pinned.ID, time.Second); !ok || dec.Behavior != Allow {
		t.Fatal("evicted undelivered decision", dec, ok)
	}
	h.CloseManaged("T1")
	must(t, h.OpenManaged("opencode", "root", "J2", "T2", nil))
	for _, token := range []string{"T1", "T2"} {
		for _, ref := range []struct{ id, path string }{{last, "/api/bridge/permission/done"}, {reply, "/api/bridge/ack"}, {pinned.ID, "/api/bridge/permission/wait"}} {
			if _, release, err := h.BridgeLease(context.Background(), token, "", "", ref.id, ref.path); !errors.Is(err, ErrLifecycleConflict) {
				if err == nil {
					release()
				}
				t.Fatalf("old generation accepted: token=%s ref=%s err=%v", token, ref.id, err)
			}
		}
	}
	h.CloseManaged("T2")
}

func TestPermissionWaitCancellationPreservesUndeliveredDecision(t *testing.T) {
	h, _, _ := newHub(t)
	for range 100 {
		p := h.Ask("claude", "ordinary", Pending{Tool: "Bash"}, true)
		if !h.Decide(p.ID, Decision{Behavior: Deny, Message: "retain"}) {
			t.Fatal("decision rejected")
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		dec, ok := h.Await(ctx, p.ID, time.Second)
		if !ok {
			if _, found := h.permissionRoots[p.ID]; !found {
				t.Fatal("cancelled wait lost undelivered ownership")
			}
			dec, ok = h.Await(context.Background(), p.ID, time.Second)
		}
		if !ok || dec.Behavior != Deny || dec.Message != "retain" {
			t.Fatal(dec, ok)
		}
	}
	if len(h.permissionRoots) != 0 || len(h.waiters) != 0 {
		t.Fatal("completed waits retained")
	}
}

func TestConcurrentPermissionWaitsReleaseCompletedOwnership(t *testing.T) {
	h, _, _ := newHub(t)
	p := h.Ask("claude", "ordinary", Pending{Tool: "Bash"}, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make(chan Decision, 2)
	var waits sync.WaitGroup
	defer func() { cancel(); waits.Wait() }()
	for range 2 {
		waits.Add(1)
		go func() { defer waits.Done(); dec, _ := h.Await(ctx, p.ID, 5*time.Second); results <- dec }()
	}
	deadline := time.Now().Add(time.Second)
	for {
		h.mu.Lock()
		waiting := 0
		if ref := h.permissionRoots[p.ID]; ref != nil {
			waiting = ref.awaiting
		}
		h.mu.Unlock()
		if waiting == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("waiters did not enter")
		}
		time.Sleep(time.Millisecond)
	}
	if !h.Decide(p.ID, Decision{Behavior: Allow}) {
		t.Fatal("decision rejected")
	}
	behaviors := map[string]int{}
	for range 2 {
		select {
		case dec := <-results:
			behaviors[dec.Behavior]++
		case <-time.After(time.Second):
			t.Fatal("waiter did not settle")
		}
	}
	if behaviors[Allow] != 1 || behaviors[Terminal] != 1 {
		t.Fatal(behaviors)
	}
	if len(h.permissionRoots) != 0 || len(h.waiters) != 0 {
		t.Fatal("drained waits retained ownership")
	}
}
