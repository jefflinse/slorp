package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/hub"
)

func TestBridgeSnapshotRestoresStateWithoutReplayingTurns(t *testing.T) {
	f := newFixture(t)
	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	cost := 0.25
	b := bridgeSnapshot{
		Status: hub.Working, StartedAt: at.Add(-time.Minute).UnixMilli(), ActiveAt: at.UnixMilli(), PromptAt: at.UnixMilli(),
		LastPrompt: "Build it", LastReply: "", Model: "openai/test", Reasoning: "high", ContextTokens: 90, ContextWindow: 100,
		Used: &hub.Tokens{In: 10, Out: 20, CacheRead: 30, CacheWrite: 40}, CostUSD: &cost,
	}
	poll := func() {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"agent": "opencode", "sessionId": "ses_1", "snapshot": b})
		if code := f.post("/api/bridge/poll", string(body)); code != 200 {
			t.Fatalf("poll = %d", code)
		}
	}
	poll()
	poll()
	s := f.state().Sessions[0]
	if s.Model != b.Model || s.Reasoning != b.Reasoning || s.ContextTokens != 90 || s.ContextWindow != 100 || s.Used != *b.Used || s.CostUSD != cost {
		t.Fatalf("telemetry = %+v", s)
	}
	if s.Status != hub.Working || len(s.Timeline) != 1 || !s.Timeline[0].At.Equal(at) || !s.ActiveAt.Equal(at) {
		t.Fatalf("restored state = %+v", s)
	}

	p := f.hub.Ask("opencode", s.ID, hub.Pending{Tool: "bash", Input: "ls"}, true)
	poll()
	if s := f.state().Sessions[0]; s.Status != hub.Waiting || s.Pending == nil || s.Pending.ID != p.ID {
		t.Fatalf("a poll erased a pending prompt: %+v", s)
	}
	f.hub.Done(p.ID)
	b.Compacted = &hub.Compaction{At: at.Add(time.Minute), Auto: true, Before: 90}
	b.ContextTokens, b.Status, b.LastReply = 0, hub.Failed, ""
	b.WaitingFor = "provider unavailable"
	poll()
	poll()
	s = f.state().Sessions[0]
	if s.Status != hub.Failed || s.WaitingFor != b.WaitingFor || s.ContextTokens != 0 || len(s.Timeline) != 2 || s.Timeline[1].Compacted == nil {
		t.Fatalf("compaction and failure = %+v", s)
	}
	if code := f.post("/api/bridge/poll", `{"agent":"opencode","sessionId":"ses_1"}`); code != 200 {
		t.Fatal(code)
	}
	if s := f.state().Sessions[0]; s.Model != b.Model || s.Used != *b.Used {
		t.Fatalf("an older bridge erased telemetry: %+v", s)
	}
	zero := 0.0
	b.CostUSD, b.Used, b.LastReply, b.Status = &zero, &hub.Tokens{}, "", hub.Idle
	poll()
	if s := f.state().Sessions[0]; s.CostUSD != 0 || s.Used != (hub.Tokens{}) || s.LastReply != "" {
		t.Fatalf("zero readings were not applied: %+v", s)
	}
}

func TestOrderedQuestionsOverHTTP(t *testing.T) {
	f := newFixture(t)
	input := `{"questions":[{"question":"Choose","multiSelect":true,"options":[{"label":"A, B"},{"label":"C"}]},{"question":"Choose","custom":true,"options":[]}]}`
	request, _ := json.Marshal(map[string]any{"agent": "opencode", "sessionId": "ses_1", "tool": "question", "question": true, "ordered": true, "canAnswer": true, "input": input})
	_, body := f.do("POST", "/api/bridge/permission", string(request), nil)
	var asked struct {
		ID         string
		Answerable bool
	}
	if json.Unmarshal([]byte(body), &asked) != nil || !asked.Answerable {
		t.Fatalf("question refused: %s", body)
	}
	if code := f.post("/api/permissions/"+asked.ID, `{"selections":[["wrong"],["custom"]]}`); code != 409 {
		t.Fatalf("invalid selection = %d", code)
	}
	if code := f.post("/api/permissions/"+asked.ID, `{"selections":[["A, B","C"],["custom"]]}`); code != 204 {
		t.Fatalf("valid selections = %d", code)
	}
	_, body = f.do("POST", "/api/bridge/permission/wait", `{"id":"`+asked.ID+`"}`, nil)
	if strings.TrimSpace(body) != `{"state":"allow","selections":[["A, B","C"],["custom"]]}` {
		t.Fatalf("lossy question answer: %s", body)
	}
}

func TestPluginManagementDoesNotRequireSessionLaunching(t *testing.T) {
	plugin := &launcher{}
	f := newFixture(t, map[string]PluginManager{"opencode": plugin})
	if code := f.post("/api/plugins/opencode", `{}`); code != 204 {
		t.Fatal(code)
	}
	if code := f.post("/api/plugins/opencode/update", `{}`); code != 204 {
		t.Fatal(code)
	}
	if code, _ := f.do("DELETE", "/api/plugins/opencode", "", nil); code != 204 {
		t.Fatal(code)
	}
	if got := strings.Join(plugin.calls, ","); got != "install,update,uninstall" {
		t.Fatal(got)
	}
	if code := f.post("/api/sessions", `{"agent":"opencode","prompt":"hello","dir":"/tmp"}`); code != 400 {
		t.Fatal("unexpectedly enabled launching:", code)
	}
}
