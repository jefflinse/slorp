package hub

import (
	"encoding/json"
	"errors"
	"fmt"
)

// LifecycleResult is the durable outcome of one caller-supplied operation ID.
type LifecycleResult struct {
	SessionID string `json:"sessionId"`
	Outcome   string `json:"outcome"`
	Error     string `json:"error,omitempty"`
	Status    int    `json:"-"`
}

type lifecycleRecord struct {
	Fingerprint string          `json:"fingerprint"`
	Status      int             `json:"status"`
	Result      LifecycleResult `json:"result"`
}

func (h *Hub) loadLifecycle() {
	h.lifecycle = map[string]lifecycleRecord{}
	h.lifecycleActive = map[string]bool{}
	err := h.cache.Each("lifecycle", func(id string, b []byte) {
		var record lifecycleRecord
		if err := json.Unmarshal(b, &record); err != nil {
			h.lifecycleErr = err
			return
		}
		if record.Fingerprint == "" || record.Status < 100 || record.Status > 599 {
			h.lifecycleErr = errors.New("invalid lifecycle journal record")
			return
		}
		switch record.Result.Outcome {
		case "pending", "unknown", "accepted", "error":
		default:
			h.lifecycleErr = errors.New("invalid lifecycle journal outcome")
			return
		}
		if record.Result.Outcome == "pending" {
			record.Result.Outcome = "unknown"
			record.Result.Error = "the service stopped before recording a final outcome; this request will not be dispatched again"
			record.Status = 409
		}
		h.lifecycle[id] = record
	})
	h.lifecycleErr = errors.Join(h.lifecycleErr, err)
}

func lifecycleResult(r lifecycleRecord) LifecycleResult {
	result := r.Result
	result.Status = r.Status
	if result.Outcome == "unknown" || result.Outcome == "pending" {
		result.Status = 409
	}
	return result
}

// ReplayLifecycle returns an existing outcome without admitting new work.
func (h *Hub) ReplayLifecycle(id, fingerprint string) (LifecycleResult, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lifecycleErr != nil {
		return LifecycleResult{}, false, fmt.Errorf("reading lifecycle journal: %w", h.lifecycleErr)
	}
	r, exists := h.lifecycle[id]
	if !exists {
		return LifecycleResult{}, false, nil
	}
	if r.Fingerprint != fingerprint {
		return LifecycleResult{}, true, fmt.Errorf("%w: requestId was already used with different operation parameters", ErrLifecycleConflict)
	}
	return lifecycleResult(r), true, nil
}

// BeginLifecycle durably reserves an operation before external side effects.
// Pending entries recovered after restart are never dispatched again.
func (h *Hub) BeginLifecycle(id, fingerprint, sessionID string) (LifecycleResult, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lifecycleErr != nil {
		return LifecycleResult{}, false, fmt.Errorf("reading lifecycle journal: %w", h.lifecycleErr)
	}
	if r, exists := h.lifecycle[id]; exists {
		if r.Fingerprint != fingerprint {
			return LifecycleResult{}, false, fmt.Errorf("%w: requestId was already used with different operation parameters", ErrLifecycleConflict)
		}
		return lifecycleResult(r), false, nil
	}
	r := lifecycleRecord{Fingerprint: fingerprint, Status: 409, Result: LifecycleResult{SessionID: sessionID, Outcome: "pending", Error: "operation is in progress; retry only with the same requestId and parameters"}}
	if err := h.cache.Put("lifecycle", id, r); err != nil {
		return LifecycleResult{}, false, fmt.Errorf("reserving lifecycle operation: %w", err)
	}
	h.lifecycle[id], h.lifecycleActive[id] = r, true
	return lifecycleResult(r), true, nil
}

// CheckpointLifecycle saves the native identity before dispatching a prompt.
func (h *Hub) CheckpointLifecycle(id, sessionID string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, exists := h.lifecycle[id]
	if !exists || !h.lifecycleActive[id] {
		return ErrLifecycleConflict
	}
	if r.Result.SessionID != "" && r.Result.SessionID != sessionID {
		return ErrLifecycleConflict
	}
	r.Result.SessionID = sessionID
	if err := h.cache.Put("lifecycle", id, r); err != nil {
		return fmt.Errorf("checkpointing native session identity: %w", err)
	}
	h.lifecycle[id] = r
	return nil
}

// CompleteLifecycle records the response before it is sent to the caller.
func (h *Hub) CompleteLifecycle(id string, result LifecycleResult) (LifecycleResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, exists := h.lifecycle[id]
	if !exists || !h.lifecycleActive[id] {
		return result, ErrLifecycleConflict
	}
	delete(h.lifecycleActive, id)
	if result.SessionID == "" {
		result.SessionID = r.Result.SessionID
	}
	r.Result, r.Status = result, result.Status
	if err := h.cache.Put("lifecycle", id, r); err != nil {
		r.Result.Outcome, r.Result.Error, r.Status = "unknown", "could not durably record the operation outcome; do not repeat with a new requestId", 409
		h.lifecycle[id] = r
		return lifecycleResult(r), fmt.Errorf("recording lifecycle outcome: %w", err)
	}
	h.lifecycle[id] = r
	return result, nil
}
