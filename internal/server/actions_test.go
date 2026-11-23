package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/jefflinse/slorp/internal/hub"
)

func TestWhatTheLocalModelHasDoneIsListed(t *testing.T) {
	f := newFixture(t)
	listed := func() []hub.Action {
		t.Helper()
		code, body := f.do(http.MethodGet, "/api/actions", "", nil)
		var out struct {
			Actions []hub.Action `json:"actions"`
		}
		if err := json.Unmarshal([]byte(body), &out); code != http.StatusOK || err != nil || out.Actions == nil {
			t.Fatalf("GET /api/actions = %d %q, want a list", code, body)
		}
		return out.Actions
	}
	if got := listed(); len(got) != 0 {
		t.Errorf("%d actions before the model has done anything", len(got))
	}
	for i := range 7 {
		f.hub.Acted(hub.Action{Kind: hub.ActNote, Outcome: strconv.Itoa(i)})
	}
	if got := listed(); len(got) != 7 || got[0].Outcome != "6" || got[6].Outcome != "0" {
		t.Errorf("listed %+v; want all seven, the latest first", got)
	}
	if got := f.state().Actions; len(got) != 5 || got[0].Outcome != "6" {
		t.Errorf("the state carries %+v; want only the latest five", got)
	}
}
