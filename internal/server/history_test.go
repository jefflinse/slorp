package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/hub"
)

func TestHistoryCapabilitiesAndOpenCodeBackfill(t *testing.T) {
	called := 0
	f := fixtureWithHistory(t, map[string]History{"opencode": func(ctx context.Context, id string, limit int) ([]hub.Exchange, error) {
		called++
		if ctx.Done() == nil || id != "ses_history" || limit != hub.BackfillLimit {
			t.Errorf("history arguments: %q %d", id, limit)
		}
		return []hub.Exchange{{At: time.UnixMilli(1700000000000), Prompt: "Fix it", Reply: "Fixed."}}, nil
	}})
	_, body := f.do("GET", "/api/sessions", "", nil)
	var state struct {
		Capabilities map[string]agentCapabilities `json:"capabilities"`
	}
	if err := json.Unmarshal([]byte(body), &state); err != nil {
		t.Fatal(err)
	}
	if !state.Capabilities["opencode"].History || !state.Capabilities["claude"].History || state.Capabilities["unknown"].History {
		t.Fatal(state.Capabilities)
	}
	f.post("/api/bridge/poll", `{"agent":"opencode","sessionId":"ses_history"}`)
	if code, body := f.do("POST", "/api/sessions/ses_history/backfill", `{}`, nil); code != http.StatusAccepted || !strings.Contains(body, `"queued":1`) {
		t.Fatalf("backfill = %d %s", code, body)
	}
	if called != 1 || len(f.state().Sessions[0].Timeline) != 1 {
		t.Fatal("history did not reach the hub")
	}
}

func TestHistoryDoesNotQueueForASessionThatClosesDuringRead(t *testing.T) {
	var f *fixture
	f = fixtureWithHistory(t, map[string]History{"opencode": func(context.Context, string, int) ([]hub.Exchange, error) {
		f.hub.Retire("opencode", -time.Second)
		return []hub.Exchange{{At: time.Now(), Prompt: "Lost session", Reply: "Done"}}, nil
	}})
	f.post("/api/bridge/poll", `{"agent":"opencode","sessionId":"ses_history"}`)
	if code := f.post("/api/sessions/ses_history/backfill", `{}`); code != http.StatusConflict {
		t.Fatalf("closed session = %d", code)
	}
	if len(f.hub.Unfilled()) != 0 {
		t.Fatal("closed session queued for enrichment")
	}
}
