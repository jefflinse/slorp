package opencode

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jefflinse/slorp/internal/hub"
)

const startupTimeout = 45 * time.Second
const requestTimeout = 15 * time.Second

type runtime struct {
	cmd                                      *exec.Cmd
	liveness                                 *os.File
	done                                     chan struct{}
	base, password, token, job, dir, private string
	session                                  nativeSession
	client                                   *http.Client
	snapshot                                 *configSnapshot
	stopping                                 atomic.Bool
	busy                                     atomic.Bool
	cleanup                                  sync.Once
}

type nativeModel struct {
	ProviderID string `json:"providerID"`
	ID         string `json:"id"`
	Variant    string `json:"variant,omitempty"`
}

type nativeSession struct {
	ID        string                           `json:"id"`
	ParentID  string                           `json:"parentID"`
	Directory string                           `json:"directory"`
	Title     string                           `json:"title"`
	Agent     string                           `json:"agent"`
	Model     *nativeModel                     `json:"model"`
	Time      struct{ Created, Updated int64 } `json:"time"`
}

func opaque() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read cannot fail on supported Go versions.
	return hex.EncodeToString(b)
}

func (a *Adapter) initLifecycle() {
	a.lifeOnce.Do(func() {
		a.life, a.cancelLife = context.WithCancel(context.Background())
		a.runtimes = map[string]*runtime{}
	})
}

func (a *Adapter) operation(ctx context.Context) (context.Context, func(), error) {
	a.initLifecycle()
	if !a.lifecycleMu.TryLock() {
		return nil, nil, hub.ErrLifecycleConflict
	}
	if a.life.Err() != nil {
		a.lifecycleMu.Unlock()
		return nil, nil, errors.New("OpenCode lifecycle is shut down")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*startupTimeout)
	stop := context.AfterFunc(a.life, cancel)
	return ctx, func() { stop(); cancel(); a.lifecycleMu.Unlock() }, nil
}

// Shutdown terminates only children started by this adapter and drains their bridge requests.
func (a *Adapter) Shutdown() {
	a.initLifecycle()
	a.cancelLife()
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()
	a.runtimeMu.Lock()
	runtimes := make([]*runtime, 0, len(a.runtimes))
	for _, r := range a.runtimes {
		runtimes = append(runtimes, r)
	}
	a.runtimeMu.Unlock()
	var closed sync.WaitGroup
	for _, r := range runtimes {
		closed.Add(1)
		go func() { defer closed.Done(); a.finish(r) }()
	}
	closed.Wait()
}

