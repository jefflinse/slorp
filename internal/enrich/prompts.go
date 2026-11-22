package enrich

import (
	"cmp"
	"maps"
	"strings"

	"github.com/jefflinse/slorp/internal/hub"
)

var defaults = map[string]string{
	hub.ActDescribe:  system,
	hub.PromptLookup: planSystem,
	hub.ActFile:      routeSystem,
	hub.ActRole:      roleSystem,
	hub.ActNote:      noteSystem,
	hub.ActBrief:     briefSystem,
	hub.ActWrapup:    wrapupSystem,
	hub.ActBackfill:  backfillSystem,
}

// Defaults returns Slorp's own instructions to the local model for each
// task it does, by the task.
func Defaults() map[string]string {
	return maps.Clone(defaults)
}

// instructions returns what the model is told for task: what the user
// wrote to go ahead of every task, then what they wrote for this one or,
// where they wrote nothing, Slorp's own. It follows the user's settings as
// they are when asked.
func (e *Enricher) instructions(task string) string {
	if e.Hub == nil {
		return defaults[task]
	}
	p := e.Hub.Settings().Prompts
	told := cmp.Or(strings.TrimSpace(p.Tasks[task]), defaults[task])
	if global := strings.TrimSpace(p.Global); global != "" {
		return global + "\n\n" + told
	}
	return told
}
