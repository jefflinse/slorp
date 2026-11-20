package hub

import (
	"maps"
	"slices"
	"strings"
	"time"
)

// ProviderLimits is one root session's provider reading, never shared with other roots.
type ProviderLimits struct {
	SessionID    string                `json:"sessionId"`
	SessionLabel string                `json:"sessionLabel"`
	Provider     string                `json:"provider"`
	Status       string                `json:"status"`
	CheckedAt    time.Time             `json:"checkedAt"`
	MeasuredAt   time.Time             `json:"measuredAt,omitzero"`
	RetryAt      time.Time             `json:"retryAt,omitzero"`
	Windows      []ProviderLimitWindow `json:"windows"`
	Stale        bool                  `json:"stale"`
}

// ProviderLimitWindow is a measured quota window with a known reset time.
type ProviderLimitWindow struct {
	Kind          string    `json:"kind"`
	Label         string    `json:"label"`
	PercentUsed   float64   `json:"percentUsed"`
	ResetsAt      time.Time `json:"resetsAt"`
	WindowSeconds int64     `json:"windowSeconds,omitempty"`
}

const retiredProviderLimitEpochs = 16

type providerLimitSnapshot struct {
	epoch    string
	revision int64
	retired  []string
	reports  []ProviderLimits
}

// SetProviderLimits applies a validated root snapshot inside an Apply callback.
// Nil leaves it unchanged; an empty snapshot clears it. Only newer revisions of
// the active epoch or a new epoch replace the snapshot; the last 16 superseded
// epochs are rejected. Report timestamps govern freshness, not ordering.
func (s *Session) SetProviderLimits(epoch string, revision int64, reports []ProviderLimits) {
	if reports == nil {
		return
	}
	held := s.providerLimits
	if epoch == held.epoch && revision <= held.revision || slices.Contains(held.retired, epoch) {
		return
	}
	next := providerLimitSnapshot{epoch: epoch, revision: revision, retired: slices.Clone(held.retired), reports: make([]ProviderLimits, len(reports))}
	if epoch != held.epoch && held.epoch != "" {
		next.retired = append(next.retired, held.epoch)
		if len(next.retired) > retiredProviderLimitEpochs {
			next.retired = slices.Clone(next.retired[len(next.retired)-retiredProviderLimitEpochs:])
		}
	}
	for i, report := range reports {
		report.SessionID, report.SessionLabel, report.Stale = "", "", false
		report.Windows = slices.Clone(report.Windows)
		next.reports[i] = report
	}
	slices.SortFunc(next.reports, func(a, b ProviderLimits) int { return strings.Compare(a.Provider, b.Provider) })
	s.providerLimits = next
}

func (h *Hub) providerLimited(now time.Time) []ProviderLimits {
	out := []ProviderLimits{}
	for _, id := range slices.Sorted(maps.Keys(h.sessions)) {
		s := h.sessions[id]
		for _, held := range s.providerLimits.reports {
			report := held
			report.SessionID = id
			report.SessionLabel, _ = s.label()
			report.Stale = report.Status != "ok" || report.MeasuredAt.IsZero() || now.Sub(report.MeasuredAt) > 5*time.Minute
			report.Windows = []ProviderLimitWindow{}
			for _, window := range held.Windows {
				if report.MeasuredAt.IsZero() || now.Sub(report.MeasuredAt) > 10*time.Minute || !window.ResetsAt.After(now) {
					report.Stale = true
					continue
				}
				report.Windows = append(report.Windows, window)
			}
			out = append(out, report)
		}
	}
	return out
}