func replaceEnv(cmd *exec.Cmd, key, value string) {
	cmd.Env = slices.DeleteFunc(cmd.Env, func(s string) bool { return strings.HasPrefix(s, key+"=") })
	if value != "" {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
}

var listeningURL = regexp.MustCompile(`opencode server listening on (http://127\.0\.0\.1:[0-9]+)\r?\n`)

type readyOutput struct {
	mu    sync.Mutex
	buf   []byte
	ready chan string
}

func (o *readyOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.buf = append(o.buf, p...)
	if m := listeningURL.FindSubmatch(o.buf); len(m) == 2 {
		select {
		case o.ready <- string(m[1]):
		default:
		}
	}
	if len(o.buf) > 8192 {
		o.buf = o.buf[len(o.buf)-8192:]
	}
	return len(p), nil
}

func (a *Adapter) spawn(ctx context.Context, dir string, config map[string]any) (*runtime, error) {
	private, err := os.MkdirTemp("", "slorp-opencode-run-*")
	if err != nil {
		return nil, fmt.Errorf("creating OpenCode runtime directory: %w", err)
	}
	r := &runtime{done: make(chan struct{}), password: opaque(), token: opaque(), job: opaque(), dir: dir, private: private,
		client: &http.Client{Timeout: requestTimeout, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	r.busy.Store(true)
	args := []string{"serve", "--hostname", "127.0.0.1", "--port", "0", "--mdns=false"}
	if config == nil {
		args = append(args, "--pure")
	}
	cmd := a.command(context.Background(), args...)
	r.cmd, cmd.Dir = cmd, dir
	cmd.WaitDelay = 2 * time.Second
	replaceEnv(cmd, "OPENCODE_SERVER_PASSWORD", r.password)
	replaceEnv(cmd, "OPENCODE_SERVER_USERNAME", "opencode")
	replaceEnv(cmd, "SLORP_URL", a.URL)
	replaceEnv(cmd, "SLORP_MANAGED_FILE", filepath.Join(private, "session.json"))
	replaceEnv(cmd, "SLORP_MANAGED_RUN", r.token)
	if config != nil {
		if err := a.runtimeConfig(r, config); err != nil {
			removePrivate(private)
			return nil, err
		}
	} else {
		r.snapshot, err = a.snapshotConfig(ctx, r)
		if err != nil {
			removePrivate(private)
			return nil, fmt.Errorf("copying OpenCode configuration: %w", err)
		}
	}
	output := &readyOutput{ready: make(chan string, 1)}
	cmd.Stdout, cmd.Stderr = output, output
	cmd, r.liveness, err = supervisedCommand(cmd, private)
	if err != nil {
		removePrivate(private)
		return nil, fmt.Errorf("starting OpenCode server: %w", err)
	}
	r.cmd = cmd
	a.runtimeMu.Lock()
	a.runtimes[r.job] = r
	a.runtimeMu.Unlock()
	go func() {
		err := cmd.Wait()
		close(r.done)
		if r.stopping.Load() {
			return
		}
		a.Hub.FenceManaged(r.token)
		if err := os.Remove(filepath.Join(r.private, "session.json")); err != nil && !os.IsNotExist(err) {
			slog.Warn("removing exited OpenCode control file", "error", err)
		}
		a.lifecycleMu.Lock()
		defer a.lifecycleMu.Unlock()
		a.runtimeMu.Lock()
		owned := a.runtimes[r.job] == r
		a.runtimeMu.Unlock()
		if owned {
			if err != nil && a.life.Err() == nil {
				slog.Warn("OpenCode runtime exited", "error", err)
			}
			a.finish(r)
		}
	}()
	startup, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	select {
	case r.base = <-output.ready:
		if err := r.request(startup, http.MethodGet, "/global/health", nil, nil); err == nil {
			return r, nil
		}
	case <-r.done:
	case <-startup.Done():
	}
	a.finish(r)
	if startup.Err() != nil {
		return nil, fmt.Errorf("starting OpenCode server: %w", startup.Err())
	}
	return nil, errors.New("OpenCode server did not become ready")
}

func (r *runtime) request(ctx context.Context, method, path string, body, result any) error {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, r.base+path+"?directory="+url.QueryEscape(r.dir), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.SetBasicAuth("opencode", r.password)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		cause := ctx.Err()
		if cause == nil && errors.Is(err, context.DeadlineExceeded) {
			cause = context.DeadlineExceeded
		}
		if method == http.MethodPost && (path == "/session" || strings.HasSuffix(path, "/prompt_async")) {
			if cause != nil {
				return fmt.Errorf("%w: %w", hub.ErrLifecycleUnknown, cause)
			}
			return fmt.Errorf("%w: OpenCode %s response was not received", hub.ErrLifecycleUnknown, path)
		}
		if cause != nil {
			return fmt.Errorf("OpenCode request cancelled: %w", cause)
		}
		return fmt.Errorf("OpenCode %s %s request failed", method, path)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if method == http.MethodPost && resp.StatusCode >= 500 && (path == "/session" || strings.HasSuffix(path, "/prompt_async")) {
			return fmt.Errorf("%w: OpenCode returned HTTP %d", hub.ErrLifecycleUnknown, resp.StatusCode)
		}
		return fmt.Errorf("OpenCode %s %s returned HTTP %d", method, path, resp.StatusCode)
	}
	if result == nil {
		if resp.StatusCode == http.StatusNoContent || method == http.MethodPost && strings.HasSuffix(path, "/prompt_async") {
			return nil
		}
		_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return err
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(result); err != nil {
		if method == http.MethodPost && path == "/session" {
			if ctx.Err() != nil {
				return fmt.Errorf("%w: %w", hub.ErrLifecycleUnknown, ctx.Err())
			}
			if errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("%w: %w", hub.ErrLifecycleUnknown, context.DeadlineExceeded)
			}
			return fmt.Errorf("%w: native session response could not be decoded", hub.ErrLifecycleUnknown)
		}
		return fmt.Errorf("decoding OpenCode %s: %w", path, err)
	}
	return nil
}

func (a *Adapter) finish(r *runtime) {
	r.cleanup.Do(func() {
		r.stopping.Store(true)
		a.Hub.FenceManaged(r.token)
		if err := os.Remove(filepath.Join(r.private, "session.json")); err != nil && !os.IsNotExist(err) {
			slog.Warn("removing OpenCode control file", "error", err)
		}
		if r.session.ID != "" {
			abortCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			err := r.request(abortCtx, http.MethodPost, "/session/"+r.session.ID+"/abort", nil, nil)
			cancel()
			if err != nil {
				slog.Warn("aborting OpenCode before group shutdown", "session_id", r.session.ID, "error", err)
			}
		}
		if err := r.liveness.Close(); err != nil {
			slog.Warn("closing OpenCode supervisor liveness pipe", "error", err)
		}
		select {
		case <-r.done:
		case <-time.After(supervisorGrace + 4*time.Second):
			slog.Error("OpenCode supervisor did not finish cleanup")
		}
		a.Hub.CloseManaged(r.token)
		r.client.CloseIdleConnections()
		a.runtimeMu.Lock()
		delete(a.runtimes, r.job)
		a.runtimeMu.Unlock()
	})
}

func removePrivate(path string) {
	if err := os.RemoveAll(path); err != nil {
		slog.Warn("removing OpenCode runtime directory", "error", err)
	}
}

// configured resolves native configuration without loading external plugins, then
// runs its snapshot privately so replacing Slorp cannot also load an older copy.
func (a *Adapter) configured(ctx context.Context, dir string) (*runtime, error) {
	probe, err := a.spawn(ctx, dir, nil)
	if err != nil {
		return nil, err
	}
	var config map[string]any
	err = probe.request(ctx, http.MethodGet, "/config", nil, &config)
	if err == nil {
		probe.snapshot.original(config)
	}
	a.finish(probe)
	if err != nil {
		return nil, err
	}
	if config == nil {
		return nil, errors.New("OpenCode returned no configuration")
	}
	return a.spawn(ctx, dir, config)
}

func (a *Adapter) activate(ctx context.Context, r *runtime, prompt string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-r.done:
		return errors.New("OpenCode runtime exited before activation")
	default:
	}
	s := r.session
	_, retained := a.Hub.Archive(s.ID)
	if err := a.Hub.OpenManaged(Agent, s.ID, r.job, r.token, func(h *hub.Session) {
		if h.Cwd == "" {
			h.Cwd = r.dir
		}
		if s.Title != "" {
			h.Title = s.Title
		}
		if (!retained || h.StartedAt.IsZero()) && s.Time.Created > 0 {
			h.StartedAt = time.UnixMilli(s.Time.Created)
		}
		if s.Model != nil {
			h.Model, h.Reasoning = s.Model.ProviderID+"/"+s.Model.ID, s.Model.Variant
		}
	}); err != nil {
		return err
	}
	b, _ := json.Marshal(map[string]string{"sessionId": s.ID})
	if err := atomicFile(filepath.Join(r.private, "session.json"), b, 0o600); err != nil {
		return fmt.Errorf("activating OpenCode bridge: %w", err)
	}
	if prompt != "" {
		body := map[string]any{"parts": []map[string]string{{"type": "text", "text": prompt}}}
		if s.Agent != "" {
			body["agent"] = s.Agent
		}
		if s.Model != nil {
			body["model"] = map[string]string{"providerID": s.Model.ProviderID, "modelID": s.Model.ID}
			if s.Model.Variant != "" {
				body["variant"] = s.Model.Variant
			}
		}
		if err := r.request(ctx, http.MethodPost, "/session/"+s.ID+"/prompt_async", body, nil); err != nil {
			return err
		}
		return nil
	}
	return nil
}

// Start creates a native root session and owns its server until stopped or shut down.
func (a *Adapter) Start(ctx context.Context, l hub.Launch) (id string, err error) {
	ctx, release, err := a.operation(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	if l.Local != nil || l.Effort != "" {
		return "", fmt.Errorf("%w: local and effortLevel are Claude-only", hub.ErrLaunchInvalid)
	}
	r, err := a.configured(ctx, l.Dir)
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			a.finish(r)
		}
	}()
	catalog, err := r.catalog(ctx)
	if err != nil {
		return "", err
	}
	if err := catalog.choose(&l); err != nil {
		return "", err
	}
	provider, model, _ := strings.Cut(l.Model, "/")
	body := map[string]any{"agent": l.AgentName, "model": nativeModel{ProviderID: provider, ID: model, Variant: l.Variant}}
	if l.Name != "" {
		body["title"] = l.Name
	}
	if err = r.request(ctx, http.MethodPost, "/session", body, &r.session); err != nil {
		return "", err
	}
	if !sessionIDPattern.MatchString(r.session.ID) {
		return "", fmt.Errorf("%w: OpenCode did not return a valid native session identity", hub.ErrLifecycleUnknown)
	}
	id = r.session.ID
	if err = hub.CheckpointLaunch(ctx, id); err != nil {
		return id, fmt.Errorf("%w: %w", hub.ErrLifecycleUnknown, err)
	}
	if r.session.ParentID != "" {
		return id, errors.New("OpenCode did not create a root session")
	}
	if r.session.Model == nil {
		r.session.Model = &nativeModel{ProviderID: provider, ID: model, Variant: l.Variant}
	}
	if r.session.Agent == "" {
		r.session.Agent = l.AgentName
	}
	err = a.activate(ctx, r, l.Prompt)
	if err == nil {
		hub.ReportLaunchJob(ctx, id, r.job)
		slog.Info("OpenCode session started", "session_id", id)
		r.busy.Store(false)
	}
	return id, err
}

