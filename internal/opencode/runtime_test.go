package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/hub"
)

// TestNativeHelper is the fake executable, not an in-process substitute for a child.
func TestNativeHelper(t *testing.T) {
	if os.Getenv("SLORP_TEST_NATIVE") != "1" {
		return
	}
	if slices.Contains(os.Args, "--version") {
		fmt.Println("opencode v2.3.2")
		os.Exit(0)
	}
	if !slices.Contains(os.Args, "serve") || !slices.Contains(os.Args, "127.0.0.1") || !slices.Contains(os.Args, "0") {
		os.Exit(9)
	}
	if os.Getenv("SLORP_TEST_HANG") == "1" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	home := os.Getenv("HOME")
	if err := os.WriteFile(filepath.Join(home, "native.pid"), []byte(fmt.Sprint(os.Getpid())), 0o600); err != nil {
		os.Exit(7)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		os.Exit(8)
	}
	var mu sync.Mutex
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "opencode" || pass == "" || pass != os.Getenv("OPENCODE_PASSWORD") {
			w.WriteHeader(401)
			return
		}
		if r.URL.Query().Get("location[directory]") == "" {
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		var body map[string]any
		if r.Method == "POST" {
			if r.Body != nil {
				_ = json.NewDecoder(r.Body).Decode(&body)
			}
		}
		f, err := os.OpenFile(filepath.Join(home, "requests.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			w.WriteHeader(500)
			return
		}
		_ = json.NewEncoder(f).Encode(map[string]any{"path": r.URL.Path, "method": r.Method, "body": body})
		_ = f.Close()
		switch r.URL.Path {
		case "/api/info":
			fmt.Fprint(w, `{"healthy":true}`)
		case "/api/config":
			fmt.Fprint(w, `[{"type":"document","info":{"default_agent":"build","model":{"providerID":"provider","model":"org/model"},"plugins":[],"providers":{"private":{"settings":{"apiKey":"never-public"}}}}}]`)
		case "/api/skill":
			fmt.Fprint(w, `{"data":[]}`)
		case "/api/agent":
			fmt.Fprint(w, `{"location":{},"data":[{"id":"build","name":"Build","mode":"primary","description":"Build things","model":{"providerID":"provider","id":"org/model","variant":"high"},"system":"private prompt"},{"id":"review","name":"Review","mode":"all"},{"id":"hidden","mode":"primary","hidden":true},{"id":"child","mode":"subagent"}]}`)
		case "/api/provider":
			fmt.Fprint(w, `{"location":{},"data":[{"id":"provider","name":"Provider display","settings":{"apiKey":"secret"}}]}`)
		case "/api/model":
			fmt.Fprint(w, `{"location":{},"data":[{"id":"org/model","providerID":"provider","name":"Model","type":"chat","status":"active","enabled":true,"capabilities":{"input":["text"],"output":["text"]},"variants":[{"id":"high"},{"id":"low"}]},{"id":"retired","providerID":"provider","status":"deprecated","enabled":true,"capabilities":{"input":["text"],"output":["text"]}},{"id":"other","providerID":"offline","enabled":false,"capabilities":{"input":["text"],"output":["text"]}}]}`)
		case "/api/model/default":
			fmt.Fprint(w, `{"location":{},"data":{"id":"org/model","providerID":"provider"}}`)
		case "/api/plugin":
			fmt.Fprint(w, `{"location":{},"data":[{"id":"slorp","state":{"status":"active"}}]}`)
		case "/api/session":
			if r.Method != "POST" {
				w.WriteHeader(405)
				return
			}
			b, _ := json.Marshal(map[string]any{"data": map[string]any{"id": "ses_native", "title": body["title"], "location": body["location"], "agent": body["agent"], "model": body["model"], "time": map[string]int64{"created": 1700000000000, "updated": 1700000000000}}})
			if err := os.WriteFile(filepath.Join(home, "native.json"), b, 0o600); err != nil {
				w.WriteHeader(500)
				return
			}
			_, _ = w.Write(b)
		case "/api/session/ses_native":
			b, err := os.ReadFile(filepath.Join(home, "native.json"))
			if err != nil {
				w.WriteHeader(404)
				return
			}
			_, _ = w.Write(b)
		case "/api/session/ses_native/prompt":
			fmt.Fprint(w, `{"data":{"id":"msg_admitted","type":"user","status":"pending","payload":{"text":"hello"}}}`)
		case "/api/session/ses_native/interrupt":
			if err := os.WriteFile(filepath.Join(home, "aborted"), []byte("yes"), 0o600); err != nil {
				w.WriteHeader(500)
				return
			}
			fmt.Fprint(w, `{"interrupted":true}`)
		default:
			w.WriteHeader(404)
		}
	})
	fmt.Printf("server listening on http://%s\n", listener.Addr())
	_ = http.Serve(listener, handler)
	os.Exit(0)
}

