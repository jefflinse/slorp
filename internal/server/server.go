// Package server exposes the hub over HTTP: a JSON and server-sent-events API
// for the web UI, and the endpoints an in-agent bridge reports to and polls.
package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
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

const (
	// nameLimit is how long a name may be, in runes.
	nameLimit = 80
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

// History reads what was said in a session from its agent's own record,
// oldest first and at most limit of it.
type History func(sessionID string, limit int) ([]hub.Exchange, error)

// Launcher starts, reopens and stops one agent's sessions in the background.
type Launcher interface {
	// Start begins a session working in dir, named name unless that is
	// empty, with prompt as the first thing said in it. It returns the id
	// the session goes by, empty when the agent had not listed it yet.
	Start(ctx context.Context, dir, name, prompt string) (sessionID string, err error)
	// Resume reopens a closed session that worked in dir, and says prompt in
	// it unless that is empty.
	Resume(ctx context.Context, sessionID, dir, prompt string) error
	// Stop ends a session by the id its agent's commands take, keeping its
	// conversation.
	Stop(ctx context.Context, job string) error
	// Install gives the agent the Slorp plugin, or turns it back on.
	Install(ctx context.Context) error
	// Uninstall takes the Slorp plugin away from the agent.
	Uninstall(ctx context.Context) error
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

// directory returns path as the absolute directory it names, reading a
// leading "~" as the user's home.
func directory(path string) (string, error) {
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
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", path)
	}
	return filepath.Clean(path), nil
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

// New serves the page and its API. history, launchers and delegates are by
// agent; an agent with no history keeps no record Slorp can read, one with
// no launcher has sessions Slorp can only watch, and one with no delegates
// has subagents Slorp knows nothing of.
func New(h *hub.Hub, k *kb.Store, m *integrations.Manager, feeds Feeds, history map[string]History, launchers map[string]Launcher, delegates map[string]Delegates, prober Prober, page http.Handler) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		page.ServeHTTP(w, r)
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

	// The page starts a session in the background, optionally filed under an
	// effort and given a role from its first moment.
	mux.HandleFunc("POST /api/sessions", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Agent  string `json:"agent"`
			Dir    string `json:"dir"`
			Name   string `json:"name"`
			Prompt string `json:"prompt"`
			Effort string `json:"effort"`
			Role   string `json:"role"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		launcher := launchers[cmp.Or(in.Agent, claude.Agent)]
		prompt, name := strings.TrimSpace(in.Prompt), []rune(strings.TrimSpace(in.Name))
		var problem string
		switch {
		case launcher == nil:
			problem = "Slorp cannot start sessions of this agent"
		case prompt == "":
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
		id, err := launcher.Start(r.Context(), dir, string(name[:min(len(name), nameLimit)]), prompt)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
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
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		h.Touch()
		writeJSON(w, http.StatusAccepted, map[string]string{"sessionId": id})
	})

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
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// The page reopens a closed session in the background, optionally saying
	// something in it.
	mux.HandleFunc("POST /api/archive/{id}/resume", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Prompt string `json:"prompt"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		a, ok := h.Archive(r.PathValue("id"))
		if !ok {
			http.Error(w, "no such closed session", http.StatusNotFound)
			return
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
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusAccepted)
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
		launcher := launchers[r.PathValue("agent")]
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

	mux.HandleFunc("DELETE /api/plugins/{agent}", func(w http.ResponseWriter, r *http.Request) {
		launcher := launchers[r.PathValue("agent")]
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

	// The page asks, for one session and only when the user says so, for its
	// timeline to be filled in from the session's own record.
	mux.HandleFunc("POST /api/sessions/{id}/backfill", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		at := slices.IndexFunc(h.Sessions(), func(s hub.Session) bool { return s.ID == id })
		if at < 0 {
			http.Error(w, "no such session", http.StatusNotFound)
			return
		}
		read := history[h.Sessions()[at].Agent]
		if read == nil {
			http.Error(w, "this agent keeps no record Slorp can read", http.StatusBadRequest)
			return
		}
		exchanges, err := read(id, hub.BackfillLimit)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		queued, _ := h.Backfill(id, exchanges)
		writeJSON(w, http.StatusAccepted, map[string]int{"exchanges": len(exchanges), "queued": queued})
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
			a.Subagents = d.Subagents(a.ID)
		}
		writeJSON(w, http.StatusOK, a)
	})

	// What one subagent of a session, open or closed, was asked and reported.
	mux.HandleFunc("GET /api/sessions/{id}/subagents/{sub}", func(w http.ResponseWriter, r *http.Request) {
		id, agent := r.PathValue("id"), ""
		if at := slices.IndexFunc(h.Sessions(), func(s hub.Session) bool { return s.ID == id }); at >= 0 {
			agent = h.Sessions()[at].Agent
		} else if a, ok := h.Archive(id); ok {
			agent = a.Agent
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
	mux.HandleFunc("POST /api/wrapup", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Day string `json:"day"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		if !h.AskWrapup(in.Day) {
			http.Error(w, "day must be like 2006-01-02", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusAccepted)
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
			MeasuredAt int64 `json:"measuredAt"`
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
					s.LastPrompt, s.PromptAt = in.Text, now
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
		p := h.Ask(agent, in.SessionID, hub.Pending{Tool: in.Tool, Summary: in.Summary, Input: in.Input, Question: in.Question, AgentID: in.AgentID}, in.CanAnswer)
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
		var ok bool
		if in.Model.URL, ok = modelURL(in.Model.URL); !ok {
			http.Error(w, "the model server's address must be an http or https one", http.StatusBadRequest)
			return
		}
		in.Model.Name = strings.TrimSpace(in.Model.Name)
		h.SetSettings(in)
		w.WriteHeader(http.StatusNoContent)
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
