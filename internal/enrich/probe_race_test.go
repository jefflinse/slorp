package enrich

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
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
