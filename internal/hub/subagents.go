package hub

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const retainedSubagentLimit = 200

var (
	ErrSubagentInvalid  = errors.New("invalid subagent record")
	ErrSubagentSession  = errors.New("no such open session")
	ErrSubagentConflict = errors.New("session closed or agent does not match")
	ErrSubagentLimit    = errors.New("retained subagent limit reached")
)

type subagentRecord struct {
	Agent      string          `json:"agent"`
	SessionID  string          `json:"sessionId"`
	Revision   int64           `json:"revision"`
	Subagent   Subagent        `json:"subagent"`
	Report     *SubagentReport `json:"report,omitempty"`
	ReceivedAt time.Time       `json:"receivedAt"`
	// Handed is set on a record a session was handed as it took up the
	// one the subagent was started under; it does not count toward the
	// session's limit.
	Handed bool `json:"handed,omitempty"`
}

func subagentKey(root, child string) string {
	b, _ := json.Marshal([2]string{root, child}) // Strings always marshal successfully.
	return string(b)
}

func (h *Hub) loadSubagents() {
	h.retained = map[string]map[string]subagentRecord{}
	err := h.cache.Each("subagent", func(key string, body []byte) {
		var r subagentRecord
		if err := json.Unmarshal(body, &r); err != nil {
			h.retainedErr = fmt.Errorf("decoding retained subagent: %w", err)
			return
		}
		if err := validateSubagent(r.Agent, r.SessionID, r.Revision, r.Subagent, r.Report); err != nil || key != subagentKey(r.SessionID, r.Subagent.ID) {
			h.retainedErr = errors.New("invalid stored subagent record")
			return
		}
		if h.retained[r.SessionID] == nil {
			h.retained[r.SessionID] = map[string]subagentRecord{}
		}
		if !r.Handed && own(h.retained[r.SessionID]) >= retainedSubagentLimit {
			h.retainedErr = errors.New("stored subagent count exceeds limit")
			return
		}
		r.Subagent.ReportRevision = r.Revision
		r.Subagent.Done = r.Subagent.Done || ended(r.Subagent.Status)
		// A restart does not establish that a child is still running.
		r.ReceivedAt = time.Time{}
		h.retained[r.SessionID][r.Subagent.ID] = r
	})
	if err != nil {
		h.retainedErr = fmt.Errorf("loading retained subagents: %w", err)
	}
	h.warn(h.retainedErr)
}

// UpsertSubagent durably replaces one child of an open, matching session. Equal
// or older revisions are no-ops; a nil report preserves the previous report.
func (h *Hub) UpsertSubagent(agent, sessionID string, revision int64, a Subagent, report *SubagentReport) error {
	if err := validateSubagent(agent, sessionID, revision, a, report); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.sessions[sessionID]
	if s == nil {
		if _, ok := h.archive[sessionID]; ok {
			return ErrSubagentConflict
		}
		return ErrSubagentSession
	}
	if s.Agent != agent {
		return ErrSubagentConflict
	}
	if h.retainedErr != nil {
		return fmt.Errorf("retained subagents unavailable: %w", h.retainedErr)
	}
	old, exists := h.retained[sessionID][a.ID]
	if exists && old.Agent != agent {
		return ErrSubagentConflict
	}
	if exists && revision <= old.Revision {
		return nil
	}
	if !exists && own(h.retained[sessionID]) >= retainedSubagentLimit {
		return ErrSubagentLimit
	}
	a.ReportRevision = revision
	a.Done = a.Done || ended(a.Status)
	a.Samples = slices.Clone(a.Samples)
	if report == nil {
		report = old.Report
	} else {
		copy := *report
		report = &copy
	}
	r := subagentRecord{Agent: agent, SessionID: sessionID, Revision: revision, Subagent: a, Report: report, ReceivedAt: time.Now(), Handed: old.Handed}
	// Persist the root first so a child-only session is discoverable after a crash.
	// A failed child write can leave a harmless root document, never a partial child.
	if err := h.cache.Put("session", sessionID, h.doc(s)); err != nil {
		return fmt.Errorf("storing subagent root: %w", err)
	}
	if err := h.cache.Put("subagent", subagentKey(sessionID, a.ID), r); err != nil {
		return fmt.Errorf("storing subagent record: %w", err)
	}
	if h.retained[sessionID] == nil {
		h.retained[sessionID] = map[string]subagentRecord{}
	}
	h.retained[sessionID][a.ID] = r
	h.changed()
	return nil
}

// RetainedSubagentReport returns a copy of the retained report, if supplied.
func (h *Hub) RetainedSubagentReport(agent, sessionID, childID string) (SubagentReport, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r, ok := h.childRecord(sessionID, childID); ok && r.Agent == agent && r.Report != nil {
		return *r.Report, true
	}
	return SubagentReport{}, false
}

// childRecord is the retained record a session's subagent is listed and
// reported by, its own or one it was handed.
func (h *Hub) childRecord(root, child string) (subagentRecord, bool) {
	r, ok := h.retained[root][child]
	return r, ok
}

// own counts the retained records started under a session itself.
func own(records map[string]subagentRecord) int {
	n := 0
	for _, r := range records {
		if !r.Handed {
			n++
		}
	}
	return n
}

// childKey is which subagents a list holds, and which of them are done.
func childKey(children []Subagent) string {
	var b strings.Builder
	for _, a := range children {
		b.WriteString(a.ID)
		if a.Done {
			b.WriteString("+")
		}
		b.WriteString(",")
	}
	return b.String()
}

