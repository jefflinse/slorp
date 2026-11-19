package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRetainedSubagentLifecycle(t *testing.T) {
	h, k, st := newHub(t)
	h.Apply("opencode", "root", func(s *Session) { s.Used = Tokens{In: 42}; s.CostUSD = 1.25 })
	a := Subagent{ID: "ses_child:own", ParentID: "ses_parent", Type: "review", Description: "Review it", Depth: 2,
		Status: "running", Doing: "reading", Samples: []int{12}, Tools: 3, Used: Tokens{Out: 15}, CostUSD: .5,
		StartedAt: time.Now().Add(-time.Minute).Truncate(time.Millisecond), ActiveAt: time.Now().Truncate(time.Millisecond)}
	report := SubagentReport{Prompt: "private prompt", Report: "private report"}
	must(t, h.UpsertSubagent("opencode", "root", 100, a, &report))
	report.Report = "caller mutation"
	a.Samples[0] = 99
	s := session(t, h, "root")
	if len(s.Subagents) != 1 || s.Subagents[0].ReportRevision != 100 || s.Subagents[0].Samples[0] != 12 || s.Used.In != 42 || s.CostUSD != 1.25 {
		t.Fatalf("accepted child or root usage: %+v", s)
	}
	s.Subagents[0].Samples[0] = 88
	if session(t, h, "root").Subagents[0].Samples[0] != 12 {
		t.Fatal("snapshot aliases retained samples")
	}
	b, err := json.Marshal(h.State())
	must(t, err)
	if strings.Contains(string(b), "private") {
		t.Fatal("report leaked into state/SSE")
	}
	var doc map[string]any
	_, err = st.Get("session", "root", &doc)
	must(t, err)
	if _, ok := doc["subagents"]; ok {
		t.Fatal("child stored in session document")
	}

	ch, cancel := h.Subscribe()
	defer cancel()
	for _, revision := range []int64{99, 100} {
		must(t, h.UpsertSubagent("opencode", "root", revision, Subagent{ID: a.ID, Description: "stale"}, &SubagentReport{Report: "stale"}))
	}
	select {
	case <-ch:
		t.Fatal("stale write notified subscribers")
	default:
	}
	if got, ok := h.RetainedSubagentReport("opencode", "root", a.ID); !ok || got.Report != "private report" {
		t.Fatalf("stale or caller mutation changed report: %+v %v", got, ok)
	}

	h.sessions["root"].bridgeSeen = time.Now().Add(-time.Minute)
	s = session(t, h, "root")
	if got := s.Subagents[0]; got.Status != "" || got.Doing != "" || len(got.Samples) != 0 || got.Description != a.Description || got.Tools != 3 || got.CostUSD != .5 || got.ParentID != a.ParentID {
		t.Fatalf("expired metadata: %+v", got)
	}
	a.Status, a.Doing, a.Samples = "failed", "error", nil
	must(t, h.UpsertSubagent("opencode", "root", 101, a, nil))
	select {
	case <-ch:
	default:
		t.Fatal("accepted lifecycle change did not notify subscribers")
	}
	h.Apply("opencode", "root", func(s *Session) { s.Delegated([]Subagent{{ID: a.ID, Status: "running"}}) })
	if got := session(t, h, "root").Subagents[0]; got.Status != "failed" || !got.Done || got.ReportRevision != 101 {
		t.Fatalf("old poll overwrote terminal record: %+v", got)
	}
	h.Retire("opencode", -time.Second)
	checkArchive := func(h *Hub) {
		t.Helper()
		got, ok := h.Archive("root")
		if !ok || len(got.Subagents) != 1 || got.Subagents[0].Status != "failed" || got.Subagents[0].Doing != "" || got.Subagents[0].ReportRevision != 101 {
			t.Fatalf("child-only archive: %+v %v", got, ok)
		}
		if len(h.State().Archive[0].Subagents) != 0 {
			t.Fatal("archive list includes detail")
		}
		if r, ok := h.RetainedSubagentReport("opencode", "root", a.ID); !ok || r.Report != "private report" {
			t.Fatalf("omitted report not preserved: %+v %v", r, ok)
		}
	}
	checkArchive(h)
	h = New(k, st)
	checkArchive(h)
	if err := h.UpsertSubagent("opencode", "root", 102, a, nil); !errors.Is(err, ErrSubagentConflict) {
		t.Fatalf("archived write: %v", err)
	}
	h.Apply("opencode", "root", nil)
	if len(h.State().Archive) != 0 || len(session(t, h, "root").Subagents) != 1 {
		t.Fatal("reopening lost children or left archive")
	}
	// Restart while open, then settle a root with no prompts or insight.
	h = New(k, st)
	h.SetRoster("opencode", nil)
	checkArchive(h)
}

