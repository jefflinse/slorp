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
	"strings"

	"github.com/jefflinse/slorp/internal/claude"
	"github.com/jefflinse/slorp/internal/hub"
)

type launchCatalog interface {
	LaunchOptions(context.Context, string) (hub.LaunchOptions, error)
}

type resumeCommander interface {
	ResumeCommand(id, dir string) string
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

func lifecycleRoutes(mux *http.ServeMux, h *hub.Hub, launchers map[string]Launcher) {
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
		var command string
		if launcher, ok := launchers[agent].(resumeCommander); ok {
			command = launcher.ResumeCommand(id, dir)
		} else if agent == claude.Agent && launchers[agent] != nil {
			bin := "claude"
			if a, ok := launchers[agent].(*claude.Adapter); ok {
				bin = a.Bin
			}
			command = "cd -- " + hub.ShellQuote(dir) + " && " + hub.ShellQuote(bin) + " --resume " + hub.ShellQuote(id)
		} else {
			http.Error(w, "this agent has no resume command", http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"cwd": dir, "command": command})
	})
}
