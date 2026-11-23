package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/hub"
	"github.com/jefflinse/slorp/internal/integrations"
	"github.com/jefflinse/slorp/internal/testdb"
)

// upgrader stands in for the release checker, recording what it was asked.
// Restart runs after the response is flushed, so a test waits on restarted
// before it reads calls.
type upgrader struct {
	mu        sync.Mutex
	calls     []string
	err       error
	restarted chan struct{}
}

func (u *upgrader) Upgrade(context.Context) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls = append(u.calls, "upgrade")
	return u.err
}

func (u *upgrader) Restart() {
	u.mu.Lock()
	u.calls = append(u.calls, "restart")
	u.mu.Unlock()
	u.restarted <- struct{}{}
}

// asked returns what the upgrader was asked, and then forgets it and fails
// any upgrade with err.
func (u *upgrader) asked(err error) []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	calls := u.calls
	u.calls, u.err = nil, err
	return calls
}

func TestUpgradingSlorp(t *testing.T) {
	st := testdb.Open(t)
	k := testdb.KB(t, st)
	u := &upgrader{restarted: make(chan struct{}, 1)}
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

	code, _ := post()
	select {
	case <-u.restarted:
	case <-time.After(5 * time.Second):
		t.Fatal("Slorp was never restarted")
	}
	if calls := u.asked(errors.New("blocked by policy")); code != 204 || !slices.Equal(calls, []string{"upgrade", "restart"}) {
		t.Errorf("upgrade = %d after %v, want 204 and Slorp restarted once upgraded", code, calls)
	}
	code, body := post()
	if calls := u.asked(nil); code != 502 || !strings.Contains(body, "blocked by policy") || !slices.Equal(calls, []string{"upgrade"}) {
		t.Errorf("a failed upgrade = %d %q after %v, want 502 with what went wrong and no restart", code, body, calls)
	}
}

func TestUpgradingSlorpThatCannotUpgradeItself(t *testing.T) {
	if code := newFixture(t).post("/api/upgrade", `{}`); code != 404 {
		t.Errorf("upgrade with no upgrader = %d, want 404", code)
	}
}