type subagentFailCache struct {
	Cache
	failKind    string
	loadFailure bool
}

func (c *subagentFailCache) Put(kind, id string, v any) error {
	if kind == c.failKind {
		return errors.New("injected storage failure")
	}
	return c.Cache.Put(kind, id, v)
}

func (c *subagentFailCache) Each(kind string, fn func(string, []byte)) error {
	if kind == "subagent" && c.loadFailure {
		return errors.New("injected loading failure")
	}
	return c.Cache.Each(kind, fn)
}

func TestRetainedSubagentStorageFailureRetry(t *testing.T) {
	_, k, st := newHub(t)
	c := &subagentFailCache{Cache: st}
	h := New(k, c)
	h.Apply("opencode", "root", nil)
	a := Subagent{ID: "child", Description: "first"}
	must(t, h.UpsertSubagent("opencode", "root", 1, a, &SubagentReport{Report: "first"}))
	for _, kind := range []string{"session", "subagent"} {
		t.Run(kind, func(t *testing.T) {
			c.failKind = kind
			a.Description = "second"
			before := h.State()
			ch, cancel := h.Subscribe()
			defer cancel()
			if err := h.UpsertSubagent("opencode", "root", 2, a, &SubagentReport{Report: "second"}); err == nil {
				t.Fatal("write succeeded despite storage failure")
			}
			if !reflect.DeepEqual(before, h.State()) {
				t.Fatal("failed write changed visible state")
			}
			select {
			case <-ch:
				t.Fatal("failed write notified subscribers")
			default:
			}
			if r, _ := h.RetainedSubagentReport("opencode", "root", "child"); r.Report != "first" {
				t.Fatal("failed write changed report")
			}
		})
	}
	c.failKind = ""
	must(t, h.UpsertSubagent("opencode", "root", 2, a, &SubagentReport{Report: "second"}))
	h = New(k, c)
	h.Apply("opencode", "root", nil)
	must(t, h.UpsertSubagent("opencode", "root", 1, Subagent{ID: "child"}, nil))
	if got := session(t, h, "root").Subagents[0]; got.ReportRevision != 2 || got.Description != "second" {
		t.Fatalf("retry or restored revision failed: %+v", got)
	}
	c.loadFailure = true
	h = New(k, c)
	h.Apply("opencode", "root", nil)
	if err := h.UpsertSubagent("opencode", "root", 3, a, nil); err == nil {
		t.Fatal("write allowed after failed index load")
	}
}

func TestRetainedSubagentSourceAndBounds(t *testing.T) {
	h, _, _ := newHub(t)
	source := Subagent{ID: "a", Type: "Explore", Description: "source", Tools: 5, Used: Tokens{Out: 10}}
	h.SetRoster("claude", []Session{{ID: "root", Agent: "claude", Subagents: []Subagent{source}}})
	must(t, h.UpsertSubagent("claude", "root", 1, Subagent{ID: "a", Description: "bridge", Tools: 7}, nil))
	want := source
	want.ReportRevision = 1
	if got := session(t, h, "root").Subagents[0]; !reflect.DeepEqual(got, want) {
		t.Fatalf("source overwritten: %+v", got)
	}
	if err := h.UpsertSubagent("opencode", "root", 2, Subagent{ID: "a"}, nil); !errors.Is(err, ErrSubagentConflict) {
		t.Fatal(err)
	}
	if err := h.UpsertSubagent("opencode", "missing", 2, Subagent{ID: "a"}, nil); !errors.Is(err, ErrSubagentSession) {
		t.Fatal(err)
	}
	for i := 1; i < retainedSubagentLimit; i++ {
		must(t, h.UpsertSubagent("claude", "root", 1, Subagent{ID: fmt.Sprint(i)}, nil))
	}
	if err := h.UpsertSubagent("claude", "root", 1, Subagent{ID: "extra"}, nil); !errors.Is(err, ErrSubagentLimit) {
		t.Fatalf("unbounded child count: %v", err)
	}
	must(t, h.UpsertSubagent("claude", "root", 2, Subagent{ID: "a"}, nil))
	h.Apply("opencode", "other", nil)
	must(t, h.UpsertSubagent("opencode", "other", 1, Subagent{ID: "a"}, &SubagentReport{Report: "other"}))
	if _, ok := h.RetainedSubagentReport("claude", "root", "a"); ok {
		t.Fatal("report crossed roots")
	}
	if subagentKey("a:b", "c") == subagentKey("a", "b:c") {
		t.Fatal("ambiguous key")
	}
}

