package enrich

import (
	"strings"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/hub"
)

func TestAskSaysWhenTheModelRanOutOfTokens(t *testing.T) {
	llm := fakeLLM{content: `{"title":"t","summary":"` + strings.Repeat("\n", 80), finish: "length"}
	_, err := llm.enricher(t).describe(ctx, "m", hub.Undescribed{})
	if err == nil || !strings.Contains(err.Error(), "ran out of tokens") {
		t.Fatalf("got %v, want an error saying the model ran out of tokens", err)
	}

	llm = fakeLLM{content: "Sure! Here is a summary.", finish: "stop"}
	_, err = llm.enricher(t).describe(ctx, "m", hub.Undescribed{})
	if err == nil || strings.Contains(err.Error(), "ran out of tokens") {
		t.Fatalf("got %v, want the plain not-JSON error for a model that stopped on its own", err)
	}
}

func TestWorkThatKeepsFailingWaitsLongerEachTime(t *testing.T) {
	e := &Enricher{failures: map[string]int{}}
	for _, tt := range []struct {
		failures int
		want     time.Duration
	}{
		{0, retryAfter},
		{1, retryAfter},
		{2, 2 * retryAfter},
		{3, 4 * retryAfter},
		{5, 16 * retryAfter},
		{40, maxRetryAfter},
	} {
		e.failures["k"] = tt.failures
		if got := e.retryWait("k"); got != tt.want {
			t.Errorf("after %d failures in a row, wait %s; want %s", tt.failures, got, tt.want)
		}
	}
	if got := e.retryWait("never failed"); got != retryAfter {
		t.Errorf("work that never failed waits %s; want %s", got, retryAfter)
	}
}
