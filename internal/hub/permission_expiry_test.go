package hub

import (
	"context"
	"testing"
	"time"
)

func prunePermissionsAt(h *Hub, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.prunePermissionDecisions(now)
}

func TestAbandonedDecisionsExpireWithoutAnotherAsk(t *testing.T) {
	for _, tick := range []string{"retire", "roster"} {
		t.Run(tick, func(t *testing.T) {
			h, _, _ := newHub(t)
			for range 1000 {
				p := h.Ask("claude", "ordinary", Pending{Tool: "Bash"}, true)
				if !h.Decide(p.ID, Decision{Behavior: Allow}) {
					t.Fatal("decision rejected")
				}
				deadline := h.permissionRoots[p.ID].decisionDeadline
				h.Done(p.ID)
				h.Retire("claude", -time.Second)
				if !h.permissionRoots[p.ID].decisionDeadline.Equal(deadline) {
					t.Fatal("Done/Retire extended the grace period")
				}
			}
			if len(h.permissionRoots) != 1000 || len(h.waiters) != 1000 {
				t.Fatal("decisions lost during retry grace")
			}
			// Advance decision age, then exercise the production tick without another Ask.
			h.mu.Lock()
			for _, ref := range h.permissionRoots {
				ref.decisionDeadline = time.Now().Add(-time.Second)
			}
			h.mu.Unlock()
			if tick == "retire" {
				h.Retire("opencode", time.Hour)
			} else {
				h.SetRoster("claude", nil)
			}
			if len(h.permissionRoots) != 0 || len(h.waiters) != 0 {
				t.Fatalf("abandoned decisions retained: roots=%d waiters=%d", len(h.permissionRoots), len(h.waiters))
			}
		})
	}
}

func TestPermissionDecisionGraceBoundaryAndCancelledRetry(t *testing.T) {
	h, _, _ := newHub(t)
	p := h.Ask("claude", "ordinary", Pending{Tool: "Bash"}, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := h.Await(ctx, p.ID, time.Second); ok {
		t.Fatal("cancelled undecided wait completed")
	}
	if !h.Decide(p.ID, Decision{Behavior: Deny, Message: "retry this decision"}) {
		t.Fatal("decision rejected")
	}
	deadline := h.permissionRoots[p.ID].decisionDeadline
	h.Done(p.ID)
	h.Retire("claude", -time.Second)
	prunePermissionsAt(h, deadline.Add(-time.Nanosecond))
	if _, ok := h.permissionRoots[p.ID]; !ok {
		t.Fatal("retry grace expired early")
	}
	if dec, ok := h.Await(context.Background(), p.ID, time.Second); !ok || dec.Behavior != Deny || dec.Message != "retry this decision" {
		t.Fatal(dec, ok)
	}

	p = h.Ask("claude", "ordinary", Pending{Tool: "Bash"}, true)
	if !h.Decide(p.ID, Decision{Behavior: Allow}) {
		t.Fatal("decision rejected")
	}
	deadline = h.permissionRoots[p.ID].decisionDeadline
	prunePermissionsAt(h, deadline)
	if len(h.permissionRoots) != 0 || len(h.waiters) != 0 {
		t.Fatal("decision retained at deadline")
	}
	if dec, ok := h.Await(context.Background(), p.ID, time.Second); !ok || dec.Behavior != Terminal {
		t.Fatal(dec, ok)
	}
}

func TestPermissionExpiryProtectsActiveWaitAndUndecidedPrompt(t *testing.T) {
	h, _, _ := newHub(t)
	p := h.Ask("claude", "ordinary", Pending{Tool: "Bash"}, true)
	if !h.Decide(p.ID, Decision{Behavior: Allow}) {
		t.Fatal("decision rejected")
	}
	pending := h.Ask("claude", "other", Pending{Tool: "Bash"}, true)
	h.mu.Lock()
	ref := h.permissionRoots[p.ID]
	ref.awaiting++ // Hold an admitted wait while advancing sweep time.
	future := ref.decisionDeadline.Add(time.Hour)
	h.mu.Unlock()
	prunePermissionsAt(h, future)
	h.mu.Lock()
	if h.permissionRoots[p.ID] != ref || h.waiters[p.ID] == nil {
		t.Error("expired an active wait")
	}
	ref.awaiting--
	h.mu.Unlock()
	prunePermissionsAt(h, future)
	if _, ok := h.permissionRoots[p.ID]; ok {
		t.Fatal("abandoned wait survived its deadline")
	}
	if _, ok := h.permissionRoots[pending.ID]; !ok {
		t.Fatal("expired an undecided prompt")
	}
	h.Done(pending.ID)
}

func TestManagedDecisionExpiryUsesBoundedGenerationTombstones(t *testing.T) {
	h, _, _ := newHub(t)
	must(t, h.OpenManaged("opencode", "root", "J1", "T1", nil))
	var deadline time.Time
	for range 1000 {
		p := h.Ask("opencode", "root", Pending{Tool: "shell"}, true)
		if !h.Decide(p.ID, Decision{Behavior: Allow}) {
			t.Fatal("decision rejected")
		}
		deadline = h.permissionRoots[p.ID].decisionDeadline
		h.Done(p.ID)
	}
	prunePermissionsAt(h, deadline)
	if len(h.permissionRoots) != 0 || len(h.waiters) != 0 || len(h.runs["T1"].completed) != completedReferenceLimit {
		t.Fatal("expired managed decisions not bounded")
	}
	retained := h.runs["T1"].completed[completedReferenceLimit-1].id
	ctx, release, err := h.BridgeLease(context.Background(), "T1", "", "", retained, "/api/bridge/permission/wait")
	must(t, err)
	dec, ok := h.Await(ctx, retained, time.Second)
	release()
	if !ok || dec.Behavior != Terminal {
		t.Fatal("expired decision was delivered", dec, ok)
	}
	h.CloseManaged("T1")
}