func TestRetainedSubagentPollActivity(t *testing.T) {
	h, _, _ := newHub(t)
	h.Apply("opencode", "root", nil)
	a := Subagent{ID: "child", Status: "running", Doing: "initial", Samples: []int{1}, Tools: 7, CostUSD: .5, ParentID: "parent"}
	must(t, h.UpsertSubagent("opencode", "root", 1, a, nil))
	r := h.retained["root"]["child"]
	r.ReceivedAt = time.Now().Add(-time.Minute)
	h.retained["root"]["child"] = r
	if got := session(t, h, "root").Subagents[0]; got.Status != "" || got.Doing != "" || len(got.Samples) != 0 {
		t.Fatalf("retained activity did not expire: %+v", got)
	}
	h.Apply("opencode", "root", func(s *Session) {
		s.Delegated([]Subagent{{ID: "child", Status: "waiting", Doing: "current", Samples: []int{2}, CostUSD: 99, ParentID: "wrong"}})
	})
	got := session(t, h, "root").Subagents[0]
	if got.Status != "waiting" || got.Doing != "current" || len(got.Samples) != 1 || got.Samples[0] != 2 || got.Tools != 7 || got.CostUSD != .5 || got.ParentID != "parent" {
		t.Fatalf("poll did not refresh just transient activity: %+v", got)
	}
	h.sessions["root"].bridgeSeen = time.Now().Add(-time.Minute)
	if got := session(t, h, "root").Subagents[0]; got.Status != "" || got.Doing != "" || len(got.Samples) != 0 || got.Tools != 7 {
		t.Fatalf("poll activity did not expire: %+v", got)
	}
}

func TestRetainedSubagentConcurrentRevisions(t *testing.T) {
	h, k, st := newHub(t)
	h.Apply("opencode", "root", nil)
	var wg sync.WaitGroup
	for revision := int64(1); revision <= 32; revision++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := h.UpsertSubagent("opencode", "root", revision, Subagent{ID: "child", Tools: int(revision)}, &SubagentReport{Report: fmt.Sprint(revision)}); err != nil {
				t.Error(err)
			}
			h.Sessions()
			h.RetainedSubagentReport("opencode", "root", "child")
		}()
	}
	wg.Wait()
	h = New(k, st)
	h.Apply("opencode", "root", nil)
	got := session(t, h, "root").Subagents[0]
	report, ok := h.RetainedSubagentReport("opencode", "root", "child")
	if got.ReportRevision != 32 || got.Tools != 32 || !ok || report.Report != "32" {
		t.Fatalf("concurrent record torn or regressed: %+v %+v", got, report)
	}
}

func TestSourceBackedRetainedTerminalLifecycle(t *testing.T) {
	for _, status := range []string{"failed", "killed", "completed", ""} {
		t.Run("terminal-"+status, func(t *testing.T) {
			h, k, st := newHub(t)
			source := Subagent{ID: "child", Type: "Explore", Description: "source", Model: "source-model", Tools: 7, Used: Tokens{Out: 12}, Status: "running"}
			roster := []Session{{ID: "root", Agent: "claude", Subagents: []Subagent{source}}}
			h.SetRoster("claude", roster)
			must(t, h.UpsertSubagent("claude", "root", 1, Subagent{ID: "child", Status: status, Done: true, Type: "wrong", Tools: 99}, nil))
			check := func(got Subagent, sourceBacked bool) {
				t.Helper()
				if got.Status != status || !got.Done || got.ReportRevision != 1 {
					t.Errorf("retained terminal lifecycle lost: %+v", got)
				}
				if sourceBacked && (got.Type != source.Type || got.Description != source.Description || got.Model != source.Model || got.Tools != source.Tools || got.Used != source.Used) {
					t.Errorf("source metadata overwritten: %+v", got)
				}
			}
			check(session(t, h, "root").Subagents[0], true)
			h.Apply("claude", "root", func(s *Session) {
				s.Delegated([]Subagent{{ID: "child", Status: "running", Doing: "stale work", Samples: []int{1}}})
			})
			check(session(t, h, "root").Subagents[0], true)
			h.SetRoster("claude", roster)
			check(session(t, h, "root").Subagents[0], true)
			h.sessions["root"].bridgeSeen = time.Now().Add(-time.Minute)
			check(session(t, h, "root").Subagents[0], true)
			h.SetRoster("claude", nil)
			archived, ok := h.Archive("root")
			if !ok || len(archived.Subagents) != 1 {
				t.Fatalf("missing archived child: %+v", archived)
			}
			check(archived.Subagents[0], false)
			h = New(k, st)
			archived, ok = h.Archive("root")
			if !ok || len(archived.Subagents) != 1 {
				t.Fatalf("missing restarted archive: %+v", archived)
			}
			check(archived.Subagents[0], false)
			h.SetRoster("claude", roster)
			h.Apply("claude", "root", func(s *Session) { s.Delegated([]Subagent{{ID: "child", Status: "running"}}) })
			check(session(t, h, "root").Subagents[0], true)
		})
	}
}

