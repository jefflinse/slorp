package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/hub"
	"github.com/jefflinse/slorp/internal/testdb"
)

type generationLauncher struct {
	h             *hub.Hub
	mu            sync.Mutex
	jobs          map[string]string
	stopped       []string
	started, bind chan struct{}
	report        bool
}

func (l *generationLauncher) Start(ctx context.Context, launch hub.Launch) (string, error) {
	if err := l.h.OpenManaged("opencode", "ses_same", "J1", "T1", func(s *hub.Session) { s.Cwd = launch.Dir }); err != nil {
		return "", err
	}
	l.mu.Lock()
	l.jobs["J1"] = "T1"
	l.mu.Unlock()
	if l.report {
		hub.ReportLaunchJob(ctx, "ses_same", "J1")
	}
	close(l.started)
	<-l.bind
	return "ses_same", nil
}

func (l *generationLauncher) Resume(_ context.Context, id, dir, _ string) error {
	if err := l.h.OpenManaged("opencode", id, "J2", "T2", func(s *hub.Session) { s.Cwd = dir }); err != nil {
		return err
	}
	l.mu.Lock()
	l.jobs["J2"] = "T2"
	l.mu.Unlock()
	return nil
}

func (l *generationLauncher) Stop(_ context.Context, job string) error {
	l.mu.Lock()
	l.stopped = append(l.stopped, job)
	token, ok := l.jobs[job]
	delete(l.jobs, job)
	l.mu.Unlock()
	if !ok {
		return hub.ErrLifecycleConflict
	}
	l.h.CloseManaged(token)
	return nil
}

func TestBindingFailureNeverStopsReplacementGeneration(t *testing.T) {
	t.Run("reported job", func(t *testing.T) { bindingFailureGeneration(t, true) })
	t.Run("missing ownership fails closed", func(t *testing.T) { bindingFailureGeneration(t, false) })
}

func bindingFailureGeneration(t *testing.T, report bool) {
	t.Helper()
	cache := testdb.Open(t)
	k := testdb.KB(t, cache)
	h := hub.New(k, cache)
	e, err := k.Create("Binding target", "")
	if err != nil {
		t.Fatal(err)
	}
	l := &generationLauncher{h: h, jobs: map[string]string{}, started: make(chan struct{}), bind: make(chan struct{}), report: report}
	handler := journalHandler(h, k, l)
	body := `{"agent":"opencode","dir":` + quotedJSON(t.TempDir()) + `,"effort":` + quotedJSON(e.ID) + `}`
	response := httptest.NewRecorder()
	finished := make(chan struct{})
	go func() {
		handler.ServeHTTP(response, journalRequest(context.Background(), "/api/sessions", body))
		close(finished)
	}()
	var release sync.Once
	defer func() { release.Do(func() { close(l.bind) }); <-finished; h.CloseManaged("T2") }()
	select {
	case <-l.started:
	case <-time.After(time.Second):
		t.Fatal("start did not reach bind handoff")
	}
	stop := httptest.NewRecorder()
	handler.ServeHTTP(stop, journalRequest(context.Background(), "/api/sessions/ses_same/stop", `{}`))
	if stop.Code != http.StatusNoContent {
		t.Fatal(stop.Code, stop.Body.String())
	}
	resume := httptest.NewRecorder()
	handler.ServeHTTP(resume, journalRequest(context.Background(), "/api/archive/ses_same/resume", `{}`))
	if resume.Code != http.StatusAccepted {
		t.Fatal(resume.Code, resume.Body.String())
	}
	if err := k.Delete(e.ID); err != nil {
		t.Fatal(err)
	}
	release.Do(func() { close(l.bind) })
	<-finished
	if response.Code != 500 || !strings.Contains(response.Body.String(), `"sessionId":"ses_same"`) {
		t.Fatal(response.Code, response.Body.String())
	}
	l.mu.Lock()
	stopped := slices.Clone(l.stopped)
	l.mu.Unlock()
	if slices.Contains(stopped, "J2") {
		t.Fatalf("binding cleanup stopped replacement generation: %v", stopped)
	}
	want := 1
	if report {
		want = 2
	}
	if len(stopped) != want {
		t.Fatalf("cleanup did not use the reported ownership: %v", stopped)
	}
	if sessions := h.Sessions(); len(sessions) != 1 || sessions[0].Job != "J2" || !sessions[0].CanStop {
		t.Fatalf("replacement lost: %+v", sessions)
	}
}
