package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jefflinse/slorp/internal/hub"
)

type providerLimitReport struct {
	Provider   string          `json:"provider"`
	Status     string          `json:"status"`
	CheckedAt  *int64          `json:"checkedAt"`
	MeasuredAt json.RawMessage `json:"measuredAt"`
	RetryAt    json.RawMessage `json:"retryAt"`
	Windows    []struct {
		Kind          string          `json:"kind"`
		Label         string          `json:"label"`
		PercentUsed   *float64        `json:"percentUsed"`
		ResetsAt      string          `json:"resetsAt"`
		WindowSeconds json.RawMessage `json:"windowSeconds"`
	} `json:"windows"`
}

type providerLimitsSnapshot struct {
	epoch    string
	revision int64
	reports  []hub.ProviderLimits
}

func parseProviderLimitsSnapshot(raw, epochRaw, revisionRaw json.RawMessage, now time.Time) (providerLimitsSnapshot, error) {
	var snapshot providerLimitsSnapshot
	if raw == nil && epochRaw == nil && revisionRaw == nil {
		return snapshot, nil
	}
	if raw == nil || json.Unmarshal(epochRaw, &snapshot.epoch) != nil || !validProviderLimitsEpoch(snapshot.epoch) || json.Unmarshal(revisionRaw, &snapshot.revision) != nil || snapshot.revision <= 0 || snapshot.revision > 1<<53-1 {
		return providerLimitsSnapshot{}, errors.New("invalid providerLimits version")
	}
	snapshot.epoch = strings.ToLower(snapshot.epoch)
	reports, err := parseProviderLimits(raw, now)
	if err != nil {
		return providerLimitsSnapshot{}, err
	}
	snapshot.reports = reports
	return snapshot, nil
}

func validProviderLimitsEpoch(epoch string) bool {
	if len(epoch) != 36 {
		return false
	}
	for i, c := range epoch {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func parseProviderLimits(raw json.RawMessage, now time.Time) ([]hub.ProviderLimits, error) {
	if raw == nil {
		return nil, nil
	}
	invalid := errors.New("invalid providerLimits")
	var reports []providerLimitReport
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&reports); err != nil || reports == nil || len(reports) > 3 {
		return nil, invalid
	}
	out := make([]hub.ProviderLimits, 0, len(reports))
	seen := map[string]bool{}
	for _, report := range reports {
		switch report.Provider {
		case "openai-codex", "github-copilot", "opencode-go":
		default:
			return nil, invalid
		}
		if seen[report.Provider] || report.CheckedAt == nil || !validProviderTimestamp(*report.CheckedAt, now) || report.Windows == nil || len(report.Windows) > 8 {
			return nil, invalid
		}
		seen[report.Provider] = true
		p := hub.ProviderLimits{Provider: report.Provider, Status: report.Status, CheckedAt: time.UnixMilli(*report.CheckedAt).UTC(), Windows: []hub.ProviderLimitWindow{}}
		for _, field := range []struct {
			raw    json.RawMessage
			at     *time.Time
			future time.Duration
		}{{report.MeasuredAt, &p.MeasuredAt, time.Minute}, {report.RetryAt, &p.RetryAt, 24 * time.Hour}} {
			if field.raw != nil {
				var millis *int64
				if json.Unmarshal(field.raw, &millis) != nil || millis == nil || *millis <= 0 || *millis > now.Add(field.future).UnixMilli() {
					return nil, invalid
				}
				*field.at = time.UnixMilli(*millis).UTC()
			}
		}
		if p.MeasuredAt.After(p.CheckedAt) {
			return nil, invalid
		}
		switch p.Status {
		case "ok":
			if p.MeasuredAt.IsZero() {
				return nil, invalid
			}
		case "unsupported", "auth_required":
			if !p.MeasuredAt.IsZero() || len(report.Windows) != 0 {
				return nil, invalid
			}
		case "unavailable", "throttled":
		default:
			return nil, invalid
		}
		if len(report.Windows) > 0 && p.MeasuredAt.IsZero() {
			return nil, invalid
		}
		for _, window := range report.Windows {
			if !validProviderString(window.Kind) || !validProviderString(window.Label) || len(window.ResetsAt) > 100 || window.PercentUsed == nil || math.IsNaN(*window.PercentUsed) || math.IsInf(*window.PercentUsed, 0) || *window.PercentUsed < 0 || *window.PercentUsed > 100 {
				return nil, invalid
			}
			resets, err := time.Parse(time.RFC3339Nano, window.ResetsAt)
			if err != nil || resets.IsZero() || resets.UTC().Year() < 1 || resets.UTC().Year() > 9999 {
				return nil, invalid
			}
			if !strings.HasSuffix(window.ResetsAt, "Z") {
				zone := window.ResetsAt[len(window.ResetsAt)-6:]
				if zone[1:3] >= "24" || zone[4:6] >= "60" {
					return nil, invalid
				}
			}
			w := hub.ProviderLimitWindow{Kind: window.Kind, Label: window.Label, PercentUsed: *window.PercentUsed, ResetsAt: resets.UTC()}
			if window.WindowSeconds != nil {
				var seconds *int64
				if json.Unmarshal(window.WindowSeconds, &seconds) != nil || seconds == nil || *seconds <= 0 {
					return nil, invalid
				}
				w.WindowSeconds = *seconds
			}
			p.Windows = append(p.Windows, w)
		}
		out = append(out, p)
	}
	return out, nil
}

func validProviderTimestamp(millis int64, now time.Time) bool {
	return millis > 0 && millis <= now.Add(time.Minute).UnixMilli()
}

func validProviderString(s string) bool {
	return strings.TrimSpace(s) != "" && utf8.RuneCountInString(s) <= 100
}