func TestParentUpdateDoesNotRefreshChildObservation(t *testing.T) {
	h, _, _ := newHub(t)
	h.Apply("opencode", "root", func(s *Session) {
		s.Delegated([]Subagent{{ID: "child", Status: "waiting", Doing: "old", Samples: []int{1}}})
	})
	must(t, h.UpsertSubagent("opencode", "root", 1, Subagent{ID: "child", Status: "running", Doing: "new", Samples: []int{2}}, nil))
	h.Apply("opencode", "root", func(s *Session) { s.LastReply = "parent activity" })
	got := session(t, h, "root").Subagents[0]
	if got.Status != "running" || got.Doing != "new" || !reflect.DeepEqual(got.Samples, []int{2}) {
		t.Fatalf("parent update refreshed old child observation: %+v", got)
	}
}

func TestSubagentSamplesAreOwned(t *testing.T) {
	for _, kind := range []string{"source", "live", "source-live", "source-retained", "retained", "retained-live"} {
		t.Run(kind, func(t *testing.T) {
			h, _, _ := newHub(t)
			input := []Subagent{{ID: "child", Samples: []int{7}, Status: "running"}}
			if strings.Contains(kind, "source") {
				h.SetRoster("claude", []Session{{ID: "root", Agent: "claude", Subagents: input}})
			} else {
				h.Apply("claude", "root", nil)
			}
			if strings.Contains(kind, "retained") {
				h.Apply("claude", "root", nil)
				must(t, h.UpsertSubagent("claude", "root", 1, input[0], nil))
			}
			if strings.Contains(kind, "live") {
				h.Apply("claude", "root", func(s *Session) { s.Delegated(input) })
			}
			input[0].Samples[0] = 99
			input[0].Description = "mutated input"
			got := session(t, h, "root").Subagents[0]
			if !reflect.DeepEqual(got.Samples, []int{7}) || got.Description != "" {
				t.Fatalf("ingress aliases caller data: %+v", got)
			}
			got.Samples[0] = 88
			next := h.State().Sessions[0].Subagents[0]
			if !reflect.DeepEqual(next.Samples, []int{7}) {
				t.Errorf("snapshot aliases stored samples: %+v", next)
			}
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range 1000 {
					got.Samples[0] = i
					input[0].Samples[0] = i
				}
			}()
			for range 100 {
				if got := h.State().Sessions[0].Subagents[0]; !reflect.DeepEqual(got.Samples, []int{7}) {
					t.Errorf("concurrent snapshot mutation reached hub: %+v", got)
					break
				}
			}
			wg.Wait()
		})
	}
}

