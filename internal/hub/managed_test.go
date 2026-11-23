package hub

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestManagedFenceDrainsLeasesBeforeArchive(t *testing.T) {
	h, _, _ := newHub(t)
	must(t, h.OpenManaged("opencode", "ses_root", "job", "private-token", func(s *Session) { s.Cwd = "/work" }))
	ctx, release, err := h.BridgeLease(context.Background(), "private-token", "opencode", "ses_root", "", "/api/bridge/poll")
	must(t, err)
	closed := make(chan struct{})
	go func() { h.CloseManaged("private-token"); close(closed) }()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("lease not cancelled")
	}
	select {
	case <-closed:
		t.Fatal("archived before draining lease")
	default:
	}
	if _, _, err := h.BridgeLease(context.Background(), "private-token", "opencode", "ses_root", "", "/api/bridge/poll"); !errors.Is(err, ErrLifecycleConflict) {
		t.Fatal(err)
	}
	h.Apply("opencode", "ses_root", func(s *Session) { s.LastReply = "last in-flight result" })
	if len(h.Sessions()) != 1 {
		t.Fatal("global mutex held or session lost before drain")
	}
	release()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("close did not drain")
	}
	if _, ok := h.Archive("ses_root"); !ok {
		t.Fatal("empty managed session not archived")
	}
	if _, _, err := h.BridgeLease(context.Background(), "private-token", "opencode", "ses_root", "", "/api/bridge/poll"); err == nil {
		t.Fatal("stopped token accepted")
	}
	if len(h.Sessions()) != 0 {
		t.Fatal("in-flight request reopened stopped session")
	}
	must(t, h.OpenManaged("opencode", "ses_root", "new-job", "new-token", nil))
	if session(t, h, "ses_root").LastReply != "last in-flight result" {
		t.Fatal("last accepted mutation was lost")
	}
	h.CloseManaged("new-token")
}

func TestManagedReferenceFencingAndPermissionCancellation(t *testing.T) {
	h, _, _ := newHub(t)
	must(t, h.OpenManaged("opencode", "one", "job-one", "token-one", nil))
	must(t, h.OpenManaged("opencode", "two", "job-two", "token-two", nil))
	p := h.Ask("opencode", "one", Pending{Tool: "shell"}, true)
	r, err := h.Enqueue("one", "queued")
	must(t, err)
	_, err = h.Claim("one")
	must(t, err)
	for _, tc := range []struct{ token, agent, root, id, path string }{
		{"unknown", "opencode", "one", "", "/api/bridge/poll"},
		{"token-one", "claude", "one", "", "/api/bridge/event"},
		{"token-one", "opencode", "two", "", "/api/bridge/subagent"},
		{"token-two", "", "", p.ID, "/api/bridge/permission/wait"},
		{"token-two", "", "", r.ID, "/api/bridge/ack"},
		{"", "opencode", "one", "", "/api/bridge/poll"},
		{"", "", "", p.ID, "/api/bridge/permission/done"},
	} {
		if _, release, err := h.BridgeLease(context.Background(), tc.token, tc.agent, tc.root, tc.id, tc.path); err == nil {
			release()
			t.Fatalf("accepted %+v", tc)
		}
	}
	ctx, release, err := h.BridgeLease(context.Background(), "token-one", "", "", p.ID, "/api/bridge/permission/wait")
	must(t, err)
	done := make(chan struct{})
	go func() { h.Await(ctx, p.ID, time.Minute); release(); close(done) }()
	h.CloseManaged("token-one")
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waiter not cancelled")
	}
	if h.replies[0].State != Dropped {
		t.Fatal("claimed reply not dropped")
	}
	h.CloseManaged("token-two")
}

func TestManagedRestartPreservesIdentityWithoutRestoringOwnership(t *testing.T) {
	h, k, cache := newHub(t)
	e := effort(t, k, "Managed work")
	r := role(t, k, "Reviewer", "")
	must(t, h.OpenManaged("opencode", "ses_empty", "opaque-job", "private-token", func(s *Session) {
		s.Cwd = "/work"
		s.StartedAt = time.Unix(100, 0)
		s.Used.Out = 9
		s.Model = "provider/model"
		s.Reasoning = "high"
	}))
	must(t, k.Assign("ses_empty", e))
	must(t, k.SetRole("ses_empty", r))
	h.Touch()
	must(t, h.UpsertSubagent("opencode", "ses_empty", 1, Subagent{ID: "child", Status: "completed"}, &SubagentReport{Report: "retained result"}))
	_, err := h.Enqueue("ses_empty", "never replay after restart")
	must(t, err)
	// Simulate a crash rather than orderly close.
	restarted := New(k, cache)
	restarted.Retire("opencode", -time.Second)
	a, ok := restarted.Archive("ses_empty")
	if !ok || a.Cwd != "/work" || a.Used.Out != 9 {
		t.Fatalf("archive: %+v", a)
	}
	if _, _, err := restarted.BridgeLease(context.Background(), "private-token", "opencode", "ses_empty", "", "/api/bridge/poll"); err == nil {
		t.Fatal("restored process ownership")
	}
	must(t, restarted.OpenManaged("opencode", "ses_empty", "new-job", "new-token", nil))
	s := session(t, restarted, "ses_empty")
	if s.Effort != e || s.Role != r || s.Cwd != "/work" || !s.StartedAt.Equal(a.StartedAt) || s.Used != a.Used || s.Model != "provider/model" || s.Reasoning != "high" {
		t.Fatalf("metadata lost: %+v", s)
	}
	if len(s.Replies) != 1 || s.Replies[0].State != Dropped {
		t.Fatal("restart replayed old outbox")
	}
	if report, ok := restarted.RetainedSubagentReport("opencode", "ses_empty", "child"); !ok || report.Report != "retained result" {
		t.Fatal("lost retained child report")
	}
	b, _ := json.Marshal(restarted.State())
	if strings.Contains(string(b), "private-token") || strings.Contains(string(b), "new-token") {
		t.Fatal("private token published")
	}
	h.CloseManaged("private-token")
	restarted.CloseManaged("new-token")
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{"": "''", "normal": "'normal'", "a'b": "'a'\"'\"'b'", "$(touch /tmp/no)": "'$(touch /tmp/no)'", "line\nbreak": "'line\nbreak'"} {
		if got := ShellQuote(in); got != want {
			t.Fatalf("quote %q: %q, want %q", in, got, want)
		}
	}
}

