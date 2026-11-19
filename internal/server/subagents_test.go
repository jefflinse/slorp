package server

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/hub"
	"github.com/jefflinse/slorp/internal/testdb"
)

func TestBridgeSubagentHTTP(t *testing.T) {
	f := newFixture(t)
	const path = "/api/bridge/subagent"
	body := `{"agent":"opencode","sessionId":"root","revision":100,"subagent":{"id":"ses_child:own","type":"review","description":"Review it","status":"running","depth":1,"startedAt":"2026-11-19T12:00:00Z","activeAt":"2026-11-19T12:01:00Z","tools":2,"used":{"out":10}},"report":{"prompt":"private prompt","report":"private report","handed":false}}`
	if code := f.post(path, body); code != 404 || len(f.hub.Sessions()) != 0 {
		t.Fatalf("unknown root upsert = %d", code)
	}
	if code := f.post("/api/bridge/poll", `{"agent":"opencode","sessionId":"root","snapshot":{"used":{"out":100},"costUsd":1}}`); code != 200 {
		t.Fatal(code)
	}
	if code := f.post(path, strings.Replace(body, `"opencode"`, `"claude"`, 1)); code != 409 {
		t.Fatalf("mismatched agent = %d", code)
	}
	if code := f.post(path, body); code != 204 {
		t.Fatalf("upsert = %d", code)
	}
	s := f.state().Sessions[0]
	if len(s.Subagents) != 1 || s.Subagents[0].ReportRevision != 100 || s.Subagents[0].Tools != 2 || s.Used.Out != 100 || s.CostUSD != 1 {
		t.Fatalf("state = %+v", s)
	}
	_, state := f.do("GET", "/api/sessions", "", nil)
	if strings.Contains(state, "private") {
		t.Fatal("report leaked in state")
	}
	for _, revision := range []string{"99", "100"} {
		stale := strings.Replace(strings.Replace(body, `"revision":100`, `"revision":`+revision, 1), "private report", "stale report", 1)
		if code := f.post(path, stale); code != 204 {
			t.Fatalf("stale upsert = %d", code)
		}
	}
	const reportPath = "/api/sessions/root/subagents/ses_child:own"
	checkReport := func() {
		t.Helper()
		code, got := f.do("GET", reportPath, "", nil)
		var r hub.SubagentReport
		if err := json.Unmarshal([]byte(got), &r); err != nil || code != 200 || r.Prompt != "private prompt" || r.Report != "private report" {
			t.Fatalf("report = %d %s (%v)", code, got, err)
		}
	}
	checkReport()
	if code := f.post(path, `{"agent":"opencode","sessionId":"root","revision":101,"subagent":{"id":"ses_child:own","status":"completed"}}`); code != 204 {
		t.Fatal(code)
	}
	checkReport()
	f.hub.Retire("opencode", -time.Second)
	if code := f.post(path, body); code != 409 || len(f.hub.Sessions()) != 0 {
		t.Fatalf("closed root reopened: %d", code)
	}
	checkReport()
	code, got := f.do("GET", "/api/archive/root", "", nil)
	var a hub.Archived
	if err := json.Unmarshal([]byte(got), &a); err != nil || code != 200 || len(a.Subagents) != 1 || !a.Subagents[0].Done || a.Subagents[0].ReportRevision != 101 {
		t.Fatalf("archive = %d %s (%v)", code, got, err)
	}
	if code, _ := f.do("GET", "/api/sessions/root/subagents/missing", "", nil); code != 404 {
		t.Fatalf("missing child report = %d", code)
	}
}

