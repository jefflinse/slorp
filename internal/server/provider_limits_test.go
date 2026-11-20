package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/hub"
)

func providerReportJSON(at time.Time) map[string]any {
	return map[string]any{
		"provider": "openai-codex", "status": "ok", "checkedAt": at.UnixMilli(), "measuredAt": at.UnixMilli(),
		"windows": []any{map[string]any{"kind": "weekly", "label": "Week", "percentUsed": 0, "resetsAt": at.Add(time.Hour).UTC().Format(time.RFC3339), "windowSeconds": 604800}},
	}
}

const providerEpoch = "11111111-1111-4111-8111-111111111111"

func providerPollJSON(t *testing.T, root string, reports any) string {
	t.Helper()
	return providerVersionedPollJSON(t, root, providerEpoch, 1, reports)
}

func providerVersionedPollJSON(t *testing.T, root, epoch string, revision int64, reports any) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"agent": "opencode", "sessionId": root, "title": "Native title", "providerLimits": reports, "providerLimitsEpoch": epoch, "providerLimitsRevision": revision})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestProviderLimitsRejectBeforeMutation(t *testing.T) {
	at := time.Now().UTC().Add(-time.Second).Truncate(time.Millisecond)
	window := func(p map[string]any) map[string]any { return p["windows"].([]any)[0].(map[string]any) }
	tests := map[string]func(map[string]any) any{
		"null array":              func(p map[string]any) any { return nil },
		"object instead of array": func(p map[string]any) any { return p },
		"null report":             func(p map[string]any) any { return []any{nil} },
		"unknown provider":        func(p map[string]any) any { p["provider"] = "anthropic"; return []any{p} },
		"missing provider":        func(p map[string]any) any { delete(p, "provider"); return []any{p} },
		"unknown status":          func(p map[string]any) any { p["status"] = "error"; return []any{p} },
		"missing status":          func(p map[string]any) any { delete(p, "status"); return []any{p} },
		"missing checked":         func(p map[string]any) any { delete(p, "checkedAt"); return []any{p} },
		"null checked":            func(p map[string]any) any { p["checkedAt"] = nil; return []any{p} },
		"zero checked":            func(p map[string]any) any { p["checkedAt"] = 0; return []any{p} },
		"negative checked":        func(p map[string]any) any { p["checkedAt"] = -1; return []any{p} },
		"fractional checked":      func(p map[string]any) any { p["checkedAt"] = 1.5; return []any{p} },
		"checked overflow":        func(p map[string]any) any { p["checkedAt"] = 1e30; return []any{p} },
		"future checked":          func(p map[string]any) any { p["checkedAt"] = at.Add(2 * time.Minute).UnixMilli(); return []any{p} },
		"future measured":         func(p map[string]any) any { p["measuredAt"] = at.Add(2 * time.Minute).UnixMilli(); return []any{p} },
		"future retry": func(p map[string]any) any {
			p["status"] = "throttled"
			p["retryAt"] = at.Add(25 * time.Hour).UnixMilli()
			return []any{p}
		},
		"measured after checked": func(p map[string]any) any { p["measuredAt"] = at.Add(time.Millisecond).UnixMilli(); return []any{p} },
		"missing measured":       func(p map[string]any) any { delete(p, "measuredAt"); return []any{p} },
		"null measured":          func(p map[string]any) any { p["measuredAt"] = nil; return []any{p} },
		"zero measured":          func(p map[string]any) any { p["measuredAt"] = 0; return []any{p} },
		"missing windows":        func(p map[string]any) any { delete(p, "windows"); return []any{p} },
		"null windows":           func(p map[string]any) any { p["windows"] = nil; return []any{p} },
		"null window":            func(p map[string]any) any { p["windows"] = []any{nil}; return []any{p} },
		"missing percent":        func(p map[string]any) any { delete(window(p), "percentUsed"); return []any{p} },
		"null percent":           func(p map[string]any) any { window(p)["percentUsed"] = nil; return []any{p} },
		"negative percent":       func(p map[string]any) any { window(p)["percentUsed"] = -0.01; return []any{p} },
		"overfull percent":       func(p map[string]any) any { window(p)["percentUsed"] = 100.01; return []any{p} },
		"overflow percent":       func(p map[string]any) any { window(p)["percentUsed"] = json.RawMessage("1e999"); return []any{p} },
		"string percent":         func(p map[string]any) any { window(p)["percentUsed"] = "NaN"; return []any{p} },
		"bad date":               func(p map[string]any) any { window(p)["resetsAt"] = "2026-02-30T12:00:00Z"; return []any{p} },
		"missing date":           func(p map[string]any) any { delete(window(p), "resetsAt"); return []any{p} },
		"null date":              func(p map[string]any) any { window(p)["resetsAt"] = nil; return []any{p} },
		"zero date":              func(p map[string]any) any { window(p)["resetsAt"] = "0001-01-01T00:00:00Z"; return []any{p} },
		"bad time":               func(p map[string]any) any { window(p)["resetsAt"] = "2026-11-20T25:00:00Z"; return []any{p} },
		"bad zone hour":          func(p map[string]any) any { window(p)["resetsAt"] = "2026-11-20T12:00:00+24:00"; return []any{p} },
		"bad zone minute":        func(p map[string]any) any { window(p)["resetsAt"] = "2026-11-20T12:00:00+00:60"; return []any{p} },
		"missing kind":           func(p map[string]any) any { delete(window(p), "kind"); return []any{p} },
		"missing label":          func(p map[string]any) any { delete(window(p), "label"); return []any{p} },
		"blank label":            func(p map[string]any) any { window(p)["label"] = "  "; return []any{p} },
		"long label":             func(p map[string]any) any { window(p)["label"] = strings.Repeat("界", 101); return []any{p} },
		"long kind":              func(p map[string]any) any { window(p)["kind"] = strings.Repeat("x", 101); return []any{p} },
		"long date": func(p map[string]any) any {
			window(p)["resetsAt"] = "2026-11-20T12:00:00." + strings.Repeat("0", 100) + "Z"
			return []any{p}
		},
		"null seconds":       func(p map[string]any) any { window(p)["windowSeconds"] = nil; return []any{p} },
		"zero seconds":       func(p map[string]any) any { window(p)["windowSeconds"] = 0; return []any{p} },
		"fractional seconds": func(p map[string]any) any { window(p)["windowSeconds"] = 1.5; return []any{p} },
		"negative seconds":   func(p map[string]any) any { window(p)["windowSeconds"] = -1; return []any{p} },
		"too many windows": func(p map[string]any) any {
			w := window(p)
			p["windows"] = []any{w, w, w, w, w, w, w, w, w}
			return []any{p}
		},
		"too many reports":                      func(p map[string]any) any { return []any{p, p, p, p} },
		"duplicate provider":                    func(p map[string]any) any { return []any{p, p} },
		"unsupported gauge":                     func(p map[string]any) any { p["status"] = "unsupported"; return []any{p} },
		"auth measured":                         func(p map[string]any) any { p["status"] = "auth_required"; p["windows"] = []any{}; return []any{p} },
		"unavailable gauge without measurement": func(p map[string]any) any { p["status"] = "unavailable"; delete(p, "measuredAt"); return []any{p} },
		"null retry":                            func(p map[string]any) any { p["retryAt"] = nil; return []any{p} },
		"raw error":                             func(p map[string]any) any { p["error"] = "secret"; return []any{p} },
		"raw auth":                              func(p map[string]any) any { p["token"] = "secret"; return []any{p} },
		"input label":                           func(p map[string]any) any { p["sessionLabel"] = "untrusted"; return []any{p} },
		"input root":                            func(p map[string]any) any { p["sessionId"] = "other"; return []any{p} },
		"invalid second report":                 func(p map[string]any) any { return []any{p, map[string]any{"provider": "unknown"}} },
	}
	f := newFixture(t)
	if code := f.post("/api/bridge/poll", providerPollJSON(t, "root", []any{providerReportJSON(at)})); code != 200 {
		t.Fatal(code)
	}
	if _, ok := f.hub.Enqueue("root", "keep queued"); !ok {
		t.Fatal("enqueue failed")
	}
	before := f.state()
	for name, invalid := range tests {
		t.Run(name, func(t *testing.T) {
			body := providerPollJSON(t, "root", invalid(providerReportJSON(at)))
			body = strings.Replace(body, "Native title", "must not mutate", 1)
			if code := f.post("/api/bridge/poll", body); code != http.StatusBadRequest {
				t.Fatalf("invalid report = %d: %s", code, body)
			}
			got := f.state()
			if !reflect.DeepEqual(before.ProviderLimits, got.ProviderLimits) || got.Sessions[0].Title != before.Sessions[0].Title || got.Sessions[0].Replies[0].State != hub.Queued {
				t.Fatalf("invalid request mutated state or claimed reply: %+v", got.Sessions)
			}
			body = strings.Replace(body, `"sessionId":"root"`, `"sessionId":"new-root"`, 1)
			if code := f.post("/api/bridge/poll", body); code != http.StatusBadRequest || len(f.state().Sessions) != 1 {
				t.Fatalf("invalid request created root: %d", code)
			}
		})
	}
}

