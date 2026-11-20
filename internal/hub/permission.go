package hub

import "time"

const completedReferenceLimit = 256
const permissionDecisionGrace = 2 * time.Minute

type permissionReference struct {
	root, run         string
	awaiting          int
	complete, decided bool
	decisionDeadline  time.Time
}

type completedReference struct {
	id    string
	reply bool
}

func (r *managedRun) hasCompletedReference(id string, reply bool) bool {
	for _, ref := range r.completed {
		if ref.id == id && ref.reply == reply {
			return true
		}
	}
	return false
}

func (h *Hub) rememberCompletedReference(run, id string, reply bool) {
	r := h.runs[run]
	if r == nil || r.closing || r.hasCompletedReference(id, reply) {
		return
	}
	if len(r.completed) == completedReferenceLimit {
		copy(r.completed, r.completed[1:])
		r.completed = r.completed[:completedReferenceLimit-1]
	}
	r.completed = append(r.completed, completedReference{id: id, reply: reply})
}

// finishPermission releases completed ownership only after its decision and active waits are drained.
func (h *Hub) finishPermission(id string) {
	ref := h.permissionRoots[id]
	if ref == nil || !ref.complete || ref.decided || ref.awaiting != 0 {
		return
	}
	delete(h.waiters, id)
	delete(h.permissionRoots, id)
	h.rememberCompletedReference(ref.run, id, false)
}

// prunePermissionDecisions bounds abandoned decisions without expiring an active Await.
func (h *Hub) prunePermissionDecisions(now time.Time) {
	for id, ref := range h.permissionRoots {
		if ref.complete && ref.decided && ref.awaiting == 0 && !ref.decisionDeadline.IsZero() && !now.Before(ref.decisionDeadline) {
			ref.decided = false
			h.finishPermission(id)
		}
	}
}