func TestBridgeSubagentReportReplacement(t *testing.T) {
	f := newFixture(t)
	f.hub.Apply("opencode", "root", nil)
	const path = "/api/bridge/subagent"
	const reportPath = "/api/sessions/root/subagents/child"
	for _, step := range []struct {
		body     string
		wantCode int
		want     hub.SubagentReport
	}{
		{`{"agent":"opencode","sessionId":"root","revision":1,"subagent":{"id":"child"}}`, 404, hub.SubagentReport{}},
		{`{"agent":"opencode","sessionId":"root","revision":2,"subagent":{"id":"child"},"report":{"prompt":"p","report":"r","handed":true}}`, 200, hub.SubagentReport{Prompt: "p", Report: "r", Handed: true}},
		{`{"agent":"opencode","sessionId":"root","revision":3,"subagent":{"id":"child"},"report":null}`, 200, hub.SubagentReport{Prompt: "p", Report: "r", Handed: true}},
		{`{"agent":"opencode","sessionId":"root","revision":4,"subagent":{"id":"child"},"report":{}}`, 200, hub.SubagentReport{}},
	} {
		if code := f.post(path, step.body); code != 204 {
			t.Fatal(code)
		}
		code, body := f.do("GET", reportPath, "", nil)
		if code != step.wantCode {
			t.Fatalf("report = %d %s", code, body)
		}
		if code == 200 {
			var got hub.SubagentReport
			if err := json.Unmarshal([]byte(body), &got); err != nil || got != step.want {
				t.Fatalf("report = %s (%v), want %+v", body, err, step.want)
			}
		}
	}
}

func TestBridgeSubagentValidation(t *testing.T) {
	f := newFixture(t)
	f.hub.Apply("opencode", "root", nil)
	valid := `{"agent":"opencode","sessionId":"root","revision":1,"subagent":{"id":"child"}}`
	cases := map[string]string{
		"empty":               `{}`,
		"zero revision":       strings.Replace(valid, `"revision":1`, `"revision":0`, 1),
		"negative revision":   strings.Replace(valid, `"revision":1`, `"revision":-1`, 1),
		"fractional revision": strings.Replace(valid, `"revision":1`, `"revision":1.5`, 1),
		"overflow revision":   strings.Replace(valid, `"revision":1`, `"revision":9223372036854775808`, 1),
		"missing agent":       strings.Replace(valid, `"agent":"opencode",`, "", 1),
		"bad agent":           strings.Replace(valid, `"opencode"`, `"other"`, 1),
		"path child":          strings.Replace(valid, `"child"`, `"../secret"`, 1),
		"backslash child":     strings.Replace(valid, `"child"`, `"a\\b"`, 1),
		"empty child":         strings.Replace(valid, `"child"`, `""`, 1),
		"long child":          strings.Replace(valid, `"child"`, `"`+strings.Repeat("x", 257)+`"`, 1),
		"self child":          strings.Replace(valid, `"child"`, `"root"`, 1),
		"trailing document":   valid + `{}`,
		"oversize trailing":   valid + strings.Repeat(" ", 1<<20),
		"oversize body":       `{"padding":"` + strings.Repeat("x", 1<<20) + `"}`,
	}
	for name, extra := range map[string]string{
		"status": `"status":"unknown"`, "done conflict": `"done":true,"status":"running"`,
		"depth": `"depth":65`, "negative depth": `"depth":-1`, "tools": `"tools":-1`, "huge tools": `"tools":1000000001`,
		"usage": `"used":{"in":-1}`, "huge usage": `"used":{"out":1000000000001}`, "cost": `"costUsd":-1`,
		"samples": `"samples":[-1]`, "many samples": `"samples":[0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0]`,
		"numeric time": `"startedAt":1234`, "invalid time": `"activeAt":"yesterday"`,
		"parent path": `"parentId":"/tmp/secret"`, "self parent": `"parentId":"child"`,
		"description": `"description":"` + strings.Repeat("x", 1001) + `"`,
		"doing":       `"doing":"` + strings.Repeat("x", 301) + `"`,
		"type":        `"type":"` + strings.Repeat("x", 81) + `"`,
		"model":       `"model":"` + strings.Repeat("x", 201) + `"`,
		"effort":      `"effort":"` + strings.Repeat("x", 81) + `"`,
	} {
		cases[name] = strings.Replace(valid, `"id":"child"`, `"id":"child",`+extra, 1)
	}
	for _, field := range []string{"prompt", "report"} {
		cases["long "+field] = strings.TrimSuffix(valid, "}") + `,"report":{"` + field + `":"` + strings.Repeat("界", 60001) + `"}}`
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if code := f.post("/api/bridge/subagent", body); code != 400 {
				t.Fatalf("invalid request = %d", code)
			}
		})
	}
	if len(f.state().Sessions[0].Subagents) != 0 {
		t.Fatal("invalid requests mutated state")
	}
	// Limits count runes, not bytes; the report revision comes from the envelope.
	body := strings.Replace(valid, `"id":"child"`, `"id":"child","reportRevision":999`, 1)
	body = strings.TrimSuffix(body, "}") + `,"report":{"prompt":"` + strings.Repeat("界", 60000) + `","report":"` + strings.Repeat("界", 60000) + `"}}`
	if code := f.post("/api/bridge/subagent", body); code != 204 {
		t.Fatalf("valid Unicode boundary = %d", code)
	}
	if got := f.state().Sessions[0].Subagents[0].ReportRevision; got != 1 {
		t.Fatalf("client-controlled report revision = %d", got)
	}
}