func runtimeFixture(t *testing.T) *Adapter {
	t.Helper()
	a := fixture(t)
	for _, key := range []string{"XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(key, filepath.Join(a.Home, key))
	}
	t.Setenv("SLORP_TEST_NATIVE", "1")
	t.Setenv("OPENCODE_CONFIG_CONTENT", "")
	t.Setenv("SLORP_TEST_HANG", "")
	if err := os.Mkdir(filepath.Join(a.Home, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	put(t, a.Bin, "#!/bin/sh\nexec "+hub.ShellQuote(bin)+" -test.run='^TestNativeHelper$' -- \"$@\"\n")
	if err := os.Chmod(a.Bin, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Shutdown)
	return a
}

func requests(t *testing.T, a *Adapter, path string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(a.Home, "requests.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var found []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		if r["path"] == path {
			found = append(found, r)
		}
	}
	return found
}

func TestManagedLifecycleSameIdentityAndMetadata(t *testing.T) {
	a := runtimeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	var reportedID, reportedJob string
	ctx = hub.WithLaunchJob(ctx, func(id, job string) { reportedID, reportedJob = id, job })
	id, err := a.Start(ctx, hub.Launch{Dir: a.Home, Name: "Native", Prompt: "hello", AgentName: "review", Model: "provider/org/model", Variant: "low"})
	if err != nil || id != "ses_native" {
		t.Fatalf("start: %q %v", id, err)
	}
	cancel()
	s := a.Hub.Sessions()[0]
	if reportedID != id || reportedJob != s.Job {
		t.Fatalf("created generation not reported: id=%q job=%q session=%+v", reportedID, reportedJob, s)
	}
	r := a.runtimes[s.Job]
	if !s.CanStop || s.Cwd != a.Home || r == nil {
		t.Fatalf("session: %+v", s)
	}
	select {
	case <-r.done:
		t.Fatal("request cancellation killed owned runtime")
	default:
	}
	control := filepath.Join(r.private, "session.json")
	info, err := os.Stat(control)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("control mode: %v %v", info, err)
	}
	info, err = os.Stat(r.private)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("runtime mode: %v %v", info, err)
	}
	b, err := os.ReadFile(control)
	var activation map[string]string
	if err != nil || json.Unmarshal(b, &activation) != nil || activation["sessionId"] != "ses_native" || activation["opencodeURL"] != r.base {
		t.Fatalf("control: %s %v", b, err)
	}
	create := requests(t, a, "/api/session")[0]["body"].(map[string]any)
	model := create["model"].(map[string]any)
	if model["id"] != "org/model" || model["providerID"] != "provider" || model["variant"] != "low" || create["agent"] != "review" {
		t.Fatal(create)
	}
	prompt := requests(t, a, "/api/session/ses_native/prompt")[0]["body"].(map[string]any)
	if prompt["text"] != "hello" || len(prompt) != 1 {
		t.Fatal(prompt)
	}
	a.Hub.Apply(Agent, id, func(s *hub.Session) {
		s.Used.Out = 123
		s.CostUSD = 0.5
		s.LastPrompt = "hello"
		s.PromptAt = time.Unix(1700000001, 0)
	})
	if _, ok := a.Hub.Enqueue(id, "drop me"); !ok {
		t.Fatal("enqueue")
	}
	if err := a.Stop(context.Background(), s.Job); err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.done:
	default:
		t.Fatal("child not reaped")
	}
	if _, err := os.Stat(r.private); !os.IsNotExist(err) {
		t.Fatalf("private files retained: %v", err)
	}
	archived, ok := a.Hub.Archive(id)
	if !ok || archived.Used.Out != 123 || archived.CostUSD != 0.5 {
		t.Fatalf("archive: %+v", archived)
	}
	if err := a.Resume(context.Background(), id, a.Home, ""); err != nil {
		t.Fatal(err)
	}
	opened := a.Hub.Sessions()[0]
	if opened.ID != id || opened.Cwd != a.Home || opened.Used != archived.Used || opened.CostUSD != archived.CostUSD || !opened.StartedAt.Equal(archived.StartedAt) || len(opened.Timeline) != len(archived.Timeline) {
		t.Fatalf("reopen: %+v / %+v", opened, archived)
	}
	if len(opened.Replies) != 1 || opened.Replies[0].State != hub.Dropped {
		t.Fatal(opened.Replies)
	}
	if len(requests(t, a, "/api/session")) != 1 || len(requests(t, a, "/api/session/ses_native/prompt")) != 1 {
		t.Fatal("promptless resume created a new session or turn")
	}
	if err := a.Resume(context.Background(), id, a.Home, "duplicate"); !errors.Is(err, hub.ErrLifecycleConflict) {
		t.Fatal(err)
	}
	if err := a.Stop(context.Background(), opened.Job); err != nil {
		t.Fatal(err)
	}
	if err := a.Resume(context.Background(), id, a.Home, "again"); err != nil {
		t.Fatal(err)
	}
	prompts := requests(t, a, "/api/session/ses_native/prompt")
	last := prompts[len(prompts)-1]["body"].(map[string]any)
	if last["text"] != "again" || len(last) != 1 {
		t.Fatal(last)
	}
}

