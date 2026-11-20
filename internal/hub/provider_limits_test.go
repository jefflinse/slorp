package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func providerReading(at time.Time, provider string, percent float64) ProviderLimits {
	return ProviderLimits{Provider: provider, Status: "ok", CheckedAt: at, MeasuredAt: at,
		Windows: []ProviderLimitWindow{{Kind: "weekly", Label: "Week", PercentUsed: percent, ResetsAt: at.Add(time.Hour), WindowSeconds: 604800}}}
}

const providerEpoch = "11111111-1111-4111-8111-111111111111"

func applyProviderLimits(h *Hub, root string, reports ...ProviderLimits) {
	h.Apply("opencode", root, func(s *Session) { s.SetProviderLimits(providerEpoch, s.providerLimits.revision+1, reports) })
}

func applyProviderLimitsVersion(h *Hub, root, epoch string, revision int64, reports ...ProviderLimits) {
	h.Apply("opencode", root, func(s *Session) { s.SetProviderLimits(epoch, revision, reports) })
}

func TestProviderLimitsScopedSnapshots(t *testing.T) {
	h, _, _ := newHub(t)
	at := time.Now().UTC().Add(-time.Minute)
	codex := providerReading(at, "openai-codex", 12)
	copilot := providerReading(at, "github-copilot", 34)
	applyProviderLimits(h, "a", codex, copilot)
	applyProviderLimits(h, "b", providerReading(at, "openai-codex", 56))
	h.Rename("a", "My runtime")
	got := h.State().ProviderLimits
	if len(got) != 3 || got[0].SessionID != "a" || got[0].SessionLabel != "My runtime" || got[0].Provider != "github-copilot" || got[0].Windows[0].PercentUsed != 34 || got[1].Windows[0].PercentUsed != 12 || got[2].SessionID != "b" || got[2].Windows[0].PercentUsed != 56 {
		t.Fatalf("scoped readings: %+v", got)
	}
	applyProviderLimits(h, "a")
	if !reflect.DeepEqual(got, h.State().ProviderLimits) {
		t.Fatal("omission changed readings")
	}
	applyProviderLimits(h, "a", []ProviderLimits{}...)
	if got := h.State().ProviderLimits; len(got) != 1 || got[0].SessionID != "b" {
		t.Fatalf("clear affected another root: %+v", got)
	}
	applyProviderLimitsVersion(h, "a", providerEpoch, 1, codex)
	if len(h.State().ProviderLimits) != 1 {
		t.Fatal("delayed report resurrected a cleared reading")
	}
	applyProviderLimits(h, "a", providerReading(at.Add(time.Second), "openai-codex", 0))
	if got := h.State().ProviderLimits; len(got) != 2 || got[0].Windows[0].PercentUsed != 0 {
		t.Fatalf("new reading after clear: %+v", got)
	}
}

func TestProviderLimitsOrderingAndNotifications(t *testing.T) {
	h, _, _ := newHub(t)
	at := time.Now().UTC().Add(-time.Minute)
	p := providerReading(at, "openai-codex", 20)
	applyProviderLimitsVersion(h, "root", providerEpoch, 2, p)
	ticks, cancel := h.Subscribe()
	defer cancel()
	applyProviderLimitsVersion(h, "root", providerEpoch, 2, p)
	older := providerReading(at.Add(-time.Second), p.Provider, 99)
	applyProviderLimitsVersion(h, "root", providerEpoch, 1, older)
	select {
	case <-ticks:
		t.Fatal("unchanged or older poll emitted an event")
	default:
	}
	if h.State().ProviderLimits[0].Windows[0].PercentUsed != 20 {
		t.Fatal("older reading replaced current one")
	}
	p.CheckedAt = at.Add(time.Second)
	applyProviderLimitsVersion(h, "root", providerEpoch, 3, p)
	select {
	case <-ticks:
	default:
		t.Fatal("a new check was not published")
	}
	p = ProviderLimits{Provider: p.Provider, Status: "auth_required", CheckedAt: p.CheckedAt}
	applyProviderLimitsVersion(h, "root", providerEpoch, 4, p)
	got := h.State().ProviderLimits[0]
	if got.Status != "auth_required" || !got.Stale || len(got.Windows) != 0 || !got.MeasuredAt.IsZero() {
		t.Fatalf("new context retained previous quota: %+v", got)
	}
	applyProviderLimitsVersion(h, "root", providerEpoch, 3, older)
	if h.State().ProviderLimits[0].Status != "auth_required" {
		t.Fatal("old context restored")
	}
}

