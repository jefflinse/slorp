package hub

import (
	"errors"
	"testing"
)

func TestLifecycleJournalReplayAndCrashRecovery(t *testing.T) {
	h, k, cache := newHub(t)
	result, admitted, err := h.BeginLifecycle("key", "hash", "")
	must(t, err)
	if !admitted || result.Outcome != "pending" {
		t.Fatal(result, admitted)
	}
	must(t, h.CheckpointLifecycle("key", "ses_created"))
	result, admitted, err = h.BeginLifecycle("key", "hash", "")
	must(t, err)
	if admitted || result.Status != 409 || result.SessionID != "ses_created" {
		t.Fatal(result, admitted)
	}
	if _, _, err := h.BeginLifecycle("key", "different", ""); !errors.Is(err, ErrLifecycleConflict) {
		t.Fatal(err)
	}
	restarted := New(k, cache)
	result, admitted, err = restarted.BeginLifecycle("key", "hash", "")
	must(t, err)
	if admitted || result.Outcome != "unknown" || result.SessionID != "ses_created" || result.Status != 409 {
		t.Fatal(result, admitted)
	}
	_, _, err = h.BeginLifecycle("accepted", "second", "")
	must(t, err)
	_, err = h.CompleteLifecycle("accepted", LifecycleResult{SessionID: "ses_known", Outcome: "accepted", Status: 202})
	must(t, err)
	restarted = New(k, cache)
	result, admitted, err = restarted.BeginLifecycle("accepted", "second", "")
	must(t, err)
	if admitted || result.Status != 202 || result.Outcome != "accepted" || result.SessionID != "ses_known" {
		t.Fatal(result, admitted)
	}
}

type journalFailure struct {
	Cache
	fail bool
}

func (c *journalFailure) Put(kind, id string, v any) error {
	if c.fail && kind == "lifecycle" {
		return errors.New("journal unavailable")
	}
	return c.Cache.Put(kind, id, v)
}

func TestLifecycleJournalFailsClosed(t *testing.T) {
	_, k, cache := newHub(t)
	fault := &journalFailure{Cache: cache, fail: true}
	h := New(k, fault)
	if _, admitted, err := h.BeginLifecycle("key", "hash", ""); err == nil || admitted {
		t.Fatal("admitted without durable reservation")
	}
	fault.fail = false
	_, _, err := h.BeginLifecycle("key", "hash", "")
	must(t, err)
	must(t, h.CheckpointLifecycle("key", "ses_known"))
	fault.fail = true
	result, err := h.CompleteLifecycle("key", LifecycleResult{Outcome: "accepted", Status: 202})
	if err == nil || result.Outcome != "unknown" || result.SessionID != "ses_known" {
		t.Fatal(result, err)
	}
	restarted := New(k, cache)
	result, admitted, err := restarted.BeginLifecycle("key", "hash", "")
	must(t, err)
	if admitted || result.Outcome != "unknown" || result.SessionID != "ses_known" {
		t.Fatal(result, admitted)
	}
}
