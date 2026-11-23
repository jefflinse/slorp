package hub

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type managedRun struct {
	agent, root string
	ctx         context.Context
	cancel      context.CancelFunc
	closing     bool
	leases      sync.WaitGroup
	completed   []completedReference
}

// OpenManaged registers live ownership, never recovered from a persisted job or PID.
func (h *Hub) OpenManaged(agent, id, job, token string, update func(*Session)) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if agent == "" || id == "" || job == "" || token == "" {
		return fmt.Errorf("%w: managed ownership requires an agent, session, job and token", ErrLaunchInvalid)
	}
	if h.sessions[id] != nil || h.runs[token] != nil || h.unmanagedAdmissions[id] != 0 {
		return ErrLifecycleConflict
	}
	if err := h.takeReplies(id); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := &managedRun{agent: agent, root: id, ctx: ctx, cancel: cancel}
	if h.runs == nil {
		h.runs = map[string]*managedRun{}
	}
	h.runs[token] = run
	now := time.Now()
	s := &Session{ID: id, Agent: agent, Kind: "background", Status: Idle, StartedAt: now, ActiveAt: now, managed: true}
	h.restore(s)
	s.managed, s.run, s.Job, s.CanStop = true, token, job, true
	if update != nil {
		update(s)
	}
	h.sessions[id] = s
	err := h.checkpoint(s)
	h.changed()
	if err != nil {
		return fmt.Errorf("persisting managed session %s: %w", id, err)
	}
	return nil
}

// FenceManaged rejects new bridge leases and cancels existing permission waits.
func (h *Hub) FenceManaged(token string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if run := h.runs[token]; run != nil {
		run.closing = true
		run.cancel()
		if s := h.sessions[run.root]; s != nil && s.run == token {
			s.CanStop = false
			if s.Pending != nil {
				h.settle(s.Pending.ID)
			}
		}
	}
}

// CloseManaged drains bridge mutations before archiving the owned generation.
func (h *Hub) CloseManaged(token string) {
	h.FenceManaged(token)
	h.mu.Lock()
	run := h.runs[token]
	h.mu.Unlock()
	if run == nil {
		return
	}
	run.leases.Wait()
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.runs, token)
	for id, ref := range h.permissionRoots {
		if ref.run == token {
			h.settle(id)
			ref.decided = false
			h.finishPermission(id)
		}
	}
	for id, owner := range h.replyRuns {
		if owner == token {
			delete(h.replyRuns, id)
		}
	}
	if s := h.sessions[run.root]; s != nil && s.run == token {
		h.dropReplies(s.ID)
		h.leave(s)
		delete(h.sessions, s.ID)
		h.changed()
	}
}

// takeReplies makes the run about to be opened for a session the owner of
// what is waiting for it, as if queued for the run, all of it or, with an
// error wrapping ErrNotSaved, none.
func (h *Hub) takeReplies(id string) error {
	taken := map[string]any{}
	var waiting []*Reply
	for _, r := range h.replies {
		if r.SessionID == id && (r.State == Queued || r.State == Claimed) && r.Owner != OwnerRun {
			owned := *r
			owned.Owner = OwnerRun
			taken[r.ID], waiting = owned, append(waiting, r)
		}
	}
	if len(taken) == 0 {
		return nil
	}
	if err := h.cache.PutAll("reply", taken); err != nil {
		return fmt.Errorf("the replies waiting for %s %w: %w", id, ErrNotSaved, err)
	}
	for _, r := range waiting {
		r.Owner = OwnerRun
	}
	return nil
}

func (h *Hub) dropReplies(id string) {
	for _, r := range h.replies {
		if r.SessionID == id && (r.State == Queued || r.State == Claimed) {
			r.State = Dropped
			h.warn(h.cache.Put("reply", r.ID, r))
		}
	}
}

// BridgeLease validates a managed request's root or referenced permission/reply.
// The release must run after the handler's last mutation, including on cancellation.
func (h *Hub) BridgeLease(ctx context.Context, token, agent, root, reference, endpoint string) (context.Context, func(), error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if agent == "" && endpoint != "/api/bridge/ack" && endpoint != "/api/bridge/permission/wait" && endpoint != "/api/bridge/permission/done" {
		agent = "claude"
	}
	if reference != "" {
		var owner, ownerRun string
		reply := endpoint == "/api/bridge/ack"
		if reply {
			for _, r := range h.replies {
				if r.ID == reference {
					owner = r.SessionID
					ownerRun = h.replyRuns[reference]
					break
				}
			}
		} else {
			if ref := h.permissionRoots[reference]; ref != nil {
				owner, ownerRun = ref.root, ref.run
			}
		}
		if token != "" && ownerRun != token {
			run := h.runs[token]
			if run == nil || !run.hasCompletedReference(reference, reply) {
				return nil, nil, ErrLifecycleConflict
			}
			owner, ownerRun = run.root, token
		}
		if token == "" && ownerRun != "" {
			return nil, nil, ErrLifecycleConflict
		}
		if root != "" && owner != root {
			return nil, nil, ErrLifecycleConflict
		}
		root = owner
	}
	if token == "" {
		if s := h.sessions[root]; s != nil && s.run != "" {
			return nil, nil, ErrLifecycleConflict
		}
		if root == "" {
			return ctx, func() {}, nil
		}
		if h.unmanagedAdmissions == nil {
			h.unmanagedAdmissions = map[string]int{}
		}
		h.unmanagedAdmissions[root]++
		return ctx, func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.unmanagedAdmissions[root]--
			if h.unmanagedAdmissions[root] == 0 {
				delete(h.unmanagedAdmissions, root)
			}
		}, nil
	}
	run := h.runs[token]
	if run == nil || run.closing || root != run.root || agent != "" && agent != run.agent {
		return nil, nil, ErrLifecycleConflict
	}
	run.leases.Add(1)
	child, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(run.ctx, cancel)
	return child, func() { stop(); cancel(); run.leases.Done() }, nil
}