func TestProviderLimitsSnapshotRemovalsFollowRevision(t *testing.T) {
	h, _, _ := newHub(t)
	at := time.Now().Add(-time.Minute)
	first := providerReading(at, "openai-codex", 10)
	second := providerReading(at.Add(time.Second), "github-copilot", 20)
	applyProviderLimitsVersion(h, "root", providerEpoch, 2, first, second)
	applyProviderLimitsVersion(h, "root", providerEpoch, 1, first)
	if len(h.State().ProviderLimits) != 2 {
		t.Fatal("an old snapshot removed a newer provider")
	}
	applyProviderLimitsVersion(h, "root", providerEpoch, 3, first)
	if got := h.State().ProviderLimits; len(got) != 1 || got[0].Provider != first.Provider {
		t.Fatalf("snapshot did not remove absent provider: %+v", got)
	}
	applyProviderLimitsVersion(h, "root", providerEpoch, 2, second)
	if got := h.State().ProviderLimits; len(got) != 1 || got[0].Provider != first.Provider {
		t.Fatalf("old snapshot resurrected removed provider: %+v", got)
	}
}

func TestProviderLimitsFreshness(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name   string
		age    time.Duration
		status string
		reset  time.Time
		stale  bool
		count  int
	}{
		{"fresh", time.Minute, "ok", now.Add(time.Hour), false, 1},
		{"five minute boundary", 5 * time.Minute, "ok", now.Add(time.Hour), false, 1},
		{"stale", 5*time.Minute + time.Millisecond, "ok", now.Add(time.Hour), true, 1},
		{"ten minute boundary", 10 * time.Minute, "ok", now.Add(time.Hour), true, 1},
		{"expired", 10*time.Minute + time.Millisecond, "ok", now.Add(time.Hour), true, 0},
		{"reset passed", time.Minute, "ok", now.Add(-time.Second), true, 0},
		{"reset boundary", time.Minute, "ok", now, true, 0},
		{"failed refresh", time.Minute, "unavailable", now.Add(time.Hour), true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := newHub(t)
			p := providerReading(now.Add(-tc.age), "openai-codex", 25)
			p.CheckedAt, p.Status, p.Windows[0].ResetsAt = now, tc.status, tc.reset
			applyProviderLimits(h, "root", p)
			got := h.providerLimited(now)[0]
			if got.Stale != tc.stale || len(got.Windows) != tc.count || got.Status != tc.status {
				t.Fatalf("freshness: %+v", got)
			}
		})
	}
}

func TestProviderLimitsDetachedAndNotDeduplicated(t *testing.T) {
	h, _, _ := newHub(t)
	p := providerReading(time.Now(), "openai-codex", 25)
	p.Windows = append(p.Windows, p.Windows[0])
	applyProviderLimits(h, "root", p)
	p.Windows[0].PercentUsed = 90
	got := h.State().ProviderLimits
	if len(got[0].Windows) != 2 || got[0].Windows[0].PercentUsed != 25 {
		t.Fatalf("input alias or deduplication: %+v", got)
	}
	got[0].Windows[0].PercentUsed = 90
	if h.State().ProviderLimits[0].Windows[0].PercentUsed != 25 {
		t.Fatal("output aliases stored windows")
	}
}

func TestProviderLimitsRetirementAndPersistence(t *testing.T) {
	for _, managed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unmanaged", true: "managed"}[managed], func(t *testing.T) {
			h, k, cache := newHub(t)
			if managed {
				must(t, h.OpenManaged("opencode", "root", "job", "token", nil))
			}
			p := providerReading(time.Now(), "openai-codex", 42.12345)
			applyProviderLimits(h, "root", p)
			applyProviderLimitsVersion(h, "root", "22222222-2222-4222-8222-222222222222", 10, p)
			h.Rename("root", "Retained session")
			must(t, cache.Each("session", func(_ string, body []byte) {
				for _, forbidden := range []string{"providerLimits", "openai-codex", "42.12345", "percentUsed", providerEpoch, "22222222-2222-4222-8222-222222222222"} {
					if strings.Contains(string(body), forbidden) {
						t.Errorf("persisted quota: %s", body)
					}
				}
			}))
			restarted := New(k, cache)
			applyProviderLimits(restarted, "root")
			if len(restarted.State().ProviderLimits) != 0 {
				t.Fatal("restart restored quotas")
			}
			if managed {
				h.CloseManaged("token")
			} else {
				h.Retire("opencode", -time.Second)
			}
			if len(h.State().ProviderLimits) != 0 {
				t.Fatal("retired quotas still visible")
			}
			if managed {
				must(t, h.OpenManaged("opencode", "root", "job2", "token2", nil))
				defer h.CloseManaged("token2")
			} else {
				applyProviderLimits(h, "root")
			}
			if len(h.State().ProviderLimits) != 0 {
				t.Fatal("reopen restored old account's quotas")
			}
			if got := h.sessions["root"].providerLimits; got.epoch != "" || got.revision != 0 || len(got.retired) != 0 {
				t.Fatalf("reopen retained ordering: %+v", got)
			}
			applyProviderLimitsVersion(h, "root", providerEpoch, 1, p)
			if len(h.State().ProviderLimits) != 1 {
				t.Fatal("new generation could not start at revision one")
			}
		})
	}
}