func TestProviderLimitsPollContract(t *testing.T) {
	f := newFixture(t)
	at := time.Now().UTC().Add(-time.Second).Truncate(time.Millisecond)
	for i, provider := range []string{"openai-codex", "github-copilot", "opencode-go"} {
		p := providerReportJSON(at)
		p["provider"] = provider
		if code := f.post("/api/bridge/poll", providerPollJSON(t, string(rune('a'+i)), []any{p})); code != 200 {
			t.Fatal(code)
		}
	}
	f.hub.Rename("a", "Hub name")
	got := f.state().ProviderLimits
	if len(got) != 3 || got[0].SessionID != "a" || got[0].SessionLabel != "Hub name" || got[0].Windows[0].PercentUsed != 0 || got[0].Stale || !got[0].CheckedAt.Equal(at) {
		t.Fatalf("contract: %+v", got)
	}
	if code := f.post("/api/bridge/poll", `{"agent":"opencode","sessionId":"a"}`); code != 200 || !reflect.DeepEqual(got, f.state().ProviderLimits) {
		t.Fatal("legacy omission erased limits")
	}
	if code := f.post("/api/bridge/poll", providerVersionedPollJSON(t, "a", providerEpoch, 2, []any{})); code != 200 || len(f.state().ProviderLimits) != 2 {
		t.Fatal("empty array did not clear only this root")
	}
}

