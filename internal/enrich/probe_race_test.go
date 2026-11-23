package enrich

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/hub"
	"github.com/jefflinse/slorp/internal/testdb"
)

// serving is a model server with one model, which answers only for it.
func serving(t *testing.T, model string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			io.WriteString(w, `{"data":[{"id":"`+model+`"}]}`)
		case "/v1/chat/completions":
			var in struct{ Model string }
			if json.NewDecoder(r.Body).Decode(&in) != nil || in.Model != model {
				http.Error(w, "no such model", http.StatusNotFound)
				return
			}
			io.WriteString(w, `{"choices":[{"message":{"content":"pong"}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestAProbeFinishesWithTheServerItBeganWith(t *testing.T) {
	listing, release := make(chan struct{}), make(chan struct{})
	var armed atomic.Bool
	var listed, released sync.Once
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			if armed.Load() {
				listed.Do(func() { close(listing) })
				<-release
			}
			io.WriteString(w, `{"data":[{"id":"a"}]}`)
		case "/v1/chat/completions":
			var in struct{ Model string }
			if json.NewDecoder(r.Body).Decode(&in) != nil || in.Model != "a" {
				http.Error(w, "no such model", http.StatusNotFound)
				return
			}
			io.WriteString(w, `{"choices":[{"message":{"content":"pong"}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(a.Close)
	t.Cleanup(func() { released.Do(func() { close(release) }) })
	st := testdb.Open(t)
	h := hub.New(testdb.KB(t, st), st)
	choose := func(m hub.ModelSettings) {
		t.Helper()
		if _, err := h.UpdateSettings(func(s *hub.Settings) error { s.Model = m; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	b := hub.ModelSettings{URL: serving(t, "b"), Name: "b"}
	choose(hub.ModelSettings{URL: a.URL, Name: "a"})
	e := &Enricher{Hub: h, Resolve: Startup{}.Config, failed: map[string]time.Time{}}
	e.step(context.Background())

	armed.Store(true)
	probed := make(chan hub.ModelCheck, 1)
	go func() { probed <- e.Probe(context.Background(), nil) }()
	select {
	case <-listing:
	case <-time.After(5 * time.Second):
		t.Fatal("the probe never asked the first server what it offers")
	}
	choose(b)
	e.step(context.Background())
	if e.BaseURL != b.URL {
		t.Fatalf("the worker is on %s, want it moved to %s", e.BaseURL, b.URL)
	}
	released.Do(func() { close(release) })
	select {
	case check := <-probed:
		if check.URL != a.URL || check.Model != "a" || !check.Answered {
			t.Errorf("a probe begun on the first server finished as %s with %q, answered %v, %s", check.URL, check.Model, check.Answered, check.Error)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the probe never finished")
	}
	if check := e.Probe(context.Background(), nil); check.URL != b.URL || check.Model != "b" || !check.Answered {
		t.Errorf("the next probe tried %s with %q, answered %v, %s; want the second server", check.URL, check.Model, check.Answered, check.Error)
	}
}

func TestAProbeUsesOneServerAndItsModelWhileTheWorkerChangesThem(t *testing.T) {
	st := testdb.Open(t)
	h := hub.New(testdb.KB(t, st), st)
	pairs := []hub.ModelSettings{{URL: serving(t, "a"), Name: "a"}, {URL: serving(t, "b"), Name: "b"}}
	choose := func(m hub.ModelSettings) {
		if _, err := h.UpdateSettings(func(s *hub.Settings) error { s.Model = m; return nil }); err != nil {
			t.Error(err)
		}
	}
	choose(pairs[0])
	e := &Enricher{Hub: h, Resolve: Startup{}.Config, failed: map[string]time.Time{}}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := 0; ctx.Err() == nil; i++ {
			choose(pairs[i%2])
			e.step(ctx)
		}
	})
	for range 50 {
		check := e.Probe(context.Background(), nil)
		if !check.Answered || (check.URL == pairs[0].URL) != (check.Model == "a") {
			t.Errorf("a probe tried %s with %q: answered %v, %s", check.URL, check.Model, check.Answered, check.Error)
		}
	}
	cancel()
	wg.Wait()
}