func TestManagingPreviouslyObservedSessionRetainsMetadata(t *testing.T) {
	h, _, _ := newHub(t)
	h.Apply("opencode", "observed", func(s *Session) {
		s.Cwd = "/original"
		s.StartedAt = time.Unix(123, 0)
		s.Used.Out = 42
		s.CostUSD = 1.25
		s.LastPrompt = "original prompt"
	})
	h.Retire("opencode", -time.Second)
	a, ok := h.Archive("observed")
	if !ok {
		t.Fatal("observed session not archived")
	}
	if err := h.OpenManaged("opencode", "observed", "new-job", "new-token", nil); err != nil {
		t.Fatal(err)
	}
	s := session(t, h, "observed")
	if s.Cwd != a.Cwd || !s.StartedAt.Equal(a.StartedAt) || s.Used != a.Used || s.CostUSD != a.CostUSD || len(s.Timeline) != len(a.Timeline) {
		t.Fatalf("lost observed metadata: %+v / %+v", s, a)
	}
	h.CloseManaged("new-token")
}

func TestTokenlessAdmissionBlocksManagedOwnership(t *testing.T) {
	h, _, _ := newHub(t)
	h.Apply("opencode", "root", func(s *Session) { s.LastPrompt = "earlier turn" })
	if _, err := h.Enqueue("root", "old reply"); err != nil {
		t.Fatal(err)
	}
	_, release, err := h.BridgeLease(context.Background(), "", "opencode", "root", "", "/api/bridge/poll")
	must(t, err)
	h.Retire("opencode", -time.Second)
	if err := h.OpenManaged("opencode", "root", "job", "token", nil); !errors.Is(err, ErrLifecycleConflict) {
		t.Fatalf("ownership crossed an admitted tokenless request: %v", err)
	}
	h.Apply("opencode", "root", func(s *Session) { s.LastPrompt = "old admission" })
	if got, err := h.Claim("root"); err != nil || len(got) != 1 {
		t.Fatal("tokenless owner could not finish its admitted claim")
	}
	release()
	h.Retire("opencode", -time.Second)
	must(t, h.OpenManaged("opencode", "root", "job", "token", nil))
	if _, release, err := h.BridgeLease(context.Background(), "", "opencode", "root", "", "/api/bridge/poll"); err == nil {
		release()
		t.Fatal("tokenless request crossed managed ownership")
	}
	h.CloseManaged("token")
}

type checkpointCache struct {
	Cache
	writes int
}

func (c *checkpointCache) Put(kind, id string, v any) error {
	if kind == "session" {
		c.writes++
	}
	return c.Cache.Put(kind, id, v)
}

func TestManagedSnapshotCheckpointWithoutNewPrompt(t *testing.T) {
	_, k, cache := newHub(t)
	counted := &checkpointCache{Cache: cache}
	h := New(k, counted)
	must(t, h.OpenManaged("opencode", "root", "job", "token", nil))
	h.Apply("opencode", "root", func(s *Session) { s.LastPrompt = "one prompt"; s.PromptAt = time.Unix(100, 0) })
	update := func(s *Session) {
		s.LastReply = "final answer"
		s.Used = Tokens{In: 321, Out: 123}
		s.CostUSD = 4.2
		s.ActiveAt = time.Unix(200, 0)
	}
	h.Apply("opencode", "root", update)
	writes := counted.writes
	h.Apply("opencode", "root", update)
	if counted.writes != writes {
		t.Fatal("unchanged managed snapshot rewrote durable state")
	}
	restarted := New(k, cache)
	restarted.Retire("opencode", -time.Second)
	must(t, restarted.OpenManaged("opencode", "root", "job2", "token2", nil))
	s := session(t, restarted, "root")
	if s.LastReply != "final answer" || s.Used.Out != 123 || s.Used.In != 321 || s.CostUSD != 4.2 || !s.ActiveAt.Equal(time.Unix(200, 0)) || len(s.Timeline) != 1 {
		t.Fatalf("lost final snapshot: %+v", s)
	}
	restarted.CloseManaged("token2")
	h.CloseManaged("token")
}
