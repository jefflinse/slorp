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
	Discuss(ctx context.Context, id, repo string, number int) (integrations.Discussion, error)
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
	// Discuss is how often the discussions of pull requests that have
	// changed are read, and Discussions how many are read each time. The
	// one the user is looking at is not kept waiting.
	Discuss     time.Duration
	Discussions int

	kick, nudge chan struct{}
	polled      map[string]time.Time
	discussed   time.Time
}

func New(h *hub.Hub, src Source, dirs func() []string) *Poller {
	return &Poller{Hub: h, Source: src, Dirs: dirs, Every: 5 * time.Minute, Tick: 15 * time.Second, Discuss: time.Minute, Discussions: 3,
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

// Step searches the efforts' directories and the sessions', collects the
// feeds that are due, or every feed when now is true, drops those that have
// been turned off, and, when that is due, reads a few of the discussions of
// the pull requests listed that have changed since theirs was last read.
// The discussion of the one the user is looking at is read whenever it has
// changed.
func (p *Poller) Step(ctx context.Context, now bool) {
	p.Hub.SetClones(p.Source.Clones(p.Dirs()))
	p.Hub.SetCheckouts(p.Source.Clones(p.Hub.Cwds()))
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
		items := make([]hub.Item, len(found.Items))
		for i, it := range found.Items {
			sum := sha1.Sum([]byte(it.Key))
			items[i] = hub.Item{ID: f.ID + "-" + hex.EncodeToString(sum[:6]), Kind: it.Kind, Urgency: it.Urgency, Title: it.Title,
				Ask: it.Ask, Summary: it.Summary, URL: it.URL, Repo: it.Repo, Pull: it.Pull, At: it.At}
		}
		p.Hub.SetFeed(f.ID, items, found.PullRequests, err)
	}
	due := time.Since(p.discussed) >= p.Discuss
	for _, ref := range p.Hub.Undiscussed(p.Discussions) {
		if due || ref.Wanted {
			d, err := p.Source.Discuss(ctx, ref.Source, ref.Repo, ref.Number)
			p.Hub.SetDiscussion(ref, d, err)
		}
	}
	if due {
		p.discussed = time.Now()
	}
}