func TestBridgeSubagentClaudeFallback(t *testing.T) {
	f := newFixture(t)
	source := hub.Subagent{ID: "child", Type: "Explore", Description: "source", Tools: 7}
	f.hub.SetRoster("claude", []hub.Session{{ID: "root", Agent: "claude", Subagents: []hub.Subagent{source}}})
	f.record.subagents = map[string][]hub.Subagent{"root": {source}}
	f.record.reports = map[string]hub.SubagentReport{"root/child": {Report: "source report"}}
	const path = "/api/bridge/subagent"
	const reportPath = "/api/sessions/root/subagents/child"
	body := `{"agent":"claude","sessionId":"root","revision":1,"subagent":{"id":"child","description":"bridge"}}`
	if code := f.post(path, body); code != 204 {
		t.Fatal(code)
	}
	if code, got := f.do("GET", reportPath, "", nil); code != 200 || !strings.Contains(got, "source report") {
		t.Fatalf("fallback = %d %s", code, got)
	}
	body = strings.Replace(body, `"revision":1`, `"revision":2`, 1)
	body = strings.TrimSuffix(body, "}") + `,"report":{"report":"retained report"}}`
	if code := f.post(path, body); code != 204 {
		t.Fatal(code)
	}
	if code, got := f.do("GET", reportPath, "", nil); code != 200 || !strings.Contains(got, "retained report") {
		t.Fatalf("retained-first = %d %s", code, got)
	}
	f.hub.SetRoster("claude", nil)
	code, got := f.do("GET", "/api/archive/root", "", nil)
	var a hub.Archived
	if err := json.Unmarshal([]byte(got), &a); err != nil || code != 200 || len(a.Subagents) != 1 || a.Subagents[0].Description != "source" || a.Subagents[0].Tools != 7 || a.Subagents[0].ReportRevision != 2 {
		t.Fatalf("source archive metadata overwritten: %d %s (%v)", code, got, err)
	}
}

type failingSubagentStore struct {
	hub.Cache
	fail bool
}

func (c *failingSubagentStore) Put(kind, id string, v any) error {
	if c.fail && kind == "subagent" {
		return errors.New("secret storage detail")
	}
	return c.Cache.Put(kind, id, v)
}

func TestBridgeSubagentStorageFailureAndRestart(t *testing.T) {
	st := testdb.Open(t)
	k := testdb.KB(t, st)
	c := &failingSubagentStore{Cache: st, fail: true}
	h := hub.New(k, c)
	serve := func(h *hub.Hub) *fixture {
		srv := httptest.NewServer(New(h, k, nil, nil, nil, nil, nil, nil, nil, nil))
		t.Cleanup(srv.Close)
		return &fixture{t: t, srv: srv, hub: h, kb: k}
	}
	f := serve(h)
	h.Apply("opencode", "root", nil)
	body := `{"agent":"opencode","sessionId":"root","revision":1,"subagent":{"id":"child","status":"completed"},"report":{"report":"saved"}}`
	code, got := f.do("POST", "/api/bridge/subagent", body, nil)
	if code != 500 || strings.Contains(got, "secret") || len(h.Sessions()[0].Subagents) != 0 {
		t.Fatalf("storage failure = %d %s", code, got)
	}
	c.fail = false
	if code := f.post("/api/bridge/subagent", body); code != 204 {
		t.Fatalf("retry = %d", code)
	}
	h = hub.New(k, c)
	h.SetRoster("opencode", nil)
	f = serve(h)
	if code, got := f.do("GET", "/api/sessions/root/subagents/child", "", nil); code != 200 || !strings.Contains(got, "saved") {
		t.Fatalf("report after restart = %d %s", code, got)
	}
	if code, got := f.do("GET", "/api/archive/root", "", nil); code != 200 || !strings.Contains(got, `"reportRevision":1`) {
		t.Fatalf("archive after restart = %d %s", code, got)
	}
}

