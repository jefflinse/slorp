package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/jefflinse/slorp/internal/hub"
	"github.com/jefflinse/slorp/internal/integrations"
	"github.com/jefflinse/slorp/internal/testdb"
)

// upgrader stands in for the release checker, recording what it was asked.
type upgrader struct {
	calls []string
	err   error
}

func (u *upgrader) Upgrade(context.Context) error {
	u.calls = append(u.calls, "upgrade")
	return u.err
}

func (u *upgrader) Restart() { u.calls = append(u.calls, "restart") }

func TestUpgradingSlorp(t *testing.T) {
	st := testdb.Open(t)
	k := testdb.KB(t, st)
	u := &upgrader{}
	srv := httptest.NewServer(New(hub.New(k, st), k, integrations.NewManager(func() map[string]bool { return nil }), &asked{}, nil, nil, nil, nil, nil, nil, u, Page(nil, "")))
	t.Cleanup(srv.Close)
	post := func() (int, string) {
		res, err := http.Post(srv.URL+"/api/upgrade", "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(body)
	}

	if code, _ := post(); code != 204 || !slices.Equal(u.calls, []string{"upgrade", "restart"}) {
		t.Errorf("upgrade = %d after %v, want 204 and Slorp restarted once upgraded", code, u.calls)
	}
	u.calls, u.err = nil, errors.New("blocked by policy")
	if code, body := post(); code != 502 || !strings.Contains(body, "blocked by policy") || !slices.Equal(u.calls, []string{"upgrade"}) {
		t.Errorf("a failed upgrade = %d %q after %v, want 502 with what went wrong and no restart", code, body, u.calls)
	}
}

func TestUpgradingSlorpThatCannotUpgradeItself(t *testing.T) {
	if code := newFixture(t).post("/api/upgrade", `{}`); code != 404 {
		t.Errorf("upgrade with no upgrader = %d, want 404", code)
	}
}
