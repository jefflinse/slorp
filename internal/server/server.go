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
)

// New returns the handler. uiPath is the page served at /.
func New(h *hub.Hub, uiPath string) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFile(w, r, uiPath)
	})

	mux.HandleFunc("GET /api/sessions", func(w http.ResponseWriter, r *http.Request) {
		sessions, items, summarizer := h.State()
		writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions, "items": items, "summarizer": summarizer})
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

	mux.HandleFunc("POST /api/bridge/poll", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			SessionID string `json:"sessionId"`
			Cwd       string `json:"cwd"`
		}
		if !readJSON(w, r, &in) || in.SessionID == "" {
			return
		}
		h.Apply(claude.Agent, in.SessionID, func(s *hub.Session) {
			if s.Cwd == "" {
				s.Cwd = in.Cwd
			}
		})
		replies := h.Claim(in.SessionID)
		if replies == nil {
			replies = []hub.Reply{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"replies": replies})
	})

	mux.HandleFunc("POST /api/bridge/event", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			SessionID string `json:"sessionId"`
			Type      string `json:"type"`
			Text      string `json:"text"`
			Tool      string `json:"tool"`
			Summary   string `json:"summary"`
		}
		if !readJSON(w, r, &in) || in.SessionID == "" {
			return
		}
		h.Apply(claude.Agent, in.SessionID, func(s *hub.Session) {
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
			case "permission":
				s.Status, s.WaitingFor, s.ActiveAt = hub.Waiting, "permission prompt", now
				s.Pending = &hub.Pending{Tool: in.Tool, Summary: in.Summary, At: now}
			case "tool.done":
				if s.Pending != nil {
					s.Status, s.WaitingFor, s.Pending = hub.Working, "", nil
				}
			}
		})
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