func TestCatalogAndInvalidLaunchChoices(t *testing.T) {
	a := runtimeFixture(t)
	options, err := a.LaunchOptions(context.Background(), a.Home)
	if err != nil {
		t.Fatal(err)
	}
	if len(options.Agents) != 2 || len(options.Models) != 1 || options.Defaults.AgentName != "build" || options.Defaults.Model != "provider/org/model" || options.Defaults.Variant != "high" || options.Models[0].Provider != "Provider display" {
		t.Fatalf("options: %+v", options)
	}
	b, _ := json.Marshal(options)
	for _, forbidden := range []string{"secret", "never-public", "private prompt", "apiKey", "options"} {
		if strings.Contains(string(b), forbidden) {
			t.Fatalf("catalog leaked %s", forbidden)
		}
	}
	if len(a.Hub.Sessions()) != 0 || len(a.runtimes) != 0 {
		t.Fatal("catalog activated a session or leaked a child")
	}
	for _, l := range []hub.Launch{{AgentName: "child"}, {Model: "offline/other"}, {Model: "provider/org/model", Variant: "disabled"}, {Effort: "high"}, {Local: &hub.LocalModel{}}} {
		l.Dir = a.Home
		if id, err := a.Start(context.Background(), l); id != "" || !errors.Is(err, hub.ErrLaunchInvalid) {
			t.Fatalf("accepted %+v: %q %v", l, id, err)
		}
	}
	if len(requests(t, a, "/api/session")) != 0 || len(a.runtimes) != 0 {
		t.Fatal("invalid start created native data or leaked a child")
	}
}