func TestProviderLimitsRosterAndLegacy(t *testing.T) {
	h, _, _ := newHub(t)
	at := time.Now()
	h.SetLimits("claude", at, []Limit{{Kind: "five_hour", PercentUsed: 35}})
	legacy := h.State().Limits
	applyProviderLimits(h, "root", providerReading(at, "openai-codex", 25))
	h.SetRoster("opencode", []Session{{ID: "root", Agent: "opencode", Title: "Roster name"}})
	if got := h.State().ProviderLimits; len(got) != 1 || got[0].SessionLabel != "Roster name" {
		t.Fatalf("roster lost quotas or label: %+v", got)
	}
	h.SetRoster("opencode", nil)
	h.SetRoster("opencode", []Session{{ID: "root", Agent: "opencode"}})
	if len(h.State().ProviderLimits) != 0 || !reflect.DeepEqual(h.State().Limits, legacy) {
		t.Fatal("roster resurrected quotas or changed legacy limits")
	}
}

func TestProviderLimitsCancelledLeaseDrainsBeforeReopen(t *testing.T) {
	h, _, _ := newHub(t)
	must(t, h.OpenManaged("opencode", "root", "job", "token", nil))
	ctx, release, err := h.BridgeLease(context.Background(), "token", "opencode", "root", "", "/api/bridge/poll")
	must(t, err)
	closed := make(chan struct{})
	go func() { h.CloseManaged("token"); close(closed) }()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		release()
		t.Fatal("lease not cancelled")
	}
	applyProviderLimits(h, "root", providerReading(time.Now(), "openai-codex", 10))
	release()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("close did not drain")
	}
	must(t, h.OpenManaged("opencode", "root", "job2", "token2", nil))
	defer h.CloseManaged("token2")
	if len(h.State().ProviderLimits) != 0 {
		t.Fatal("in-flight reading leaked into replacement generation")
	}
}

func TestProviderLimitsJSONContract(t *testing.T) {
	h, _, _ := newHub(t)
	p := providerReading(time.Now().UTC().Truncate(time.Millisecond), "openai-codex", 0)
	applyProviderLimits(h, "root", p)
	b, err := json.Marshal(h.State())
	must(t, err)
	var state map[string]json.RawMessage
	must(t, json.Unmarshal(b, &state))
	var reports []map[string]any
	must(t, json.Unmarshal(state["providerLimits"], &reports))
	for _, key := range []string{"checkedAt", "measuredAt"} {
		if reports[0][key] != p.CheckedAt.Format(time.RFC3339Nano) {
			t.Fatalf("%s not ISO: %s", key, state["providerLimits"])
		}
	}
	if _, ok := reports[0]["retryAt"]; ok {
		t.Fatal("absent retryAt serialized")
	}
	if reports[0]["stale"] != false || reports[0]["sessionId"] != "root" {
		t.Fatalf("contract: %s", state["providerLimits"])
	}
}

type providerLimitCache struct {
	Cache
	writes int
}

func (c *providerLimitCache) Put(kind, id string, v any) error {
	c.writes++
	return c.Cache.Put(kind, id, v)
}

func TestProviderLimitsDoNotWriteCache(t *testing.T) {
	_, k, cache := newHub(t)
	counted := &providerLimitCache{Cache: cache}
	h := New(k, counted)
	must(t, h.OpenManaged("opencode", "root", "job", "token", nil))
	defer h.CloseManaged("token")
	writes := counted.writes
	p := providerReading(time.Now(), "openai-codex", 25)
	applyProviderLimits(h, "root", p)
	p.CheckedAt = p.CheckedAt.Add(time.Second)
	p.Windows[0].PercentUsed = 50
	applyProviderLimits(h, "root", p)
	applyProviderLimits(h, "root", []ProviderLimits{}...)
	if counted.writes != writes {
		t.Fatalf("quota-only updates wrote cache: %d -> %d", writes, counted.writes)
	}
	ticks, cancel := h.Subscribe()
	defer cancel()
	applyProviderLimits(h, "root", []ProviderLimits{}...)
	select {
	case <-ticks:
		t.Fatal("repeated clear emitted an event")
	default:
	}
}

