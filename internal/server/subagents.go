package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"

	"github.com/jefflinse/slorp/internal/hub"
)

func subagentHandler(h *hub.Hub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Agent     string              `json:"agent"`
			SessionID string              `json:"sessionId"`
			Revision  int64               `json:"revision"`
			Subagent  hub.Subagent        `json:"subagent"`
			Report    *hub.SubagentReport `json:"report"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		if err := dec.Decode(&in); err != nil {
			http.Error(w, "bad request body", http.StatusBadRequest)
			return
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			http.Error(w, "bad request body", http.StatusBadRequest)
			return
		}
		if !agents[in.Agent] {
			http.Error(w, "agent must name a supported agent", http.StatusBadRequest)
			return
		}
		err := h.UpsertSubagent(in.Agent, in.SessionID, in.Revision, in.Subagent, in.Report)
		switch {
		case err == nil:
			w.WriteHeader(http.StatusNoContent)
		case errors.Is(err, hub.ErrSubagentInvalid):
			http.Error(w, err.Error(), http.StatusBadRequest)
		case errors.Is(err, hub.ErrSubagentSession):
			http.Error(w, err.Error(), http.StatusNotFound)
		case errors.Is(err, hub.ErrSubagentConflict), errors.Is(err, hub.ErrSubagentLimit):
			http.Error(w, err.Error(), http.StatusConflict)
		default:
			slog.ErrorContext(r.Context(), "retaining subagent", "agent", in.Agent, "session_id", in.SessionID, "subagent_id", in.Subagent.ID, "error", err)
			http.Error(w, "could not store subagent", http.StatusInternalServerError)
		}
	}
}

func archiveSubagents(source, retained []hub.Subagent) []hub.Subagent {
	out := slices.Clone(source)
	for _, a := range retained {
		if i := slices.IndexFunc(out, func(s hub.Subagent) bool { return s.ID == a.ID }); i >= 0 {
			out[i].ReportRevision = a.ReportRevision
			if a.Done {
				out[i].Done, out[i].Status = true, a.Status
				out[i].Doing, out[i].Samples = a.Doing, a.Samples
			}
		} else {
			out = append(out, a)
		}
	}
	for i := range out {
		out[i].Samples = slices.Clone(out[i].Samples)
	}
	return out
}
