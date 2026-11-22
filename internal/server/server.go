// Package server exposes the hub over HTTP: a JSON and server-sent-events API
// for the web UI, and the endpoints an in-agent bridge reports to and polls.
package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/jefflinse/slorp/internal/claude"
	"github.com/jefflinse/slorp/internal/enrich"
	"github.com/jefflinse/slorp/internal/hub"
	"github.com/jefflinse/slorp/internal/integrations"
	"github.com/jefflinse/slorp/internal/kb"
	"github.com/jefflinse/slorp/internal/opencode"
	"github.com/jefflinse/slorp/internal/terminal"
)

// modelName is what an agent's name for one of its models looks like.
var modelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/\[\]-]{0,99}$`)

// rewritable are the tasks the local model can be asked to write again for.
var rewritable = []string{hub.ActDescribe, hub.ActFile, hub.ActRole, hub.ActNote, hub.ActBrief}

// agents are the agents a bridge may report for.
var agents = map[string]bool{claude.Agent: true, opencode.Agent: true}

const (
	// nameLimit is how long a name may be, in runes.
	nameLimit = 80
	// promptLimit is how long instructions to the local model may be, in
	// runes.
	promptLimit = 20000
	// maxLimits is how many usage windows one bridge may report.
	maxLimits = 8
	// startLimit is how long the first prompt of a session started here may
	// be, in bytes. It travels as a command-line argument.
	startLimit = 200_000
	// subagentsHeard is how many subagents one bridge may report, doingLimit
	// how much of what each says it is doing is kept, in runes, and
	// samplesKept how many of its token readings.
	subagentsHeard = 200
	doingLimit     = 300
	samplesKept    = 16
)

// first returns the first n runes of s.
func first(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// agentOf returns the agent a bridge names, Claude Code's when it names
// none, and refuses one Slorp does not know.
func agentOf(w http.ResponseWriter, name string) (string, bool) {
	if name == "" {
		return claude.Agent, true
	}
	if !agents[name] {
		http.Error(w, "unknown agent", http.StatusBadRequest)
		return "", false
	}
	return name, true
}

// Feeds is what the server asks of whatever collects the integrations' feeds.
type Feeds interface {
	// Check collects every feed now.
	Check()
	// Rescan looks again at which repositories the efforts' directories hold.
	Rescan()
}

// History reads a session's own record, as its agent keeps it. Either
// reading is nil where the agent's record cannot give it.
type History struct {
	// Exchanges returns what was said in a session, oldest first and at
	// most limit of it.
	Exchanges func(ctx context.Context, sessionID string, limit int) ([]hub.Exchange, error)
	// Reply returns in full the latest message the agent wrote in a session
	// that ends with ends, empty when the record holds none.
	Reply func(ctx context.Context, sessionID, ends string) (string, error)
}

type agentCapabilities struct {
	History       bool `json:"history"`
	Start         bool `json:"start"`
	Resume        bool `json:"resume"`
	LaunchOptions bool `json:"launchOptions"`
	ResumeCommand bool `json:"resumeCommand"`
	// Attach is whether a session of its running in the background can be
	// opened in a terminal.
	Attach bool `json:"attach"`
}

// Launcher starts, reopens and stops one agent's sessions in the background.
type Launcher interface {
	// Start begins the session l describes. It returns the id the session
	// goes by, empty when the agent had not listed it yet.
	// Managed launchers report the created job with hub.ReportLaunchJob before returning.
	Start(ctx context.Context, l hub.Launch) (sessionID string, err error)
	// Resume reopens a closed session that worked in dir, and says prompt in
	// it unless that is empty.
	Resume(ctx context.Context, sessionID, dir, prompt string) error
	// Stop ends a session by the id its agent's commands take, keeping its
	// conversation.
	Stop(ctx context.Context, job string) error
}

// Terminals opens windows of the terminal emulators on this machine.
type Terminals interface {
	// Apps lists those installed, and Default names the one to use when
	// the user has chosen none, empty when there are none.
	Apps() []terminal.App
	Default() string
	// Open opens a new window of one of Apps and runs a shell command in it.
	Open(ctx context.Context, app, command string) error
}

// machine is what the page is told of where Slorp runs: the system, the
// terminal emulators it can open a session in, and the one it opens them in
// until the user chooses another.
type machine struct {
	Platform string `json:"platform"`
	Terminal struct {
		Apps    []terminal.App `json:"apps"`
		Default string         `json:"default"`
	} `json:"terminal"`
}

// PluginManager installs and maintains an agent's bridge independently of launching sessions.
type PluginManager interface {
	// Install gives the agent the Slorp plugin, or turns it back on.
	Install(ctx context.Context) error
	// Update brings the agent's Slorp plugin up to the newest version.
	Update(ctx context.Context) error
	// Uninstall takes the Slorp plugin away from the agent.
	Uninstall(ctx context.Context) error
}

// Upgrader replaces the Slorp running with the newest release.
type Upgrader interface {
	// Upgrade installs the newest release, which Slorp runs once restarted.
	Upgrade(ctx context.Context) error
	// Restart starts Slorp again.
	Restart()
}

// Delegates reads from an agent's own record the subagents its sessions
// started, for sessions that are open and ones that have closed.
type Delegates interface {
	// Subagents lists those a session started, the first started first.
	Subagents(sessionID string) []hub.Subagent
	// Report returns what one of them was asked and what it reported, and
	// false when the session started no such subagent.
	Report(sessionID, id string) (hub.SubagentReport, bool)
}

// Prober tries a local model server.
type Prober interface {
	// Probe tries the server and model in use, or with m those m would
	// have Slorp use.
	Probe(ctx context.Context, m *hub.ModelSettings) hub.ModelCheck
}

// modelURL returns the address of a model server as the user wrote it, in
// the form Slorp keeps: with a scheme, and without the /v1 its API paths
// begin with. Empty stays empty. It reports false for anything that is not
// an http or https address.
func modelURL(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", true
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return "", false
	}
	return u.Scheme + "://" + u.Host + strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/v1"), true
}

// prompts returns what the user has the local model told, in the form Slorp
// keeps it: trimmed, and with nothing for a task whose instructions are
// empty or Slorp's own. It is an error to name a task the model does not
// do, to write more than promptLimit, or to say other than whether to
// rewrite.
func prompts(p hub.Prompts) (hub.Prompts, error) {
	tidy := func(s string) (string, error) {
		if s = strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n")); utf8.RuneCountInString(s) > promptLimit {
			return "", fmt.Errorf("instructions to the local model may be at most %d characters long", promptLimit)
		}
		return s, nil
	}
	own := enrich.Defaults()
	out := hub.Prompts{Tasks: map[string]string{}, Rewrite: p.Rewrite}
	if !slices.Contains([]string{hub.RewriteAsk, hub.RewriteAlways, hub.RewriteNever}, p.Rewrite) {
		return hub.Prompts{}, fmt.Errorf("whether to rewrite is %q, %q or %q", hub.RewriteAsk, hub.RewriteAlways, hub.RewriteNever)
	}
	var err error
	if out.Global, err = tidy(p.Global); err != nil {
		return hub.Prompts{}, err
	}
	for task, text := range p.Tasks {
		standing, known := own[task]
		if !known {
			return hub.Prompts{}, fmt.Errorf("the local model does no task called %q", task)
		}
		if text, err = tidy(text); err != nil {
			return hub.Prompts{}, err
		}
		if text != "" && text != standing {
			out.Tasks[task] = text
		}
	}
	return out, nil
}

// absolute returns path as the absolute path it names, reading a leading "~"
// as the user's home.
func absolute(path string) (string, error) {
	path = strings.TrimSpace(path)
	if rest, ok := strings.CutPrefix(path, "~"); ok && (rest == "" || rest[0] == '/') {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, rest)
	}
	if !filepath.IsAbs(path) {
		return "", errors.New("dir must be an absolute path, or start with ~")
	}
	return filepath.Clean(path), nil
}

// directory returns path as absolute does, when a directory is there.
func directory(path string) (string, error) {
	path, err := absolute(path)
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", path)
	}
	return path, nil
}

// Page serves built as the web UI, or the file at path when one is given,
// read afresh for each request.
func Page(built []byte, path string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if path != "" {
			http.ServeFile(w, r, path)
			return
		}
		w.Write(built)
	})
}

// New serves the page and its API. history, launchers, plugins and delegates are by
// agent; an agent with no history keeps no record Slorp can read, one with
// no launcher has sessions Slorp can only watch, and one with no delegates
// has subagents Slorp knows nothing of. With no upgrader, Slorp cannot
// upgrade itself.
func New(h *hub.Hub, k *kb.Store, m *integrations.Manager, feeds Feeds, history map[string]History, launchers map[string]Launcher, plugins map[string]PluginManager, delegates map[string]Delegates, prober Prober, terminals Terminals, upgrader Upgrader, page http.Handler) http.Handler {
	mux := http.NewServeMux()
	here := machine{Platform: runtime.GOOS}
	here.Terminal.Apps = []terminal.App{}
	if terminals != nil {
		here.Terminal.Apps, here.Terminal.Default = terminals.Apps(), terminals.Default()
	}
	epoch := strconv.FormatInt(time.Now().UnixNano(), 36)
	capabilities := map[string]agentCapabilities{}
	for agent, read := range history {
		capabilities[agent] = agentCapabilities{History: read.Exchanges != nil}
	}
	for agent, launcher := range launchers {
		c := capabilities[agent]
		c.Start, c.Resume = launcher != nil, launcher != nil
		_, c.LaunchOptions = launcher.(launchCatalog)
		_, c.ResumeCommand = launcher.(resumeCommander)
		c.ResumeCommand = c.ResumeCommand || agent == claude.Agent && launcher != nil
		_, c.Attach = launcher.(attachCommander)
		c.Attach = c.Attach || agent == claude.Agent && launcher != nil
		capabilities[agent] = c
	}
	lifecycleRoutes(mux, h, launchers, terminals)

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		page.ServeHTTP(w, r)
	})

	mux.HandleFunc("GET /api/sessions", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, struct {
			hub.State
			machine
			Integrations []integrations.Status        `json:"integrations"`
			Capabilities map[string]agentCapabilities `json:"capabilities"`
		}{h.State(), here, m.Status(r.Context()), capabilities})
	})

	mux.HandleFunc("GET /api/actions", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, struct {
			Actions []hub.Action `json:"actions"`
		}{h.Actions()})
	})

	mux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		ch, cancel := h.Subscribe()
		defer cancel()
		fmt.Fprint(w, "data: hello\n\n")
		w.(http.Flusher).Flush()
		keepalive := time.NewTicker(20 * time.Second)
		defer keepalive.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ch:
				fmt.Fprint(w, "data: change\n\n")
			case <-keepalive.C:
				fmt.Fprint(w, ": keepalive\n\n")
			}
			w.(http.Flusher).Flush()
		}
	})

	mux.HandleFunc("POST /api/sessions/{id}/reply", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Text string `json:"text"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		if strings.TrimSpace(in.Text) == "" {
			http.Error(w, "text is required", http.StatusBadRequest)
			return
		}
		reply, ok := h.Enqueue(r.PathValue("id"), in.Text)
		if !ok {
			http.Error(w, "no such session", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusAccepted, reply)
	})

	// The page starts a session in the background, optionally filed under an
	// effort and given a role from its first moment. It runs on the model and
	// at the effort level asked for, or with local on the local model in use,
	// reached through Slorp.
	mux.HandleFunc("POST /api/sessions", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			RequestID   string `json:"requestId"`
			Agent       string `json:"agent"`
			Dir         string `json:"dir"`
			Name        string `json:"name"`
			Prompt      string `json:"prompt"`
			Effort      string `json:"effort"`
			Role        string `json:"role"`
			Model       string `json:"model"`
			AgentName   string `json:"agentName"`
			Variant     string `json:"variant"`
			EffortLevel string `json:"effortLevel"`
			Local       *struct {
				Thinking    bool     `json:"thinking"`
				Temperature *float64 `json:"temperature"`
			} `json:"local"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		agent := cmp.Or(in.Agent, claude.Agent)
		if agent == opencode.Agent && in.RequestID != "" {
			key := in.RequestID
			in.RequestID = ""
			var finish func()
			var ok bool
			w, r, finish, ok = journalLifecycle(h, w, r, key, operationFingerprint("start", "", in), "")
			if !ok {
				return
			}
			defer finish()
		}
		launcher := launchers[agent]
		prompt, name := strings.TrimSpace(in.Prompt), []rune(strings.TrimSpace(in.Name))
		start := hub.Launch{Name: string(name[:min(len(name), nameLimit)]), Prompt: prompt, Model: strings.TrimSpace(in.Model), Effort: in.EffortLevel, AgentName: strings.TrimSpace(in.AgentName), Variant: strings.TrimSpace(in.Variant)}
		var problem string
		switch {
		case launcher == nil:
			problem = "Slorp cannot start sessions of this agent"
		case in.Local != nil && agent != claude.Agent:
			problem = "only Claude Code sessions can run on the local model"
		case in.EffortLevel != "" && agent != claude.Agent:
			problem = "effortLevel is only supported for Claude Code"
		case agent == claude.Agent && (start.AgentName != "" || start.Variant != ""):
			problem = "agentName and variant are only supported for OpenCode"
		case in.Local != nil && in.Local.Temperature != nil && (*in.Local.Temperature < 0 || *in.Local.Temperature > maxTemperature):
			problem = "temperature must be between 0 and 2"
		case agent == claude.Agent && in.Local == nil && start.Model != "" && !modelName.MatchString(start.Model):
			problem = "that is not the name of a model"
		case in.Local == nil && start.Effort != "" && !slices.Contains(claude.Efforts, start.Effort):
			problem = "no such effort level"
		case prompt == "" && agent == claude.Agent:
			problem = "prompt is required"
		case len(prompt) > startLimit:
			problem = "prompt is too long"
		case in.Effort != "" && !k.Has(in.Effort):
			problem = "no such effort"
		case in.Role != "" && !k.HasRole(in.Role):
			problem = "no such role"
		}
		if problem != "" {
			http.Error(w, problem, http.StatusBadRequest)
			return
		}
		dir, err := directory(in.Dir)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		start.Dir = dir
		if in.Local != nil {
			// The session runs on whichever model is in use as it starts,
			// and keeps to it. Claude Code's effort levels mean nothing to it.
			start.Model, start.Effort = h.Summarizer().Model, ""
			if start.Model == "" {
				http.Error(w, "no local model is in use", http.StatusBadRequest)
				return
			}
			start.Local = &hub.LocalModel{URL: localBase(r.Host, in.Local.Thinking, in.Local.Temperature), Thinking: in.Local.Thinking}
		}
		createdID, createdJob := "", ""
		launchCtx := hub.WithLaunchJob(r.Context(), func(id, job string) {
			if createdJob == "" {
				createdID, createdJob = id, job
			}
		})
		id, err := launcher.Start(launchCtx, start)
		if err != nil {
			lifecycleError(w, err, id)
			return
		}
		// Without an id there is nothing to file yet; the session still
		// shows up, sorted the way any other would be.
		if id != "" && in.Effort != "" {
			err = k.Assign(id, in.Effort)
		}
		if id != "" && in.Role != "" && err == nil {
			err = k.SetRole(id, in.Role)
		}
		if err != nil {
			if agent == opencode.Agent {
				if createdID == id && createdJob != "" {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					stopErr := launcher.Stop(ctx, createdJob)
					cancel()
					if stopErr != nil {
						err = fmt.Errorf("binding session: %w; stopping created runtime: %v", err, stopErr)
					}
				} else {
					err = fmt.Errorf("binding session: %w; created runtime ownership was not reported", err)
				}
			}
			slog.ErrorContext(r.Context(), "binding launched session", "session_id", id, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"sessionId": id, "error": "session was created but its effort/role could not be bound; conversation retained"})
			return
		}
		h.Touch()
		writeJSON(w, http.StatusAccepted, map[string]string{"sessionId": id})
	})

	mux.Handle("/api/local/{thinking}/{temperature}/{rest...}", localModel(h))

	mux.HandleFunc("POST /api/sessions/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		sessions := h.Sessions()
		at := slices.IndexFunc(sessions, func(s hub.Session) bool { return s.ID == id })
		if at < 0 {
			http.Error(w, "no such session", http.StatusNotFound)
			return
		}
		s := sessions[at]
		launcher := launchers[s.Agent]
		if launcher == nil || s.Job == "" {
			http.Error(w, "only a session running in the background can be stopped from here", http.StatusBadRequest)
			return
		}
		if err := launcher.Stop(r.Context(), s.Job); err != nil {
			lifecycleError(w, err, "")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// The page reopens a closed session in the background, optionally saying
	// something in it.
	mux.HandleFunc("POST /api/archive/{id}/resume", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			RequestID string `json:"requestId"`
			Prompt    string `json:"prompt"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		key := in.RequestID
		in.RequestID = ""
		fingerprint := operationFingerprint("resume", r.PathValue("id"), in)
		a, ok := h.Archive(r.PathValue("id"))
		agent := a.Agent
		if !ok {
			for _, s := range h.Sessions() {
				if s.ID == r.PathValue("id") {
					agent = s.Agent
					break
				}
			}
		}
		if key != "" && agent != claude.Agent {
			result, exists, err := h.ReplayLifecycle(key, fingerprint)
			if err != nil {
				lifecycleError(w, err, r.PathValue("id"))
				return
			}
			if exists {
				writeJSON(w, result.Status, result)
				return
			}
		}
		if !ok {
			if agent == opencode.Agent {
				lifecycleError(w, hub.ErrLifecycleConflict, r.PathValue("id"))
				return
			}
			http.Error(w, "no such closed session", http.StatusNotFound)
			return
		}
		if a.Agent == opencode.Agent && key != "" {
			var finish func()
			w, r, finish, ok = journalLifecycle(h, w, r, key, fingerprint, a.ID)
			if !ok {
				return
			}
			defer finish()
		}
		launcher := launchers[a.Agent]
		if launcher == nil {
			http.Error(w, "Slorp cannot reopen sessions of this agent", http.StatusBadRequest)
			return
		}
		if len(in.Prompt) > startLimit {
			http.Error(w, "prompt is too long", http.StatusBadRequest)
			return
		}
		dir, err := directory(a.Cwd)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := launcher.Resume(r.Context(), a.ID, dir, strings.TrimSpace(in.Prompt)); err != nil {
			lifecycleError(w, err, a.ID)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{"sessionId": a.ID})
	})

	// edited reports a knowledge-base change, republishing state on success.
	edited := func(w http.ResponseWriter, err error) {
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		h.Touch()
		feeds.Rescan()
		w.WriteHeader(http.StatusNoContent)
	}

	mux.HandleFunc("POST /api/plugins/{agent}", func(w http.ResponseWriter, r *http.Request) {
		launcher := plugins[r.PathValue("agent")]
		if launcher == nil {
			http.Error(w, "Slorp cannot install a plugin for this agent", http.StatusNotFound)
			return
		}
		if err := launcher.Install(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /api/plugins/{agent}/update", func(w http.ResponseWriter, r *http.Request) {
		launcher := plugins[r.PathValue("agent")]
		if launcher == nil {
			http.Error(w, "Slorp cannot update a plugin for this agent", http.StatusNotFound)
			return
		}
		if err := launcher.Update(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("DELETE /api/plugins/{agent}", func(w http.ResponseWriter, r *http.Request) {
		launcher := plugins[r.PathValue("agent")]
		if launcher == nil {
			http.Error(w, "Slorp cannot uninstall a plugin for this agent", http.StatusNotFound)
			return
		}
		if err := launcher.Uninstall(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /api/upgrade", func(w http.ResponseWriter, r *http.Request) {
		if upgrader == nil {
			http.Error(w, "Slorp cannot upgrade itself here", http.StatusNotFound)
			return
		}
		if err := upgrader.Upgrade(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		// The answer goes out before the restart closes the connection.
		w.WriteHeader(http.StatusNoContent)
		w.(http.Flusher).Flush()
		upgrader.Restart()
	})

	mux.HandleFunc("POST /api/efforts", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name  string `json:"name"`
			About string `json:"about"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		_, err := k.Create(in.Name, in.About)
		edited(w, err)
	})

	mux.HandleFunc("DELETE /api/efforts/{id}", func(w http.ResponseWriter, r *http.Request) {
		edited(w, k.Delete(r.PathValue("id")))
	})

	// Gives an effort another name, description, colour or glyph, puts it
	// aside or brings it back; whatever is left out stays as it is.
	mux.HandleFunc("POST /api/efforts/{id}", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name    *string `json:"name"`
			About   *string `json:"about"`
			Color   *string `json:"color"`
			Glyph   *string `json:"glyph"`
			Shelved *bool   `json:"shelved"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		id := r.PathValue("id")
		var err error
		if in.Name != nil || in.About != nil || in.Color != nil || in.Glyph != nil {
			err = k.Edit(id, kb.Changes{Name: in.Name, About: in.About, Color: in.Color, Glyph: in.Glyph})
		}
		if err == nil && in.Shelved != nil {
			err = k.Shelve(id, *in.Shelved)
		}
		if err == nil && !k.Has(id) {
			err = errors.New("no such effort")
		}
		edited(w, err)
	})

	mux.HandleFunc("POST /api/efforts/{id}/merge", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Into string `json:"into"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		edited(w, h.MergeEfforts(r.PathValue("id"), in.Into))
	})

	mux.HandleFunc("GET /api/efforts/{id}/past", func(w http.ResponseWriter, r *http.Request) {
		past, ok := h.Past(r.PathValue("id"))
		if !ok {
			http.Error(w, "no such effort", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, past)
	})

	mux.HandleFunc("DELETE /api/efforts/{id}/dirs", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Dir string `json:"dir"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		edited(w, k.RemoveDir(r.PathValue("id"), in.Dir))
	})

	mux.HandleFunc("POST /api/efforts/{id}/move", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Before string `json:"before"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		edited(w, k.Move(r.PathValue("id"), in.Before))
	})

	mux.HandleFunc("POST /api/efforts/{id}/dirs", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Dir string `json:"dir"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		edited(w, k.AddDir(r.PathValue("id"), in.Dir))
	})

	mux.HandleFunc("POST /api/kb/{section}", func(w http.ResponseWriter, r *http.Request) {
		var in kb.Entry
		if !readJSON(w, r, &in) {
			return
		}
		if r.PathValue("section") == kb.SectionWorkspaces {
			dir, err := directory(in.Dir)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			in.Dir = dir
		}
		edited(w, k.Add(r.PathValue("section"), in))
	})

	mux.HandleFunc("DELETE /api/kb/{section}/{id}", func(w http.ResponseWriter, r *http.Request) {
		edited(w, k.Remove(r.PathValue("section"), r.PathValue("id")))
	})

	// profile lays what a request said of a profile over p, and makes its
	// directory, which need not be there yet, absolute.
	profile := func(said json.RawMessage, p *kb.Profile) error {
		if err := json.Unmarshal(said, p); err != nil {
			return errors.New("bad request body")
		}
		if p.Dir = strings.TrimSpace(p.Dir); p.Dir == "" {
			return nil
		}
		dir, err := absolute(p.Dir)
		p.Dir = dir
		return err
	}

	mux.HandleFunc("POST /api/profiles", func(w http.ResponseWriter, r *http.Request) {
		var said json.RawMessage
		if !readJSON(w, r, &said) {
			return
		}
		var p kb.Profile
		err := profile(said, &p)
		var id string
		if err == nil {
			id, err = k.AddProfile(p)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		h.Touch()
		writeJSON(w, http.StatusOK, map[string]string{"id": id})
	})

	// A change to a profile says only what is changed.
	mux.HandleFunc("POST /api/profiles/{id}", func(w http.ResponseWriter, r *http.Request) {
		var said json.RawMessage
		if !readJSON(w, r, &said) {
			return
		}
		edited(w, k.EditProfile(r.PathValue("id"), func(p *kb.Profile) error { return profile(said, p) }))
	})

	mux.HandleFunc("DELETE /api/profiles/{id}", func(w http.ResponseWriter, r *http.Request) {
		edited(w, k.RemoveProfile(r.PathValue("id")))
	})

	mux.HandleFunc("POST /api/handoffs", func(w http.ResponseWriter, r *http.Request) {
		var in kb.Handoff
		if !readJSON(w, r, &in) {
			return
		}
		id, err := k.AddHandoff(in)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		h.Touch()
		writeJSON(w, http.StatusOK, map[string]string{"id": id})
	})

	// A change to a handoff says only what is changed.
	mux.HandleFunc("POST /api/handoffs/{id}", func(w http.ResponseWriter, r *http.Request) {
		var said json.RawMessage
		if !readJSON(w, r, &said) {
			return
		}
		edited(w, k.EditHandoff(r.PathValue("id"), func(h *kb.Handoff) error {
			if json.Unmarshal(said, h) != nil {
				return errors.New("bad request body")
			}
			return nil
		}))
	})

	mux.HandleFunc("DELETE /api/handoffs/{id}", func(w http.ResponseWriter, r *http.Request) {
		edited(w, k.RemoveHandoff(r.PathValue("id")))
	})

	mux.HandleFunc("POST /api/sessions/{id}/effort", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Effort string `json:"effort"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		edited(w, k.Assign(r.PathValue("id"), in.Effort))
	})

	// recorded reads an open session's latest exchanges from its agent's own
	// record, and answers the request itself when it cannot.
	recorded := func(w http.ResponseWriter, r *http.Request) (exchanges []hub.Exchange, ok bool) {
		id := r.PathValue("id")
		sessions := h.Sessions()
		at := slices.IndexFunc(sessions, func(s hub.Session) bool { return s.ID == id })
		if at < 0 {
			http.Error(w, "no such session", http.StatusNotFound)
			return nil, false
		}
		read := history[sessions[at].Agent].Exchanges
		if read == nil {
			http.Error(w, "this agent keeps no record Slorp can read", http.StatusBadRequest)
			return nil, false
		}
		exchanges, err := read(r.Context(), id, hub.BackfillLimit)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return nil, false
		}
		if err := r.Context().Err(); err != nil {
			http.Error(w, err.Error(), http.StatusRequestTimeout)
			return nil, false
		}
		return exchanges, true
	}

	// The page asks for all of what a session last said, which Slorp keeps
	// only the end of when it is long. whole is false when the rest could not
	// be read from the session's own record, and why says what stood in the way.
	mux.HandleFunc("GET /api/sessions/{id}/response", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		sessions := h.Sessions()
		at := slices.IndexFunc(sessions, func(s hub.Session) bool { return s.ID == id })
		if at < 0 {
			http.Error(w, "no such session", http.StatusNotFound)
			return
		}
		kept := sessions[at].LastReply
		out := struct {
			Text  string `json:"text"`
			Whole bool   `json:"whole"`
			Why   string `json:"why,omitempty"`
		}{Text: kept, Whole: !replyCut(kept)}
		if read := history[sessions[at].Agent].Reply; out.Whole {
		} else if read == nil {
			out.Why = "this agent keeps no record Slorp can read"
		} else if said, err := read(r.Context(), id, strings.TrimLeft(kept, "…\uFFFD")); err != nil {
			out.Why = err.Error()
		} else if said == "" {
			out.Why = "the session's record does not hold it"
		} else {
			out.Text, out.Whole = said, true
		}
		writeJSON(w, http.StatusOK, out)
	})

	// The page asks, for one session and only when the user says so, for its
	// timeline to be filled in from the session's own record.
	mux.HandleFunc("POST /api/sessions/{id}/backfill", func(w http.ResponseWriter, r *http.Request) {
		exchanges, ok := recorded(w, r)
		if !ok {
			return
		}
		queued, ok := h.Backfill(r.PathValue("id"), exchanges)
		if !ok {
			http.Error(w, "the session closed while its history was being read", http.StatusConflict)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]int{"exchanges": len(exchanges), "queued": queued})
	})

	// The page asks, only when the user says so, for what was said in a
	// session before its latest exchange: everything from the earliest it
	// already shows, said at from, and more before that, the latest first.
	// The model is asked how each of those added came out. head is when the
	// latest exchange was said, and more whether the record holds any earlier.
	mux.HandleFunc("POST /api/sessions/{id}/exchanges", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			From time.Time `json:"from"`
			More int       `json:"more"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		exchanges, ok := recorded(w, r)
		if !ok {
			return
		}
		said := slices.DeleteFunc(slices.Clone(exchanges), func(x hub.Exchange) bool { return x.Compacted != nil })
		type exchange struct {
			At     time.Time `json:"at"`
			Prompt string    `json:"prompt"`
			Reply  string    `json:"reply"`
		}
		out := struct {
			Exchanges []exchange `json:"exchanges"`
			Head      time.Time  `json:"head,omitzero"`
			More      bool       `json:"more"`
		}{Exchanges: []exchange{}}
		var added []time.Time
		if len(said) > 0 {
			out.Head = said[len(said)-1].At
			earlier := said[:len(said)-1]
			shown := len(earlier)
			if !in.From.IsZero() {
				if i := slices.IndexFunc(earlier, func(x hub.Exchange) bool { return !x.At.Before(in.From) }); i >= 0 {
					shown = i
				}
			}
			first := max(0, shown-min(max(in.More, 0), hub.BackfillLimit))
			out.More = first > 0
			for i := len(earlier) - 1; i >= first; i-- {
				x := earlier[i]
				out.Exchanges = append(out.Exchanges, exchange{x.At, x.Prompt, x.Reply})
				if i < shown {
					added = append(added, x.At)
				}
			}
		}
		if _, ok := h.Recall(r.PathValue("id"), exchanges, added); !ok {
			http.Error(w, "the session closed while its history was being read", http.StatusConflict)
			return
		}
		writeJSON(w, http.StatusOK, out)
	})

	// The page searches the sessions, open and closed.
	mux.HandleFunc("POST /api/search", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Query string `json:"q"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"hits": h.Search(in.Query)})
	})

	mux.HandleFunc("GET /api/archive/{id}", func(w http.ResponseWriter, r *http.Request) {
		a, ok := h.Archive(r.PathValue("id"))
		if !ok {
			http.Error(w, "no such closed session", http.StatusNotFound)
			return
		}
		if d := delegates[a.Agent]; d != nil {
			a.Subagents = archiveSubagents(d.Subagents(a.ID), a.Subagents)
		}
		writeJSON(w, http.StatusOK, a)
	})

	// What one subagent of a session, open or closed, was asked and reported.
	mux.HandleFunc("GET /api/sessions/{id}/subagents/{sub}", func(w http.ResponseWriter, r *http.Request) {
		id, agent := r.PathValue("id"), ""
		sessions := h.Sessions()
		if at := slices.IndexFunc(sessions, func(s hub.Session) bool { return s.ID == id }); at >= 0 {
			agent = sessions[at].Agent
		} else if a, ok := h.Archive(id); ok {
			agent = a.Agent
		}
		if agent != "" {
			if report, ok := h.RetainedSubagentReport(agent, id, r.PathValue("sub")); ok {
				writeJSON(w, http.StatusOK, report)
				return
			}
		}
		d := delegates[agent]
		if d == nil {
			http.Error(w, "no such session with subagents Slorp can read", http.StatusNotFound)
			return
		}
		report, ok := d.Report(id, r.PathValue("sub"))
		if !ok {
			http.Error(w, "no such subagent", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, report)
	})

	// The page asks for a day, named as "2006-01-02", to be wrapped up.
	mux.HandleFunc("POST /api/journal/{day}", func(w http.ResponseWriter, r *http.Request) {
		if !h.AskWrapup(r.PathValue("day")) {
			http.Error(w, "day must be like 2006-01-02", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})

	mux.HandleFunc("GET /api/journal/{day}", func(w http.ResponseWriter, r *http.Request) {
		entry, ok := h.Wrapup(r.PathValue("day"))
		if !ok {
			http.Error(w, "nothing has been written for that day", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, entry)
	})

	// The user gives a day of the journal a tag, or takes one off.
	mux.HandleFunc("POST /api/journal/{day}/tags", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Tag string `json:"tag"`
			On  bool   `json:"on"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		if !h.TagDay(r.PathValue("day"), in.Tag, in.On) {
			http.Error(w, "no such day in the journal, or no tag named", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /api/journal/search", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Query string `json:"q"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"hits": h.SearchJournal(in.Query)})
	})

	mux.HandleFunc("POST /api/sessions/{id}/name", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name string `json:"name"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		name := []rune(strings.TrimSpace(in.Name))
		if !h.Rename(r.PathValue("id"), string(name[:min(len(name), nameLimit)])) {
			http.Error(w, "no such session", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /api/sessions/{id}/role", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Role string `json:"role"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		edited(w, k.SetRole(r.PathValue("id"), in.Role))
	})

	mux.HandleFunc("POST /api/roles/{id}", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Attention string `json:"attention"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		edited(w, k.SetRoleAttention(r.PathValue("id"), in.Attention))
	})

	mux.HandleFunc("POST /api/repos/effort", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Repo   string `json:"repo"`
			Effort string `json:"effort"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		edited(w, k.AssignRepo(in.Repo, in.Effort))
	})

	// Files one pull request, named as "owner/name#number", on its own, and
	// deleting that files it with its repository again.
	mux.HandleFunc("POST /api/pulls/effort", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Pull   string `json:"pull"`
			Effort string `json:"effort"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		edited(w, k.AssignPull(in.Pull, in.Effort))
	})

	mux.HandleFunc("DELETE /api/pulls/effort", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Pull string `json:"pull"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		edited(w, k.UnassignPull(in.Pull))
	})

	// One listed pull request with its discussion. Asking says the user is
	// looking at it, so what is still to be read or written of it comes
	// first.
	mux.HandleFunc("GET /api/pulls/{source}/{number}", func(w http.ResponseWriter, r *http.Request) {
		source, repo := r.PathValue("source"), r.URL.Query().Get("repo")
		number, _ := strconv.Atoi(r.PathValue("number"))
		p, ok := h.Pull(source, repo, number)
		if !ok {
			http.Error(w, "no such pull request", http.StatusNotFound)
			return
		}
		if h.WantPull(source, repo, number) {
			feeds.Rescan()
		}
		writeJSON(w, http.StatusOK, p)
	})

	mux.HandleFunc("POST /api/feed/check", func(w http.ResponseWriter, r *http.Request) {
		m.Recheck()
		feeds.Check()
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /api/bridge/subagent", subagentHandler(h))

	mux.HandleFunc("POST /api/bridge/poll", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Agent          string          `json:"agent"`
			SessionID      string          `json:"sessionId"`
			Cwd            string          `json:"cwd"`
			Title          string          `json:"title"`
			Branch         string          `json:"branch"`
			Snapshot       *bridgeSnapshot `json:"snapshot"`
			PluginVersion  string          `json:"pluginVersion"`
			PluginRevision string          `json:"pluginRevision"`
			PluginTUI      bool            `json:"pluginTui"`
			// ContextWindow is what the session's model can take, in tokens.
			ContextWindow int `json:"contextWindow"`
			// CostUSD is the agent's own estimate of the session's cost so far.
			CostUSD float64 `json:"costUsd"`
			// RateLimits are the user's usage windows as the agent last read
			// them, at MeasuredAt in Unix milliseconds, or zero when it
			// cannot say how old the reading is.
			RateLimits []struct {
				Kind        string  `json:"kind"`
				PercentUsed float64 `json:"percentUsed"`
				ResetsAt    string  `json:"resetsAt"`
			} `json:"rateLimits"`
			MeasuredAt             int64           `json:"measuredAt"`
			ProviderLimits         json.RawMessage `json:"providerLimits"`
			ProviderLimitsEpoch    json.RawMessage `json:"providerLimitsEpoch"`
			ProviderLimitsRevision json.RawMessage `json:"providerLimitsRevision"`
			// Agents are the session's subagents as its agent has them now;
			// absent from a bridge that does not say.
			Agents *[]struct {
				ID           string  `json:"id"`
				Type         string  `json:"type"`
				Description  string  `json:"description"`
				Status       string  `json:"status"`
				Doing        string  `json:"doing"`
				ParentID     string  `json:"parentId"`
				Model        string  `json:"model"`
				StartTime    int64   `json:"startTime"`
				TokenSamples []int   `json:"tokenSamples"`
				CostUSD      float64 `json:"costUsd"`
			} `json:"agents"`
		}
		if !readJSON(w, r, &in) || in.SessionID == "" {
			return
		}
		providerLimits, err := parseProviderLimitsSnapshot(in.ProviderLimits, in.ProviderLimitsEpoch, in.ProviderLimitsRevision, time.Now())
		if err != nil {
			http.Error(w, "invalid providerLimits", http.StatusBadRequest)
			return
		}
		if r.Context().Err() != nil {
			http.Error(w, "bridge request cancelled", http.StatusConflict)
			return
		}
		agent, ok := agentOf(w, in.Agent)
		if !ok {
			return
		}
		var live []hub.Subagent
		if in.Agents != nil {
			for _, a := range (*in.Agents)[:min(len(*in.Agents), subagentsHeard)] {
				if a.ID == "" {
					continue
				}
				sub := hub.Subagent{ID: a.ID, Type: a.Type, Description: a.Description, Status: a.Status, Doing: first(strings.TrimSpace(a.Doing), doingLimit),
					ParentID: a.ParentID, Model: a.Model, CostUSD: a.CostUSD, Samples: a.TokenSamples[max(0, len(a.TokenSamples)-samplesKept):]}
				if a.StartTime > 0 {
					sub.StartedAt = time.UnixMilli(a.StartTime)
				}
				live = append(live, sub)
			}
		}
		fresh := h.Apply(agent, in.SessionID, func(s *hub.Session) {
			s.SetProviderLimits(providerLimits.epoch, providerLimits.revision, providerLimits.reports)
			in.Snapshot.apply(s)
			s.PluginVersion, s.PluginRevision = first(in.PluginVersion, nameLimit), first(in.PluginRevision, nameLimit)
			s.PluginTUI = in.PluginTUI
			if in.Agents != nil {
				s.Delegated(live)
			}
			if in.ContextWindow > 0 {
				s.ContextWindow = in.ContextWindow
			}
			if in.CostUSD > 0 {
				s.CostUSD = in.CostUSD
			}
			if s.Cwd == "" {
				s.Cwd = in.Cwd
			}
			if in.Title != "" {
				s.Title = in.Title
			}
			if in.Branch != "" {
				s.Branch = in.Branch
			}
		})
		var windows []hub.Limit
		for _, l := range in.RateLimits[:min(len(in.RateLimits), maxLimits)] {
			resets, _ := time.Parse(time.RFC3339, l.ResetsAt)
			kind := []rune(l.Kind)
			windows = append(windows, hub.Limit{Kind: string(kind[:min(len(kind), nameLimit)]), PercentUsed: max(0, l.PercentUsed), ResetsAt: resets})
		}
		var measured time.Time
		if in.MeasuredAt > 0 {
			measured = time.UnixMilli(in.MeasuredAt)
		}
		h.SetLimits(agent, measured, windows)
		replies := h.Claim(in.SessionID)
		if replies == nil {
			replies = []hub.Reply{}
		}
		// fresh tells a bridge that Slorp knew nothing of the session until now.
		writeJSON(w, http.StatusOK, map[string]any{"replies": replies, "fresh": fresh, "epoch": epoch})
	})

	mux.HandleFunc("POST /api/bridge/event", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Agent     string          `json:"agent"`
			SessionID string          `json:"sessionId"`
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			At        int64           `json:"at"`
			Snapshot  *bridgeSnapshot `json:"snapshot"`
		}
		if !readJSON(w, r, &in) || in.SessionID == "" {
			return
		}
		agent, ok := agentOf(w, in.Agent)
		if !ok {
			return
		}
		h.Apply(agent, in.SessionID, func(s *hub.Session) {
			now := time.Now()
			switch in.Type {
			case "turn.start":
				s.Status, s.WaitingFor, s.Pending, s.ActiveAt = hub.Working, "", nil, now
				if in.Text != "" {
					s.LastPrompt, s.PromptAt = in.Text, now
					if in.At > 0 {
						s.PromptAt = time.UnixMilli(in.At)
					}
				}
			case "turn.complete":
				s.Status, s.WaitingFor, s.Pending, s.ActiveAt = hub.Idle, "", nil, now
				if in.Text != "" {
					s.LastReply = claude.Tail(in.Text, claude.ReplyLimit)
				}
			case "tool.done":
				if s.Pending != nil {
					s.Status, s.WaitingFor, s.Pending = hub.Working, "", nil
				}
			case "snapshot":
				in.Snapshot.apply(s)
			}
		})
		w.WriteHeader(http.StatusNoContent)
	})

	// A bridge reports a permission prompt and learns whether Slorp may answer it.
	mux.HandleFunc("POST /api/bridge/permission", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Agent     string `json:"agent"`
			SessionID string `json:"sessionId"`
			Tool      string `json:"tool"`
			Summary   string `json:"summary"`
			Input     string `json:"input"`
			CanAnswer bool   `json:"canAnswer"`
			Question  bool   `json:"question"`
			Ordered   bool   `json:"ordered"`
			// AgentID is the subagent that asks, when one does.
			AgentID string `json:"agentId"`
		}
		if !readJSON(w, r, &in) || in.SessionID == "" {
			return
		}
		agent, ok := agentOf(w, in.Agent)
		if !ok {
			return
		}
		p := h.Ask(agent, in.SessionID, hub.Pending{Tool: in.Tool, Summary: in.Summary, Input: in.Input, Question: in.Question, Ordered: in.Ordered, AgentID: in.AgentID}, in.CanAnswer)
		writeJSON(w, http.StatusOK, map[string]any{"id": p.ID, "answerable": p.Answerable, "epoch": epoch})
	})

	// A bridge long-polls here for the user's decision in Slorp.
	mux.HandleFunc("POST /api/bridge/permission/wait", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ID string `json:"id"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		dec, ok := h.Await(r.Context(), in.ID, 20*time.Second)
		if !ok {
			dec.Behavior = "waiting"
		}
		writeJSON(w, http.StatusOK, dec)
	})

	// A bridge reports that a prompt was settled without Slorp.
	mux.HandleFunc("POST /api/bridge/permission/done", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ID string `json:"id"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		h.Done(in.ID)
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /api/permissions/{id}", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Behavior string `json:"behavior"`
			Message  string `json:"message"`
			// Answers are the options picked, by label, for each question of
			// a question prompt by its text.
			Answers    map[string][]string `json:"answers"`
			Selections [][]string          `json:"selections"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		if in.Selections != nil || len(in.Answers) > 0 {
			ok := false
			if in.Selections != nil && len(in.Answers) == 0 {
				ok = h.AnswerSelections(r.PathValue("id"), in.Selections)
			} else if in.Selections == nil {
				ok = h.Answer(r.PathValue("id"), in.Answers)
			}
			if !ok {
				http.Error(w, "that question is no longer waiting here, or those are not answers to it", http.StatusConflict)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if in.Behavior != hub.Allow && in.Behavior != hub.Deny {
			http.Error(w, "behavior must be allow or deny", http.StatusBadRequest)
			return
		}
		if !h.Decide(r.PathValue("id"), hub.Decision{Behavior: in.Behavior, Message: in.Message}) {
			http.Error(w, "that request is no longer waiting for an answer here", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /api/integrations/{id}", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Enabled bool `json:"enabled"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		if !m.Has(r.PathValue("id")) {
			http.Error(w, "no such integration", http.StatusNotFound)
			return
		}
		m.Recheck()
		h.SetIntegration(r.PathValue("id"), in.Enabled)
		feeds.Check()
		w.WriteHeader(http.StatusNoContent)
	})

	// A save says only what it changes, which is laid over the settings as
	// they are when it is kept.
	mux.HandleFunc("POST /api/settings", func(w http.ResponseWriter, r *http.Request) {
		var said json.RawMessage
		if !readJSON(w, r, &said) {
			return
		}
		behind, err := h.UpdateSettings(func(in *hub.Settings) error {
			if err := json.Unmarshal(said, in); err != nil {
				return errors.New("bad request body")
			}
			var ok bool
			if in.Model.URL, ok = modelURL(in.Model.URL); !ok {
				return errors.New("the model server's address must be an http or https one")
			}
			in.Model.Name = strings.TrimSpace(in.Model.Name)
			if in.Terminal != "" && !slices.ContainsFunc(here.Terminal.Apps, func(a terminal.App) bool { return a.ID == in.Terminal }) {
				return errors.New("no such terminal emulator is installed")
			}
			var err error
			in.Prompts, err = prompts(in.Prompts)
			return err
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(behind) > 0 {
			writeJSON(w, http.StatusOK, struct {
				// Behind are the tasks for which what the model wrote was
				// written under other instructions than it now has.
				Behind []string `json:"behind"`
			}{behind})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /api/model/rewrite", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			// Tasks are those to write again for, none for all of them.
			Tasks []string `json:"tasks"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		for _, t := range in.Tasks {
			if !slices.Contains(rewritable, t) {
				http.Error(w, fmt.Sprintf("the local model keeps nothing written for a task called %q", t), http.StatusBadRequest)
				return
			}
		}
		h.Rewrite(in.Tasks...)
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("GET /api/prompts", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, struct {
			// Defaults are Slorp's own instructions for each task.
			Defaults map[string]string `json:"defaults"`
		}{enrich.Defaults()})
	})

	mux.HandleFunc("POST /api/model/check", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			// Model is what to try in place of what is in use.
			Model *hub.ModelSettings `json:"model"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		if in.Model != nil {
			var ok bool
			if in.Model.URL, ok = modelURL(in.Model.URL); !ok {
				http.Error(w, "the model server's address must be an http or https one", http.StatusBadRequest)
				return
			}
			in.Model.Name = strings.TrimSpace(in.Model.Name)
		}
		writeJSON(w, http.StatusOK, prober.Probe(r.Context(), in.Model))
	})

	mux.HandleFunc("POST /api/bridge/ack", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ID    string         `json:"id"`
			State hub.ReplyState `json:"state"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		if in.State != hub.Delivered && in.State != hub.Dropped {
			http.Error(w, "state must be delivered or dropped", http.StatusBadRequest)
			return
		}
		h.Ack(in.ID, in.State)
		w.WriteHeader(http.StatusNoContent)
	})

	return localOnly(bridgeFenced(h, mux))
}

// localOnly refuses requests a web page on another origin could have caused.
// Replies become prompts in agent sessions, so the API must not be reachable
// by cross-site requests or DNS rebinding.
func localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("Origin") == "" && r.Header.Get("Sec-Fetch-Site") != "" && r.Header.Get("Sec-Fetch-Site") != "same-origin" {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			http.Error(w, "content type must be application/json", http.StatusUnsupportedMediaType)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// replyCut reports whether a session's last message, as Slorp keeps it, may
// be only the end of what was said: a bridge sends at most claude.ReplyLimit
// UTF-16 units of it, and a transcript gives that many runes.
func replyCut(kept string) bool {
	return len(utf16.Encode([]rune(kept))) >= claude.ReplyLimit
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
