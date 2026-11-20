package enrich

import (
	"strings"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/hub"
	"github.com/jefflinse/slorp/internal/integrations"
	"github.com/jefflinse/slorp/internal/testdb"
)

func TestAskSaysWhenTheModelRanOutOfTokens(t *testing.T) {
	llm := fakeLLM{content: `{"title":"t","summary":"private session content`, finish: "length"}
	_, err := llm.enricher(t).describe(ctx, "m", hub.Undescribed{})
	if err == nil || !strings.Contains(err.Error(), "ran out of tokens") {
		t.Fatalf("got %v, want an error saying the model ran out of tokens", err)
	}
	if strings.Contains(err.Error(), "private session content") {
		t.Error("the error included part of the model response")
	}

	llm = fakeLLM{content: "Sure! Here is a summary.", finish: "stop"}
	_, err = llm.enricher(t).describe(ctx, "m", hub.Undescribed{})
	if err == nil || strings.Contains(err.Error(), "ran out of tokens") {
		t.Fatalf("got %v, want the plain not-JSON error for a model that stopped on its own", err)
	}
}

func TestBriefRetryWaitsAndClearsOnSuccess(t *testing.T) {
	f := &fakeLLM{listed: `{"data":[{"id":"m"}]}`, content: `{"stands":"partial`, finish: "length"}
	e := f.enricher(t)
	st := testdb.Open(t)
	e.Hub = hub.New(testdb.KB(t, st), st)
	e.Hub.SetFeed("github", nil, []integrations.PullRequest{{Repo: "acme/widgets", Number: 1, At: time.Now(), Mine: true}}, nil)
	refs := e.Hub.Undiscussed(1)
	if len(refs) != 1 {
		t.Fatalf("expected one PR to discuss, got %v", refs)
	}
	e.Hub.SetDiscussion(refs[0], integrations.Discussion{}, nil)
	key, _, job := e.next()
	if job == nil {
		t.Fatal("expected a brief to be ready")
	}

	for i, minutes := range []time.Duration{1, 2, 4, 8, 16, 32, 64, 128, 256, 360, 360} {
		e.step(ctx)
		state := e.retries[key]
		if want := minutes * time.Minute; state.wait != want {
			t.Fatalf("after %d failures, wait %s, want %s", i+1, state.wait, want)
		}
		e.step(ctx)
		if f.requests != i+1 {
			t.Fatalf("got %d requests after %d failures, want no retry before the delay", f.requests, i+1)
		}
		state.readyAt = time.Now().Add(-time.Second)
		e.retries[key] = state
	}

	f.content, f.finish = `{"stands":"Ready for review.","next":"The reviewer goes next.","points":[]}`, "stop"
	e.step(ctx)
	if _, ok := e.retries[key]; ok || len(e.Hub.Unbriefed()) != 0 {
		t.Fatal("a successful brief should finish the job and clear its retry state")
	}
}