// Resume opens exactly the retained native session; a missing session is an error.
func (a *Adapter) Resume(ctx context.Context, id, dir, prompt string) (err error) {
	ctx, release, err := a.operation(ctx)
	if err != nil {
		return err
	}
	defer release()
	if !sessionIDPattern.MatchString(id) {
		return fmt.Errorf("%w: invalid OpenCode session ID", hub.ErrLaunchInvalid)
	}
	for _, s := range a.Hub.Sessions() {
		if s.ID == id {
			return hub.ErrLifecycleConflict
		}
	}
	r, err := a.configured(ctx, dir)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			a.finish(r)
		}
	}()
	if err = r.request(ctx, http.MethodGet, "/session/"+id, nil, &r.session); err != nil {
		return err
	}
	if r.session.ID != id || r.session.ParentID != "" {
		return errors.New("OpenCode did not return the requested root session")
	}
	if err = r.restoreChoices(ctx); err != nil {
		return err
	}
	err = a.activate(ctx, r, prompt)
	if err == nil {
		slog.Info("OpenCode session reopened", "session_id", id)
		r.busy.Store(false)
	}
	return err
}

func (r *runtime) restoreChoices(ctx context.Context) error {
	if r.session.Agent != "" && r.session.Model != nil && r.session.Model.ID != "" && r.session.Model.ProviderID != "" {
		return nil
	}
	type message struct {
		Info struct {
			ID        string                                         `json:"id"`
			SessionID string                                         `json:"sessionID"`
			Role      string                                         `json:"role"`
			Agent     string                                         `json:"agent"`
			Model     *struct{ ProviderID, ModelID, Variant string } `json:"model"`
			Time      struct{ Created int64 }                        `json:"time"`
		} `json:"info"`
	}
	var messages []message
	if err := r.request(ctx, http.MethodGet, "/session/"+r.session.ID+"/message", nil, &messages); err != nil {
		return fmt.Errorf("recovering OpenCode resume choices: %w", err)
	}
	var latest *message
	for i := range messages {
		m := &messages[i]
		if m.Info.Role != "user" || m.Info.SessionID != r.session.ID {
			continue
		}
		if latest == nil || m.Info.Time.Created > latest.Info.Time.Created || m.Info.Time.Created == latest.Info.Time.Created && m.Info.ID > latest.Info.ID {
			latest = m
		}
	}
	if latest == nil {
		return nil
	}
	if r.session.Agent == "" {
		r.session.Agent = latest.Info.Agent
	}
	if m := latest.Info.Model; m != nil {
		if r.session.Model == nil {
			r.session.Model = &nativeModel{ProviderID: m.ProviderID, ID: m.ModelID, Variant: m.Variant}
		} else {
			if r.session.Model.ID == "" {
				r.session.Model.ID = m.ModelID
			}
			if r.session.Model.ProviderID == "" {
				r.session.Model.ProviderID = m.ProviderID
			}
		}
	}
	return nil
}

// Stop aborts the native turn and terminates only the process owned by job.
func (a *Adapter) Stop(ctx context.Context, job string) error {
	a.initLifecycle()
	a.runtimeMu.Lock()
	r := a.runtimes[job]
	a.runtimeMu.Unlock()
	if r == nil || !r.busy.CompareAndSwap(false, true) {
		return hub.ErrLifecycleConflict
	}
	a.finish(r)
	slog.Info("OpenCode session stopped", "session_id", r.session.ID)
	return nil
}

// ResumeCommand returns a shell-safe command using the configured executable.
func (a *Adapter) ResumeCommand(id, dir string) string {
	return "cd -- " + hub.ShellQuote(dir) + " && " + hub.ShellQuote(a.Bin) + " --session " + hub.ShellQuote(id)
}