func TestProviderLimitsConcurrentSnapshots(t *testing.T) {
	h, _, _ := newHub(t)
	at := time.Now().Add(-time.Minute)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			applyProviderLimitsVersion(h, "root", providerEpoch, int64(i+1), providerReading(at, "openai-codex", float64(i)))
			_ = h.State()
		}()
	}
	wg.Wait()
	if got := h.State().ProviderLimits; len(got) != 1 || got[0].Windows[0].PercentUsed != 19 {
		t.Fatalf("concurrent older update won: %+v", got)
	}
}

func TestProviderLimitsCachedMemberRemoval(t *testing.T) {
	h, _, _ := newHub(t)
	at := time.Now().Add(-time.Minute)
	a := ProviderLimits{Provider: "openai-codex", Status: "unsupported", CheckedAt: at}
	b := providerReading(at.Add(time.Second), "github-copilot", 10)
	applyProviderLimits(h, "root", a, b)
	applyProviderLimits(h, "root", a)
	if got := h.State().ProviderLimits; len(got) != 1 || got[0].Provider != a.Provider {
		t.Fatalf("removed provider survived cached remaining report: %+v", got)
	}
}

func TestProviderLimitsOldEmptySnapshotIgnored(t *testing.T) {
	h, _, _ := newHub(t)
	p := providerReading(time.Now(), "openai-codex", 10)
	applyProviderLimitsVersion(h, "root", providerEpoch, 2, []ProviderLimits{}...)
	applyProviderLimitsVersion(h, "root", providerEpoch, 3, p)
	ticks, cancel := h.Subscribe()
	defer cancel()
	for _, revision := range []int64{1, 2, 3} {
		applyProviderLimitsVersion(h, "root", providerEpoch, revision, []ProviderLimits{}...)
	}
	if len(h.State().ProviderLimits) != 1 {
		t.Fatal("old or equal revision empty snapshot cleared a newer reading")
	}
	select {
	case <-ticks:
		t.Fatal("ignored snapshot emitted an event")
	default:
	}
	applyProviderLimitsVersion(h, "root", providerEpoch, 4, []ProviderLimits{}...)
	applyProviderLimitsVersion(h, "root", providerEpoch, 3, p)
	if len(h.State().ProviderLimits) != 0 {
		t.Fatal("old reading resurrected after clear")
	}
}

func TestProviderLimitsEpochRestartAndBoundedRetirement(t *testing.T) {
	h, _, _ := newHub(t)
	p := providerReading(time.Now(), "openai-codex", 10)
	applyProviderLimitsVersion(h, "root", providerEpoch, 100, p)
	for i := 1; i <= 20; i++ {
		epoch := fmt.Sprintf("%08x-1111-4111-8111-111111111111", i)
		previous := h.sessions["root"].providerLimits.epoch
		p.Windows[0].PercentUsed = float64(i)
		applyProviderLimitsVersion(h, "root", epoch, 1, p)
		got := h.sessions["root"].providerLimits
		if got.epoch != epoch || got.revision != 1 || len(got.retired) != min(i, retiredProviderLimitEpochs) {
			t.Fatalf("epoch reset/retirement bound: %+v", got)
		}
		applyProviderLimitsVersion(h, "root", previous, 1000, []ProviderLimits{}...)
		for _, retired := range got.retired {
			applyProviderLimitsVersion(h, "root", retired, 1000, p)
		}
		if got := h.State().ProviderLimits; len(got) != 1 || got[0].Windows[0].PercentUsed != float64(i) || h.sessions["root"].providerLimits.epoch != epoch {
			t.Fatalf("superseded collector accepted: %+v", got)
		}
	}
	got := h.sessions["root"].providerLimits.retired
	if got[0] != "00000004-1111-4111-8111-111111111111" || got[15] != "00000013-1111-4111-8111-111111111111" {
		t.Fatalf("retained epochs not the most recent 16: %v", got)
	}
	applyProviderLimitsVersion(h, "root", "ffffffff-1111-4111-8111-111111111111", 1, []ProviderLimits{}...)
	if len(h.State().ProviderLimits) != 0 {
		t.Fatal("new empty epoch did not clear previous collector's readings")
	}
}