func TestProviderLimitsStatusRowsAndExpiredReadings(t *testing.T) {
	f := newFixture(t)
	at := time.Now().UTC().Truncate(time.Millisecond)
	for _, status := range []string{"unsupported", "auth_required", "unavailable", "throttled"} {
		p := map[string]any{"provider": "openai-codex", "status": status, "checkedAt": at.UnixMilli(), "windows": []any{}}
		if status == "throttled" {
			p["retryAt"] = at.Add(30 * time.Second).UnixMilli()
		}
		if code := f.post("/api/bridge/poll", providerPollJSON(t, status, []any{p})); code != 200 {
			t.Fatal(status, code)
		}
	}
	for _, p := range f.state().ProviderLimits {
		if !p.Stale || len(p.Windows) != 0 || !p.MeasuredAt.IsZero() {
			t.Fatalf("status fabricated a gauge: %+v", p)
		}
		if p.Status == "throttled" && !p.RetryAt.Equal(at.Add(30*time.Second)) {
			t.Fatal("retryAt was not preserved")
		}
	}
	p := providerReportJSON(at.Add(-11 * time.Minute))
	p["checkedAt"] = at.UnixMilli()
	if code := f.post("/api/bridge/poll", providerPollJSON(t, "old", []any{p})); code != 200 {
		t.Fatal(code)
	}
	for _, p := range f.state().ProviderLimits {
		if p.SessionID == "old" && (p.Status != "ok" || !p.Stale || len(p.Windows) != 0) {
			t.Fatalf("reread refreshed old measurement: %+v", p)
		}
	}
}

