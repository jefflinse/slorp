package hub

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jefflinse/slorp/internal/store"
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
// The session and every reply waiting for it are saved as the run's in one
// write before the run is registered; when what was kept of the session
// cannot be read, or that cannot be saved, nothing changes, and in the latter
// case the error wraps ErrNotSaved. A session open, or reported open
// elsewhere, is refused with ErrLifecycleConflict.
func (h *Hub) OpenManaged(agent, id, job, token string, update func(*Session)) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if agent == "" || id == "" || job == "" || token == "" {
		return fmt.Errorf("%w: managed ownership requires an agent, session, job and token", ErrLaunchInvalid)
	}
	if h.sessions[id] != nil || h.runs[token] != nil || h.unmanagedAdmissions[id] != 0 {
		return ErrLifecycleConflict
	}
	if _, held := h.unread[id]; held {
		return fmt.Errorf("%w: session %s is reported open elsewhere", ErrLifecycleConflict, id)
	}
	now := time.Now()
	s := &Session{ID: id, Agent: agent, Kind: "background", Status: Idle, StartedAt: now, ActiveAt: now, managed: true}
	found, err := h.recall(s)
	if err != nil {
		return fmt.Errorf("managed session %s could not be read: %w", id, err)
	}
	if found {
		if err := h.completeReplies(); err != nil {
			return fmt.Errorf("managed session %s %w: %w", id, ErrNotSaved, err)
		}
	}
	s.managed, s.run, s.Job, s.CanStop = true, token, job, true
	if update != nil {
		update(s)
	}
	doc := h.doc(s)
	kept, err := fingerprint(doc)
	if err != nil {
		return fmt.Errorf("managed session %s %w: %w", id, ErrNotSaved, err)
	}
	docs := []store.Doc{{Kind: "session", ID: id, Value: doc}}
	var waiting []*Reply
	for _, r := range h.replies {
		if r.SessionID == id && (r.State == Queued || r.State == Claimed) && r.Owner != OwnerRun {
			owned := *r
			owned.Owner = OwnerRun
			docs, waiting = append(docs, store.Doc{Kind: "reply", ID: r.ID, Value: owned}), append(waiting, r)
		}
	}
	if err := h.cache.PutAll(docs...); err != nil {
		return fmt.Errorf("managed session %s %w: %w", id, ErrNotSaved, err)
	}
	s.persisted = kept
	for _, r := range waiting {
		r.Owner = OwnerRun
	}
	delete(h.unsettled, id)
	delete(h.archive, id)
	ctx, cancel := context.WithCancel(context.Background())
	if h.runs == nil {
		h.runs = map[string]*managedRun{}
	}
	h.runs[token] = &managedRun{agent: agent, root: id, ctx: ctx, cancel: cancel}
	h.sessions[id] = s
	h.changed()
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
