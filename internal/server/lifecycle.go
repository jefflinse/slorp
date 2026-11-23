package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/jefflinse/slorp/internal/claude"
	"github.com/jefflinse/slorp/internal/hub"
	"github.com/jefflinse/slorp/internal/terminal"
)

type launchCatalog interface {
	LaunchOptions(context.Context, string) (hub.LaunchOptions, error)
}

type resumeCommander interface {
	ResumeCommand(id, dir string) string
}

// attachCommander is a launcher whose running sessions can be opened in a
// terminal: AttachCommand gives the shell command that opens the one running
// as job, and ok is false when it runs none.
type attachCommander interface {
	AttachCommand(job string) (command string, ok bool, err error)
}

// resumeCommand is the shell command that resumes one of an agent's closed
// sessions in a terminal, in the directory it worked in. ok is false for an
// agent that has none.
func resumeCommand(launchers map[string]Launcher, agent, id, dir string) (command string, ok bool) {
	if launcher, ok := launchers[agent].(resumeCommander); ok {
		return launcher.ResumeCommand(id, dir), true
	}
	if agent == claude.Agent && launchers[agent] != nil {
		return "cd -- " + hub.ShellQuote(dir) + " && " + hub.ShellQuote(claudeBin(launchers)) + " --resume " + hub.ShellQuote(id), true
	}
	return "", false
}

func claudeBin(launchers map[string]Launcher) string {
	if a, ok := launchers[claude.Agent].(*claude.Adapter); ok {
		return a.Bin
	}
	return "claude"
}

var (
	errNoSession     = errors.New("no such session")
	errOpenElsewhere = errors.New("this session is open in a terminal of its own; only a background or a closed session can be opened in another")
	errNoCommand     = errors.New("this agent has no resume command")
)

// terminalCommand is the shell command that opens a session in a terminal:
// one that attaches to a session running in the background, or the one that
// resumes a closed session. A session open in a terminal of its own, or
// reported open in one though not yet known, has none.
func terminalCommand(h *hub.Hub, launchers map[string]Launcher, id string) (string, error) {
	for _, s := range h.Sessions() {
		if s.ID != id {
			continue
		}
		if s.Job == "" {
			return "", errOpenElsewhere
		}
		if l, ok := launchers[s.Agent].(attachCommander); ok {
			if command, ok, err := l.AttachCommand(s.Job); ok || err != nil {
				return command, err
			}
		}
		if s.Agent != claude.Agent {
			return "", errOpenElsewhere
		}
		attach := hub.ShellQuote(claudeBin(launchers)) + " attach " + hub.ShellQuote(s.Job)
		if s.Cwd == "" {
			return attach, nil
		}
		return "cd -- " + hub.ShellQuote(s.Cwd) + " && " + attach, nil
	}
	if h.HeldOpen(id) {
		return "", errOpenElsewhere
	}
	s, ok := h.Archive(id)
	if !ok {
		return "", errNoSession
	}
	if command, ok := resumeCommand(launchers, s.Agent, id, s.Cwd); ok {
		return command, nil
	}
	return "", errNoCommand
}

func lifecycleError(w http.ResponseWriter, err error, id string) {
	code := http.StatusBadGateway
	switch {
	case errors.Is(err, hub.ErrLaunchInvalid):
		code = http.StatusBadRequest
	case errors.Is(err, hub.ErrLifecycleConflict):
		code = http.StatusConflict
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		code = http.StatusGatewayTimeout
	}
	if errors.Is(err, hub.ErrLifecycleUnknown) {
		writeJSON(w, code, map[string]string{"sessionId": id, "error": err.Error(), "outcome": "unknown"})
		return
	}
	if id != "" {
		writeJSON(w, code, map[string]string{"sessionId": id, "error": err.Error()})
		return
	}
	http.Error(w, err.Error(), code)
}

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