func TestChildLiveObservationFreshness(t *testing.T) {
	for _, kind := range []string{"live", "source-live", "retained-live", "source-retained-live"} {
		t.Run(kind, func(t *testing.T) {
			h, _, _ := newHub(t)
			source := strings.Contains(kind, "source")
			retained := strings.Contains(kind, "retained")
			roster := []Session{{ID: "root", Agent: "claude"}}
			if source {
				roster[0].Subagents = []Subagent{{ID: "child", Tools: 7}}
			}
			h.SetRoster("claude", roster)
			h.Apply("claude", "root", func(s *Session) {
				s.Delegated([]Subagent{{ID: "child", Status: "waiting", Doing: "old", Samples: []int{1}}})
			})
			observed := h.sessions["root"].liveSeen
			if observed.IsZero() {
				t.Fatal("Delegated did not timestamp the observation")
			}
			if retained {
				must(t, h.UpsertSubagent("claude", "root", 1, Subagent{ID: "child", Status: "running", Doing: "new", Samples: []int{2}}, nil))
			}
			h.Apply("claude", "root", nil)
			h.SetRoster("claude", roster)
			if !h.sessions["root"].liveSeen.Equal(observed) {
				t.Fatal("parent heartbeat or roster changed the child observation time")
			}
			want := "waiting"
			if retained {
				want = "running"
			}
			if got := session(t, h, "root").Subagents[0]; got.Status != want {
				t.Fatalf("wrong observation wins: %+v, want %s", got, want)
			}

			expired := time.Now().Add(-2 * bridgeTTL)
			h.sessions["root"].liveSeen = expired
			if retained {
				r := h.retained["root"]["child"]
				r.ReceivedAt = expired
				h.retained["root"]["child"] = r
			}
			h.Apply("claude", "root", nil)
			h.SetRoster("claude", roster)
			s := session(t, h, "root")
			if !s.CanReply || !h.sessions["root"].liveSeen.Equal(expired) {
				t.Fatal("parent heartbeat must stay fresh without refreshing child observations")
			}
			if source || retained {
				if len(s.Subagents) != 1 || s.Subagents[0].Status != "" || s.Subagents[0].Doing != "" || len(s.Subagents[0].Samples) != 0 {
					t.Fatalf("expired live observation survived heartbeat: %+v", s.Subagents)
				}
			} else if len(s.Subagents) != 0 {
				t.Fatalf("expired live-only child survived heartbeat: %+v", s.Subagents)
			}
			h.Apply("claude", "root", func(s *Session) {
				s.Delegated([]Subagent{{ID: "child", Status: "waiting", Doing: "fresh", Samples: []int{3}}})
			})
			if !h.sessions["root"].liveSeen.After(observed) {
				t.Fatal("new observation did not advance liveSeen")
			}
			if got := session(t, h, "root").Subagents[0]; got.Status != "waiting" || got.Doing != "fresh" || !reflect.DeepEqual(got.Samples, []int{3}) {
				t.Fatalf("new live observation not used: %+v", got)
			}
			h.Apply("claude", "root", func(s *Session) { s.Delegated(nil) })
			if got := session(t, h, "root"); len(got.Subagents) > 0 && got.Subagents[0].Status != "" {
				t.Fatalf("empty observation did not clear live state: %+v", got.Subagents)
			}
		})
	}
}

func TestWithLivePreservesRetainedTerminalLifecycle(t *testing.T) {
	for _, status := range []string{"failed", "killed", "completed", ""} {
		known := []Subagent{{ID: "child", Done: true, Status: status, ReportRevision: 1, Samples: []int{1}, Tools: 7}}
		live := []Subagent{{ID: "child", Status: "running", Samples: []int{2}, Tools: 99}}
		got := withLive(known, live)[0]
		if !got.Done || got.Status != status || got.Tools != 7 || !reflect.DeepEqual(got.Samples, []int{1}) {
			t.Fatalf("unversioned overlay changed retained terminal lifecycle: %+v", got)
		}
		got.Samples[0] = 99
		if known[0].Samples[0] != 1 || live[0].Samples[0] != 2 {
			t.Fatal("withLive aliases its inputs")
		}
	}
}

func TestEnrichmentSnapshotsOwnSubagentSamples(t *testing.T) {
	h, k, _ := newHub(t)
	effort(t, k, "Work")
	role(t, k, "Reviewer", "")
	s := idle("root", "/work", "reply")
	s.Subagents = []Subagent{{ID: "child", Samples: []int{7}}}
	h.SetRoster("claude", []Session{s})
	quieten(h, "root")
	undescribed := h.Undescribed()
	if len(undescribed) != 1 {
		t.Fatalf("undescribed = %+v", undescribed)
	}
	undescribed[0].Session.Subagents[0].Samples[0] = 99
	describe(h, "root", KindNone)
	unrouted, unroled := h.Unrouted(), h.Unroled()
	if len(unrouted) != 1 || len(unroled) != 1 {
		t.Fatalf("unrouted = %+v, unroled = %+v", unrouted, unroled)
	}
	unrouted[0].Session.Subagents[0].Samples[0] = 88
	unroled[0].Session.Subagents[0].Samples[0] = 77
	if got := session(t, h, "root").Subagents[0]; !reflect.DeepEqual(got.Samples, []int{7}) {
		t.Fatalf("enrichment snapshots alias hub samples: %+v", got)
	}
}
