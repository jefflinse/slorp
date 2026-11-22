package server

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/hub"
)

func TestHistoryCapabilitiesAndOpenCodeBackfill(t *testing.T) {
	called := 0
	f := fixtureWithHistory(t, map[string]History{"opencode": {Exchanges: func(ctx context.Context, id string, limit int) ([]hub.Exchange, error) {
		called++
		if ctx.Done() == nil || id != "ses_history" || limit != hub.BackfillLimit {
			t.Errorf("history arguments: %q %d", id, limit)
		}
		return []hub.Exchange{{At: time.UnixMilli(1700000000000), Prompt: "Fix it", Reply: "Fixed."}}, nil
	}}})
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
	f = fixtureWithHistory(t, map[string]History{"opencode": {Exchanges: func(context.Context, string, int) ([]hub.Exchange, error) {
		f.hub.Retire("opencode", -time.Second)
		return []hub.Exchange{{At: time.Now(), Prompt: "Lost session", Reply: "Done"}}, nil
	}}})
	f.post("/api/bridge/poll", `{"agent":"opencode","sessionId":"ses_history"}`)
	if code := f.post("/api/sessions/ses_history/backfill", `{}`); code != http.StatusConflict {
		t.Fatalf("closed session = %d", code)
	}
	if len(f.hub.Unfilled()) != 0 {
		t.Fatal("closed session queued for enrichment")
	}
}

func TestExchangesBeforeTheLatestAreReadAsAskedFor(t *testing.T) {
	at := func(n int) time.Time { return time.UnixMilli(1700000000000).Add(time.Duration(n) * time.Minute).UTC() }
	record := []hub.Exchange{
		{At: at(1), Prompt: "one", Reply: "Did one."},
		{At: at(2), Compacted: &hub.Compaction{At: at(2)}},
		{At: at(3), Prompt: "two", Reply: "Did two."},
		{At: at(4), Prompt: "three", Reply: "Did three."},
		{At: at(5), Prompt: "four"},
	}
	f := fixtureWithHistory(t, map[string]History{"opencode": {Exchanges: func(context.Context, string, int) ([]hub.Exchange, error) {
		return record, nil
	}}})
	f.post("/api/bridge/poll", `{"agent":"opencode","sessionId":"ses_history"}`)
	type answer struct {
		Exchanges []struct {
			At            time.Time
			Prompt, Reply string
		}
		Head time.Time
		More bool
	}
	ask := func(body string) (out answer) {
		t.Helper()
		code, got := f.do("POST", "/api/sessions/ses_history/exchanges", body, nil)
		if code != http.StatusOK {
			t.Fatalf("exchanges %s = %d %s", body, code, got)
		}
		if err := json.Unmarshal([]byte(got), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	said := func(a answer) (out []string) {
		for _, x := range a.Exchanges {
			out = append(out, x.Prompt+": "+x.Reply)
		}
		return out
	}
	waiting := func() (out []string) {
		for _, u := range f.hub.Unfilled() {
			for _, x := range u.Exchanges {
				out = append(out, x.Prompt)
			}
		}
		return out
	}

	first := ask(`{"more":1}`)
	if got := said(first); !slices.Equal(got, []string{"three: Did three."}) || !first.Head.Equal(at(5)) || !first.More || !first.Exchanges[0].At.Equal(at(4)) {
		t.Fatalf("first = %+v; want the one exchange before the latest, and word of more", first)
	}
	if got := waiting(); !slices.Equal(got, []string{"three"}) {
		t.Fatalf("waiting = %q; want only the exchange that was read", got)
	}
	from, _ := json.Marshal(at(4))
	second := ask(`{"from":` + string(from) + `,"more":1}`)
	if got := said(second); !slices.Equal(got, []string{"three: Did three.", "two: Did two."}) || !second.More {
		t.Fatalf("second = %+v; want what was shown and one more, the latest first", second)
	}
	if got := waiting(); !slices.Equal(got, []string{"three", "two"}) {
		t.Fatalf("waiting = %q; want the second exchange behind the first", got)
	}

	record = append(record, hub.Exchange{At: at(6), Prompt: "five", Reply: "Did five."})
	from, _ = json.Marshal(at(3))
	again := ask(`{"from":` + string(from) + `}`)
	if got := said(again); !slices.Equal(got, []string{"four: ", "three: Did three.", "two: Did two."}) || !again.Head.Equal(at(6)) || !again.More {
		t.Fatalf("again = %+v; want what was the latest joined to what was shown, and nothing earlier", again)
	}
	if got := waiting(); !slices.Equal(got, []string{"three", "two"}) {
		t.Fatalf("waiting = %q; want nothing queued that was not newly asked for", got)
	}
	if rest := ask(`{"from":` + string(from) + `,"more":9}`); len(rest.Exchanges) != 4 || rest.Exchanges[3].Prompt != "one" || rest.More {
		t.Fatalf("rest = %+v; want everything before the latest, and no word of more", rest)
	}
	if got := f.state().Sessions[0].Timeline; len(got) != 3 {
		t.Errorf("timeline = %+v; want only the three exchanges that were added", got)
	}

	if code := f.post("/api/sessions/nobody/exchanges", `{"more":1}`); code != http.StatusNotFound {
		t.Errorf("an unknown session = %d", code)
	}
	record = nil
	if none := ask(`{"more":1}`); len(none.Exchanges) != 0 || none.More || !none.Head.IsZero() {
		t.Errorf("an empty record = %+v", none)
	}
}