func TestProviderLimitsManagedAdmission(t *testing.T) {
	f := newFixture(t)
	if err := f.hub.OpenManaged("opencode", "root", "job", "token", nil); err != nil {
		t.Fatal(err)
	}
	body := providerPollJSON(t, "root", []any{providerReportJSON(time.Now())})
	for _, token := range []string{"", "wrong"} {
		code, _ := f.do("POST", "/api/bridge/poll", body, func(r *http.Request) { r.Header.Set("X-Slorp-Run", token) })
		if code != 409 || len(f.state().ProviderLimits) != 0 {
			t.Fatal("unfenced report accepted", code)
		}
	}
	code, _ := f.do("POST", "/api/bridge/poll", body, func(r *http.Request) { r.Header.Set("X-Slorp-Run", "token") })
	if code != 200 || len(f.state().ProviderLimits) != 1 {
		t.Fatal("managed report not accepted", code)
	}
	f.hub.CloseManaged("token")
	if err := f.hub.OpenManaged("opencode", "root", "job2", "token2", nil); err != nil {
		t.Fatal(err)
	}
	defer f.hub.CloseManaged("token2")
	code, _ = f.do("POST", "/api/bridge/poll", body, func(r *http.Request) { r.Header.Set("X-Slorp-Run", "token") })
	if code != 409 || len(f.state().ProviderLimits) != 0 {
		t.Fatal("old generation leaked quotas into reopen", code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest("POST", f.srv.URL+"/api/bridge/poll", strings.NewReader(body)).WithContext(ctx)
	r.Header.Set("X-Slorp-Run", "token2")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.srv.Config.Handler.ServeHTTP(w, r)
	if w.Code != 409 || len(f.state().ProviderLimits) != 0 {
		t.Fatal("cancelled poll mutated quotas", w.Code)
	}
}

func TestProviderLimitTimestampSkewBoundary(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	for _, offset := range []time.Duration{time.Minute, time.Minute + time.Millisecond} {
		p := providerReportJSON(now.Add(offset))
		b, err := json.Marshal([]any{p})
		if err != nil {
			t.Fatal(err)
		}
		_, err = parseProviderLimits(b, now)
		if (err == nil) != (offset == time.Minute) {
			t.Fatalf("future skew %s: %v", offset, err)
		}
	}
}

func TestProviderLimitsAcceptedBounds(t *testing.T) {
	f := newFixture(t)
	at := time.Now().Add(-time.Minute)
	var reports []any
	for _, provider := range []string{"openai-codex", "github-copilot", "opencode-go"} {
		p := providerReportJSON(at)
		p["provider"] = provider
		windows := make([]any, 8)
		for i := range windows {
			windows[i] = map[string]any{"kind": strings.Repeat("k", 100), "label": strings.Repeat("界", 100), "percentUsed": i % 2 * 100, "resetsAt": at.Add(time.Hour).Format(time.RFC3339Nano)}
		}
		p["windows"] = windows
		reports = append(reports, p)
	}
	if code := f.post("/api/bridge/poll", providerPollJSON(t, "root", reports)); code != 200 {
		t.Fatal(code)
	}
	got := f.state().ProviderLimits
	if len(got) != 3 {
		t.Fatalf("reports lost: %+v", got)
	}
	for _, p := range got {
		if len(p.Windows) != 8 || p.Windows[0].PercentUsed != 0 || p.Windows[1].PercentUsed != 100 || p.Windows[0].WindowSeconds != 0 {
			t.Fatalf("boundary windows changed: %+v", p)
		}
	}
}

func TestProviderLimitsBackoffAllowsLongRetry(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	for _, delay := range []time.Duration{time.Minute, 15 * time.Minute, time.Hour, 24 * time.Hour, 24*time.Hour + time.Millisecond} {
		t.Run(delay.String(), func(t *testing.T) {
			p := providerReportJSON(now)
			p["status"], p["retryAt"] = "throttled", now.Add(delay).UnixMilli()
			raw, err := json.Marshal([]any{p})
			if err != nil {
				t.Fatal(err)
			}
			_, err = parseProviderLimits(raw, now)
			if (err == nil) != (delay <= 24*time.Hour) {
				t.Fatalf("retry after %s: %v", delay, err)
			}
		})
	}
}

func TestProviderLimitsVersionValidationBeforeMutation(t *testing.T) {
	f := newFixture(t)
	reports := []any{providerReportJSON(time.Now().Add(-time.Second))}
	if code := f.post("/api/bridge/poll", providerPollJSON(t, "root", reports)); code != 200 {
		t.Fatal(code)
	}
	if _, ok := f.hub.Enqueue("root", "keep queued"); !ok {
		t.Fatal("enqueue failed")
	}
	before := f.state()
	tests := map[string]func(map[string]any){
		"missing epoch":           func(p map[string]any) { delete(p, "providerLimitsEpoch") },
		"null epoch":              func(p map[string]any) { p["providerLimitsEpoch"] = nil },
		"numeric epoch":           func(p map[string]any) { p["providerLimitsEpoch"] = 123 },
		"empty epoch":             func(p map[string]any) { p["providerLimitsEpoch"] = "" },
		"malformed epoch":         func(p map[string]any) { p["providerLimitsEpoch"] = "not-a-uuid" },
		"bad hex epoch":           func(p map[string]any) { p["providerLimitsEpoch"] = "g1111111-1111-4111-8111-111111111111" },
		"bad separators":          func(p map[string]any) { p["providerLimitsEpoch"] = "11111111_1111_4111_8111_111111111111" },
		"no separators":           func(p map[string]any) { p["providerLimitsEpoch"] = strings.ReplaceAll(providerEpoch, "-", "") },
		"braced epoch":            func(p map[string]any) { p["providerLimitsEpoch"] = "{" + providerEpoch + "}" },
		"long epoch":              func(p map[string]any) { p["providerLimitsEpoch"] = strings.Repeat("a", 1000) },
		"missing revision":        func(p map[string]any) { delete(p, "providerLimitsRevision") },
		"null revision":           func(p map[string]any) { p["providerLimitsRevision"] = nil },
		"zero revision":           func(p map[string]any) { p["providerLimitsRevision"] = 0 },
		"negative revision":       func(p map[string]any) { p["providerLimitsRevision"] = -1 },
		"fractional revision":     func(p map[string]any) { p["providerLimitsRevision"] = 1.5 },
		"string revision":         func(p map[string]any) { p["providerLimitsRevision"] = "2" },
		"boolean revision":        func(p map[string]any) { p["providerLimitsRevision"] = true },
		"unsafe revision":         func(p map[string]any) { p["providerLimitsRevision"] = int64(1 << 53) },
		"overflow revision":       func(p map[string]any) { p["providerLimitsRevision"] = json.RawMessage("9223372036854775808") },
		"both missing":            func(p map[string]any) { delete(p, "providerLimitsEpoch"); delete(p, "providerLimitsRevision") },
		"empty needs version":     func(p map[string]any) { p["providerLimits"] = []any{}; delete(p, "providerLimitsRevision") },
		"version without reports": func(p map[string]any) { delete(p, "providerLimits") },
		"epoch only":              func(p map[string]any) { delete(p, "providerLimits"); delete(p, "providerLimitsRevision") },
		"revision only":           func(p map[string]any) { delete(p, "providerLimits"); delete(p, "providerLimitsEpoch") },
	}
	for name, invalid := range tests {
		t.Run(name, func(t *testing.T) {
			for _, root := range []string{"root", "new-root"} {
				p := map[string]any{"agent": "opencode", "sessionId": root, "title": "must not mutate", "providerLimits": reports, "providerLimitsEpoch": providerEpoch, "providerLimitsRevision": 2}
				invalid(p)
				raw, err := json.Marshal(p)
				if err != nil {
					t.Fatal(err)
				}
				if code := f.post("/api/bridge/poll", string(raw)); code != 400 {
					t.Fatalf("invalid version = %d: %s", code, raw)
				}
				got := f.state()
				if len(got.Sessions) != 1 || got.Sessions[0].Title != before.Sessions[0].Title || got.Sessions[0].Replies[0].State != hub.Queued || !reflect.DeepEqual(got.ProviderLimits, before.ProviderLimits) {
					t.Fatal("invalid envelope mutated state or claimed reply")
				}
			}
		})
	}
}

func TestProviderLimitsVersionedSnapshotsOverHTTP(t *testing.T) {
	f := newFixture(t)
	now := time.Now().Add(-time.Minute)
	a := map[string]any{"provider": "openai-codex", "status": "unsupported", "checkedAt": now.UnixMilli(), "windows": []any{}}
	b := providerReportJSON(now.Add(time.Second))
	b["provider"] = "github-copilot"
	epoch2 := "abcdefab-cdef-4abc-8def-abcdefabcdef"
	poll := func(epoch string, revision int64, reports ...any) {
		t.Helper()
		if reports == nil {
			reports = []any{}
		}
		if code := f.post("/api/bridge/poll", providerVersionedPollJSON(t, "root", epoch, revision, reports)); code != 200 {
			t.Fatal(code)
		}
	}
	poll(providerEpoch, 1, a, b)
	poll(providerEpoch, 2, a)
	if got := f.state().ProviderLimits; len(got) != 1 || got[0].Provider != "openai-codex" {
		t.Fatalf("cached provider blocked removal: %+v", got)
	}
	poll(providerEpoch, 3)
	a["status"] = "auth_required"
	poll(providerEpoch, 4, a)
	poll(providerEpoch, 3)
	poll(providerEpoch, 4)
	if got := f.state().ProviderLimits; len(got) != 1 || got[0].Status != "auth_required" {
		t.Fatalf("same-ms context rejected or old clear accepted: %+v", got)
	}
	poll(epoch2, 1, b)
	poll(providerEpoch, 100, a)
	poll(providerEpoch, 101)
	if got := f.state().ProviderLimits; len(got) != 1 || got[0].Provider != "github-copilot" {
		t.Fatalf("epoch restart failed or superseded packet accepted: %+v", got)
	}
	ticks, cancel := f.hub.Subscribe()
	defer cancel()
	poll(strings.ToUpper(epoch2), 1, a)
	poll(epoch2, 1, b)
	select {
	case <-ticks:
		t.Fatal("same-version polls published changes")
	default:
	}
	if got := f.state().ProviderLimits; len(got) != 1 || got[0].Provider != "github-copilot" {
		t.Fatal("UUID case changed epoch identity")
	}
	poll(epoch2, 1<<53-1, a)
	if got := f.state().ProviderLimits; len(got) != 1 || got[0].Status != "auth_required" {
		t.Fatal("maximum safe JS revision rejected")
	}
	_, body := f.do("GET", "/api/sessions", "", nil)
	if strings.Contains(body, "providerLimitsEpoch") || strings.Contains(body, "providerLimitsRevision") || strings.Contains(body, epoch2) {
		t.Fatal("ordering metadata leaked into UI contract")
	}
}

func TestProviderLimitsRetryBackoffOverHTTP(t *testing.T) {
	f := newFixture(t)
	for i, delay := range []time.Duration{time.Minute, 15 * time.Minute, time.Hour, 24 * time.Hour} {
		now := time.Now().UTC().Truncate(time.Millisecond)
		p := providerReportJSON(now)
		p["status"], p["retryAt"] = "throttled", now.Add(delay).UnixMilli()
		if code := f.post("/api/bridge/poll", providerVersionedPollJSON(t, "root", providerEpoch, int64(i+1), []any{p})); code != 200 {
			t.Fatalf("%s retry broke heartbeat: %d", delay, code)
		}
		if got := f.state().ProviderLimits[0]; !got.RetryAt.Equal(now.Add(delay)) {
			t.Fatalf("retry time changed: %+v", got)
		}
	}
	p := providerReportJSON(time.Now().Add(time.Minute))
	if code := f.post("/api/bridge/poll", providerVersionedPollJSON(t, "root", providerEpoch, 5, []any{p})); code != 200 {
		t.Fatalf("one-minute measurement skew rejected over HTTP: %d", code)
	}
}
