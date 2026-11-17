// Package feed collects what the integrations' feeds find and hands it to
// the hub, on a schedule and whenever it is asked to.
package feed

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"time"

	"github.com/jefflinse/slorp/internal/hub"
	"github.com/jefflinse/slorp/internal/integrations"
)

// Source is what a Poller collects from.
type Source interface {
	Feeds(ctx context.Context) []integrations.Feed
	Clones(dirs []string) map[string][]string
}

type Poller struct {
	Hub    *hub.Hub
	Source Source
	// Dirs lists the directories the efforts file by.
	Dirs func() []string
	// Every is how often each feed is collected.
	Every time.Duration
	// Tick is how often the poller looks for something to do.
	Tick time.Duration

	kick, nudge chan struct{}
	polled      map[string]time.Time
}

func New(h *hub.Hub, src Source, dirs func() []string) *Poller {
	return &Poller{Hub: h, Source: src, Dirs: dirs, Every: 5 * time.Minute, Tick: 15 * time.Second,
		kick: make(chan struct{}, 1), nudge: make(chan struct{}, 1), polled: map[string]time.Time{}}
}

// Check asks for every feed to be collected now rather than when it is due.
func (p *Poller) Check() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

// Rescan asks for the efforts' directories to be searched again now, as
// after one of them changed.
func (p *Poller) Rescan() {
	select {
	case p.nudge <- struct{}{}:
	default:
	}
}

// Run collects until ctx ends.
func (p *Poller) Run(ctx context.Context) {
	tick := time.NewTicker(p.Tick)
	defer tick.Stop()
	now := false
	for {
		p.Step(ctx, now)
		now = false
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-p.nudge:
		case <-p.kick:
			now = true
		}
	}
}

// Step searches the efforts' directories, collects the feeds that are due,
// or every feed when now is true, and drops those that have been turned off.
func (p *Poller) Step(ctx context.Context, now bool) {
	p.Hub.SetClones(p.Source.Clones(p.Dirs()))
	for _, f := range p.Source.Feeds(ctx) {
		if !f.On {
			p.Hub.DropFeed(f.ID)
			delete(p.polled, f.ID)
			continue
		}
		if !now && time.Since(p.polled[f.ID]) < p.Every {
			continue
		}
		found, err := f.Fetch(ctx)
		p.polled[f.ID] = time.Now()
		items := make([]hub.Item, len(found))
		for i, it := range found {
			sum := sha1.Sum([]byte(it.Key))
			items[i] = hub.Item{ID: f.ID + "-" + hex.EncodeToString(sum[:6]), Kind: it.Kind, Urgency: it.Urgency, Title: it.Title,
				Ask: it.Ask, Summary: it.Summary, URL: it.URL, Repo: it.Repo, At: it.At}
		}
		p.Hub.SetFeed(f.ID, items, err)
	}
}
