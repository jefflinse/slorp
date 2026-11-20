// Package opencode keeps the roster of OpenCode sessions. OpenCode has no
// command that lists its open sessions, so a session is known only through
// the Slorp plugin running inside it, and is open for as long as that plugin
// keeps reporting.
package opencode

import (
	"context"
	"sync"
	"time"

	"github.com/jefflinse/slorp/internal/hub"
)

const Agent = "opencode"

type Adapter struct {
	Hub      *hub.Hub
	Interval time.Duration
	// Silence is how long a session's plugin may go unheard before the session counts as closed.
	Silence                               time.Duration
	Bin, Home, Data, URL                  string
	ConfigDir, ExtraConfigDir, ConfigFile string
	pluginMu                              sync.Mutex
	pluginAt                              time.Time
	lifecycleMu                           sync.Mutex
	runtimeMu                             sync.Mutex
	lifeOnce                              sync.Once
	life                                  context.Context
	cancelLife                            context.CancelFunc
	runtimes                              map[string]*runtime
}

// Run retires silent sessions until ctx ends.
func (a *Adapter) Run(ctx context.Context) {
	t := time.NewTicker(a.Interval)
	defer t.Stop()
	for {
		if a.Bin != "" && time.Since(a.pluginAt) >= 30*time.Second {
			a.CheckPlugin()
			a.pluginAt = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.Hub.Retire(Agent, a.Silence)
		}
	}
}