func operationFingerprint(kind, id string, input any) string {
	b, _ := json.Marshal(struct {
		Kind, ID string
		Input    any
	}{kind, id, input})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func replayLifecycle(h *hub.Hub, w http.ResponseWriter, key, fingerprint string) bool {
	if key == "" {
		return false
	}
	if !requestIDPattern.MatchString(key) {
		http.Error(w, "requestId must be 1–128 ASCII letters, digits, '.', '_', ':' or '-' and start with a letter or digit", http.StatusBadRequest)
		return true
	}
	result, exists, err := h.ReplayLifecycle(key, fingerprint)
	if err != nil {
		lifecycleError(w, err, "")
		return true
	}
	if exists {
		writeJSON(w, result.Status, result)
		return true
	}
	return false
}

// journalLifecycle reserves before work and persists the final response before
// network delivery. The fingerprint covers parsed parameters, not prompt text alone.
func journalLifecycle(h *hub.Hub, w http.ResponseWriter, r *http.Request, key, fingerprint, id string) (http.ResponseWriter, *http.Request, func(), bool) {
	if key == "" {
		return w, r, func() {}, true
	}
	if replayLifecycle(h, w, key, fingerprint) {
		return nil, nil, nil, false
	}
	result, admitted, err := h.BeginLifecycle(key, fingerprint, id)
	if err != nil {
		lifecycleError(w, err, id)
		return nil, nil, nil, false
	}
	if !admitted {
		writeJSON(w, result.Status, result)
		return nil, nil, nil, false
	}
	response := &bridgeResponse{header: make(http.Header)}
	ctx := hub.WithLaunchCheckpoint(r.Context(), func(id string) error { return h.CheckpointLifecycle(key, id) })
	finish := func() {
		var result hub.LifecycleResult
		if json.Unmarshal(response.body.Bytes(), &result) != nil {
			result.Error = strings.TrimSpace(response.body.String())
		}
		result.Status = response.status
		if result.Status == 0 {
			result.Status, result.Outcome, result.Error = 500, "unknown", "operation ended without a recorded response"
		}
		if result.Outcome == "" {
			result.Outcome = "error"
			if result.Status >= 200 && result.Status < 300 {
				result.Outcome = "accepted"
			}
		}
		result, err := h.CompleteLifecycle(key, result)
		if err != nil {
			slog.ErrorContext(r.Context(), "recording lifecycle result", "error", err)
			result.Status = http.StatusServiceUnavailable
		}
		writeJSON(w, result.Status, result)
	}
	return response, r.WithContext(ctx), finish, true
}

func lifecycleRoutes(mux *http.ServeMux, h *hub.Hub, launchers map[string]Launcher, terminals Terminals) {
	mux.HandleFunc("GET /api/agents/{agent}/launch-options", func(w http.ResponseWriter, r *http.Request) {
		catalog, ok := launchers[r.PathValue("agent")].(launchCatalog)
		if !ok {
			http.Error(w, "this agent has no launch options", http.StatusNotFound)
			return
		}
		dir, err := directory(r.URL.Query().Get("dir"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		options, err := catalog.LaunchOptions(r.Context(), dir)
		if err != nil {
			lifecycleError(w, err, "")
			return
		}
		writeJSON(w, http.StatusOK, options)
	})
	mux.HandleFunc("GET /api/sessions/{id}/resume-command", func(w http.ResponseWriter, r *http.Request) {
		id, agent, dir := r.PathValue("id"), "", ""
		for _, s := range h.Sessions() {
			if s.ID == id {
				agent, dir = s.Agent, s.Cwd
				break
			}
		}
		if agent == "" {
			if s, ok := h.Archive(id); ok {
				agent, dir = s.Agent, s.Cwd
			}
		}
		if agent == "" {
			http.Error(w, "no such session", http.StatusNotFound)
			return
		}
		command, ok := resumeCommand(launchers, agent, id, dir)
		if !ok {
			http.Error(w, "this agent has no resume command", http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"cwd": dir, "command": command})
	})

	// Opens a session in a window of the user's terminal emulator. The
	// command run there is Slorp's own; the request names only the session.
	mux.HandleFunc("POST /api/sessions/{id}/terminal", func(w http.ResponseWriter, r *http.Request) {
		var in struct{}
		if !readJSON(w, r, &in) {
			return
		}
		app := ""
		if terminals != nil {
			app = h.Settings().Terminal
			if !slices.ContainsFunc(terminals.Apps(), func(a terminal.App) bool { return a.ID == app }) {
				app = terminals.Default()
			}
		}
		if app == "" {
			http.Error(w, "Slorp knows no terminal emulator on this machine", http.StatusBadRequest)
			return
		}
		command, err := terminalCommand(h, launchers, r.PathValue("id"))
		switch {
		case errors.Is(err, errNoSession):
			http.Error(w, err.Error(), http.StatusNotFound)
		case errors.Is(err, errOpenElsewhere):
			http.Error(w, err.Error(), http.StatusConflict)
		case errors.Is(err, errNoCommand):
			http.Error(w, err.Error(), http.StatusBadRequest)
		case err != nil:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		default:
			if err := terminals.Open(r.Context(), app, command); err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}
	})
}