func TestArchiveMergeRetainedTerminalLifecycle(t *testing.T) {
	for _, status := range []string{"failed", "killed", "completed", ""} {
		t.Run("terminal-"+status, func(t *testing.T) {
			st := testdb.Open(t)
			k := testdb.KB(t, st)
			source := hub.Subagent{ID: "child", Type: "Explore", Description: "source", Tools: 7, Used: hub.Tokens{Out: 12}, Status: "running"}
			rec := &record{subagents: map[string][]hub.Subagent{"root": {source}}}
			serve := func(h *hub.Hub) *fixture {
				srv := httptest.NewServer(New(h, k, nil, nil, nil, nil, nil, map[string]Delegates{"claude": rec}, nil, nil))
				t.Cleanup(srv.Close)
				return &fixture{t: t, srv: srv, hub: h, kb: k}
			}
			f := serve(hub.New(k, st))
			f.hub.SetRoster("claude", []hub.Session{{ID: "root", Agent: "claude", Subagents: []hub.Subagent{source}}})
			if err := f.hub.UpsertSubagent("claude", "root", 1, hub.Subagent{ID: "child", Status: status, Done: true, Tools: 99}, nil); err != nil {
				t.Fatal(err)
			}
			f.hub.SetRoster("claude", nil)
			for _, server := range []*fixture{f, serve(hub.New(k, st))} {
				code, body := server.do("GET", "/api/archive/root", "", nil)
				var got hub.Archived
				if err := json.Unmarshal([]byte(body), &got); err != nil || code != 200 || len(got.Subagents) != 1 {
					t.Fatalf("archive = %d %s (%v)", code, body, err)
				}
				a := got.Subagents[0]
				if !a.Done || a.Status != status || a.ReportRevision != 1 || a.Type != source.Type || a.Description != source.Description || a.Tools != source.Tools || a.Used != source.Used {
					t.Fatalf("archive lost retained lifecycle or source metadata: %+v", a)
				}
			}
		})
	}
}

func TestParentBridgeMessagesDoNotRefreshChildObservation(t *testing.T) {
	f := newFixture(t)
	const old = `{"agent":"opencode","sessionId":"root","agents":[{"id":"child","status":"waiting","doing":"old","tokenSamples":[1]}]}`
	if code := f.post("/api/bridge/poll", old); code != 200 {
		t.Fatal(code)
	}
	if code := f.post("/api/bridge/subagent", `{"agent":"opencode","sessionId":"root","revision":1,"subagent":{"id":"child","status":"running","doing":"new","samples":[2]}}`); code != 204 {
		t.Fatal(code)
	}
	for _, step := range []struct {
		path string
		body string
		code int
	}{
		{"/api/bridge/poll", `{"agent":"opencode","sessionId":"root"}`, 200},
		{"/api/bridge/event", `{"agent":"opencode","sessionId":"root","type":"turn.complete","text":"parent reply"}`, 204},
	} {
		if code := f.post(step.path, step.body); code != step.code {
			t.Fatal(code)
		}
		got := f.state().Sessions[0].Subagents[0]
		if got.Status != "running" || got.Doing != "new" || len(got.Samples) != 1 || got.Samples[0] != 2 {
			t.Fatalf("parent message refreshed stale observation: %+v", got)
		}
	}
	if code := f.post("/api/bridge/poll", strings.Replace(old, "old", "fresh", 1)); code != 200 {
		t.Fatal(code)
	}
	if got := f.state().Sessions[0].Subagents[0]; got.Status != "waiting" || got.Doing != "fresh" {
		t.Fatalf("new child observation ignored: %+v", got)
	}
}

func TestArchiveMergeSamplesAreOwned(t *testing.T) {
	source := []hub.Subagent{{ID: "source", Samples: []int{1}}, {ID: "both", Samples: []int{2}}}
	retained := []hub.Subagent{{ID: "both", Done: true, Status: "failed", Samples: []int{3}}, {ID: "retained", Samples: []int{4}}}
	got := archiveSubagents(source, retained)
	for i := range got {
		got[i].Samples[0] = 99
	}
	if source[0].Samples[0] != 1 || source[1].Samples[0] != 2 || retained[0].Samples[0] != 3 || retained[1].Samples[0] != 4 {
		t.Fatal("archive output aliases source or retained samples")
	}
}