func TestManagedCancellationMissingSessionAndShutdown(t *testing.T) {
	a := runtimeFixture(t)
	if err := a.Resume(context.Background(), "ses_missing", a.Home, ""); err == nil {
		t.Fatal("missing native session accepted")
	}
	if len(requests(t, a, "/api/session")) != 0 || len(a.runtimes) != 0 {
		t.Fatal("missing session fell back to create")
	}
	t.Setenv("SLORP_TEST_HANG", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := a.Start(ctx, hub.Launch{Dir: a.Home}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if len(a.runtimes) != 0 {
		t.Fatal("cancelled child remains")
	}
	t.Setenv("SLORP_TEST_HANG", "")
	id, err := a.Start(context.Background(), hub.Launch{Dir: a.Home})
	if err != nil {
		t.Fatal(err)
	}
	a.Shutdown()
	if len(a.runtimes) != 0 || len(a.Hub.Sessions()) != 0 {
		t.Fatal("shutdown leaked ownership")
	}
	if _, ok := a.Hub.Archive(id); !ok {
		t.Fatal("empty managed session not archived")
	}
	if _, err := a.Start(context.Background(), hub.Launch{Dir: a.Home}); err == nil {
		t.Fatal("start after shutdown")
	}
}

func TestManagedConcurrentOperationsConflict(t *testing.T) {
	a := runtimeFixture(t)
	a.initLifecycle()
	a.lifecycleMu.Lock()
	_, err := a.Start(context.Background(), hub.Launch{Dir: a.Home})
	a.lifecycleMu.Unlock()
	if !errors.Is(err, hub.ErrLifecycleConflict) {
		t.Fatal(err)
	}
}

func TestManagedUnexpectedExitFencesAndArchives(t *testing.T) {
	a := runtimeFixture(t)
	id, err := a.Start(context.Background(), hub.Launch{Dir: a.Home})
	if err != nil {
		t.Fatal(err)
	}
	s := a.Hub.Sessions()[0]
	a.lifecycleMu.Lock()
	r := a.runtimes[s.Job]
	a.lifecycleMu.Unlock()
	if err := r.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, ok := a.Hub.Archive(id); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("unexpected child exit did not archive")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, _, err := a.Hub.BridgeLease(context.Background(), r.token, Agent, id, "", "/api/bridge/poll"); err == nil {
		t.Fatal("dead child token accepted")
	}
	if _, err := os.Stat(filepath.Join(r.private, "session.json")); !os.IsNotExist(err) {
		t.Fatal("dead child control file retained", err)
	}
}

func TestStopDoesNotWaitForUnrelatedStartupOrCatalog(t *testing.T) {
	a := runtimeFixture(t)
	id, err := a.Start(context.Background(), hub.Launch{Dir: a.Home})
	if err != nil {
		t.Fatal(err)
	}
	job := a.Hub.Sessions()[0].Job
	a.lifecycleMu.Lock()
	err = a.Stop(context.Background(), job)
	a.lifecycleMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Hub.Archive(id); !ok {
		t.Fatal("binding cleanup did not archive owned session")
	}
}

func TestStopRacesShutdown(t *testing.T) {
	a := runtimeFixture(t)
	id, err := a.Start(context.Background(), hub.Launch{Dir: a.Home})
	if err != nil {
		t.Fatal(err)
	}
	job := a.Hub.Sessions()[0].Job
	stopped := make(chan error, 1)
	closed := make(chan struct{})
	go func() { stopped <- a.Stop(context.Background(), job) }()
	go func() { a.Shutdown(); close(closed) }()
	if err := <-stopped; err != nil && !errors.Is(err, hub.ErrLifecycleConflict) {
		t.Fatal(err)
	}
	<-closed
	if _, ok := a.Hub.Archive(id); !ok {
		t.Fatal("concurrent cleanup lost archive")
	}
	if len(a.runtimes) != 0 || len(a.Hub.Sessions()) != 0 {
		t.Fatal("concurrent cleanup leaked runtime")
	}
}