// childIndex is what is kept of subagents to list them by once they are
// no longer there to ask.
func childIndex(children []Subagent) []Subagent {
	out := make([]Subagent, 0, len(children))
	for _, a := range children {
		a.ReportRevision, a.Doing, a.Samples, a.ActiveAt, a.Tools, a.Used, a.CostUSD = 0, "", nil, time.Time{}, 0, Tokens{}, 0
		if !ended(a.Status) {
			a.Status = ""
		}
		out = append(out, a)
	}
	return out
}

func (h *Hub) mergeSubagents(root, agent string, source []Subagent, connected bool) []Subagent {
	out := cloneSubagents(source)
	var handed []Subagent
	for _, t := range h.tookOf(root) {
		handed = append(handed, t.Children...)
	}
	var children []string
	for child := range h.retained[root] {
		children = append(children, child)
	}
	for _, a := range handed {
		children = append(children, a.ID)
	}
	var retained []subagentRecord
	seen := map[string]bool{}
	for _, child := range children {
		if seen[child] {
			continue
		}
		seen[child] = true
		if r, ok := h.childRecord(root, child); ok {
			retained = append(retained, r)
		}
	}
	for _, a := range handed {
		if !slices.ContainsFunc(out, func(s Subagent) bool { return s.ID == a.ID }) && !slices.ContainsFunc(retained, func(r subagentRecord) bool { return r.Subagent.ID == a.ID }) {
			out = append(out, a)
		}
	}
	for _, r := range retained {
		if r.Agent != agent {
			continue
		}
		a := r.Subagent
		a.Samples = slices.Clone(a.Samples)
		if !connected || time.Since(r.ReceivedAt) >= bridgeTTL {
			a.Doing, a.Samples = "", nil
			if !ended(a.Status) {
				a.Status = ""
			}
		}
		if i := slices.IndexFunc(out, func(s Subagent) bool { return s.ID == a.ID }); i >= 0 {
			out[i].ReportRevision = r.Revision
			if a.Done || r.Subagent.Status != "" && !out[i].Done && !ended(out[i].Status) {
				out[i].Done, out[i].Status = a.Done, a.Status
				out[i].Doing, out[i].Samples = a.Doing, a.Samples
			}
			continue
		}
		out = append(out, a)
	}
	if len(retained) > 0 || len(handed) > 0 {
		slices.SortStableFunc(out, func(a, b Subagent) int {
			return cmp.Or(a.StartedAt.Compare(b.StartedAt), cmp.Compare(a.ID, b.ID))
		})
	}
	return out
}

func (h *Hub) sessionSubagents(s *Session, connected bool) []Subagent {
	out := h.mergeSubagents(s.ID, s.Agent, s.Subagents, connected)
	if !connected || time.Since(s.liveSeen) >= bridgeTTL {
		return out
	}
	live := make([]Subagent, 0, len(s.live))
	for _, l := range s.live {
		r, retained := h.retained[s.ID][l.ID]
		if !retained || r.Agent != s.Agent {
			live = append(live, l)
			continue
		}
		// Polls may refresh activity, but cannot replace durable facts or
		// resurrect a finished child without a newer retained revision.
		if !r.Subagent.Done && s.liveSeen.After(r.ReceivedAt) {
			i := slices.IndexFunc(out, func(a Subagent) bool { return a.ID == l.ID })
			if out[i].Done || ended(out[i].Status) {
				continue
			}
			out[i].Status, out[i].Doing, out[i].Samples = l.Status, l.Doing, slices.Clone(l.Samples)
			out[i].Done = ended(l.Status)
		}
	}
	return withLive(out, live)
}

func cloneSubagents(agents []Subagent) []Subagent {
	out := slices.Clone(agents)
	for i := range out {
		out[i].Samples = slices.Clone(out[i].Samples)
	}
	return out
}

func validateSubagent(agent, root string, revision int64, a Subagent, report *SubagentReport) error {
	invalid := func(field string) error { return fmt.Errorf("%w: %s", ErrSubagentInvalid, field) }
	if !subagentID(agent) || !subagentID(root) || !subagentID(a.ID) || a.ID == root || a.ParentID != "" && (!subagentID(a.ParentID) || a.ParentID == a.ID) {
		return invalid("agent, sessionId, id or parentId")
	}
	if revision <= 0 {
		return invalid("revision must be positive")
	}
	for _, field := range []struct {
		value string
		limit int
	}{
		{a.Type, 80}, {a.Description, 1000}, {a.Model, 200}, {a.Effort, 80}, {a.Doing, 300},
	} {
		if utf8.RuneCountInString(field.value) > field.limit {
			return invalid("metadata too long")
		}
	}
	switch a.Status {
	case "", "running", "waiting", "idle", "completed", "failed", "killed", "pending":
	default:
		return invalid("status")
	}
	if a.Done && a.Status != "" && !ended(a.Status) {
		return invalid("done conflicts with status")
	}
	if a.Depth < 0 || a.Depth > 64 || a.Tools < 0 || a.Tools > 1_000_000_000 || len(a.Samples) > 16 {
		return invalid("depth, tools or samples")
	}
	for _, n := range append(slices.Clone(a.Samples), a.Used.In, a.Used.Out, a.Used.CacheRead, a.Used.CacheWrite) {
		if n < 0 || int64(n) > 1_000_000_000_000 {
			return invalid("usage")
		}
	}
	if math.IsNaN(a.CostUSD) || math.IsInf(a.CostUSD, 0) || a.CostUSD < 0 || a.CostUSD > 1_000_000_000 {
		return invalid("costUsd")
	}
	if report != nil && (utf8.RuneCountInString(report.Prompt) > 60000 || utf8.RuneCountInString(report.Report) > 60000) {
		return invalid("prompt or report exceeds 60000 runes")
	}
	return nil
}

func subagentID(id string) bool {
	return id != "" && len(id) <= 256 && id != "." && id != ".." && !strings.ContainsAny(id, `/\`) &&
		!strings.ContainsFunc(id, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}
