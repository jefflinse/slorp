// Package server exposes the hub over HTTP: a JSON and server-sent-events API
// for the web UI, and the endpoints an in-agent bridge reports to and polls.
package server

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/jefflinse/slorp/internal/claude"
	"github.com/jefflinse/slorp/internal/hub"
	"github.com/jefflinse/slorp/internal/integrations"
	"github.com/jefflinse/slorp/internal/kb"
	"github.com/jefflinse/slorp/internal/opencode"
)

// agents are the agents a bridge may report for.
var agents = map[string]bool{claude.Agent: true, opencode.Agent: true}

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

// New returns the handler. uiPath is the page served at /.
func New(h *hub.Hub, k *kb.Store, m *integrations.Manager, feeds Feeds, uiPath string) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFile(w, r, uiPath)
	})

	mux.HandleFunc("GET /api/sessions", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, struct {
			hub.State
			Integrations []integrations.Status `json:"integrations"`
		}{h.State(), m.Status(r.Context())})
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
		edited(w, k.Add(r.PathValue("section"), in))
	})

	mux.HandleFunc("DELETE /api/kb/{section}/{id}", func(w http.ResponseWriter, r *http.Request) {
		edited(w, k.Remove(r.PathValue("section"), r.PathValue("id")))
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

	mux.HandleFunc("POST /api/feed/check", func(w http.ResponseWriter, r *http.Request) {
		m.Recheck()
		feeds.Check()
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /api/bridge/poll", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Agent     string `json:"agent"`
			SessionID string `json:"sessionId"`
			Cwd       string `json:"cwd"`
			Title     string `json:"title"`
			Branch    string `json:"branch"`
			// ContextWindow is what the session's model can take, in tokens.
			ContextWindow int `json:"contextWindow"`
		}
		if !readJSON(w, r, &in) || in.SessionID == "" {
			return
		}
		agent, ok := agentOf(w, in.Agent)
		if !ok {
			return
		}
		fresh := h.Apply(agent, in.SessionID, func(s *hub.Session) {
			if in.ContextWindow > 0 {
				s.ContextWindow = in.ContextWindow
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
		replies := h.Claim(in.SessionID)
		if replies == nil {
			replies = []hub.Reply{}
		}
		// fresh tells a bridge that Slorp knew nothing of the session until now.
		writeJSON(w, http.StatusOK, map[string]any{"replies": replies, "fresh": fresh})
	})

	mux.HandleFunc("POST /api/bridge/event", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Agent     string `json:"agent"`
			SessionID string `json:"sessionId"`
			Type      string `json:"type"`
			Text      string `json:"text"`
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
					s.LastPrompt = in.Text
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
		}
		if !readJSON(w, r, &in) || in.SessionID == "" {
			return
		}
		agent, ok := agentOf(w, in.Agent)
		if !ok {
			return
		}
		p := h.Ask(agent, in.SessionID, hub.Pending{Tool: in.Tool, Summary: in.Summary, Input: in.Input, Question: in.Question}, in.CanAnswer)
		writeJSON(w, http.StatusOK, map[string]any{"id": p.ID, "answerable": p.Answerable})
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
			Answers map[string][]string `json:"answers"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		if len(in.Answers) > 0 {
			if !h.Answer(r.PathValue("id"), in.Answers) {
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

	mux.HandleFunc("POST /api/settings", func(w http.ResponseWriter, r *http.Request) {
		in := h.Settings()
		if !readJSON(w, r, &in) {
			return
		}
		h.SetSettings(in)
		w.WriteHeader(http.StatusNoContent)
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

	return localOnly(mux)
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
