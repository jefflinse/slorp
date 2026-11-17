package feed_test

import (
	"context"
	"errors"
	"reflect"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/jefflinse/slorp/internal/feed"
	"github.com/jefflinse/slorp/internal/hub"
	"github.com/jefflinse/slorp/internal/integrations"
	"github.com/jefflinse/slorp/internal/kb"
	"github.com/jefflinse/slorp/internal/testdb"
)

var ctx = context.Background()

// source is a stand-in for the integrations: one feed per entry of on,
// each returning found, or failing with err.
type source struct {
	mu      sync.Mutex
	order   []string
	on      map[string]bool
	found   map[string][]integrations.FeedItem
	err     error
	fetched map[string]int
	clones  map[string][]string
	scanned [][]string
}

func newSource(ids ...string) *source {
	s := &source{order: ids, on: map[string]bool{}, found: map[string][]integrations.FeedItem{}, fetched: map[string]int{}}
	for _, id := range ids {
		s.on[id] = true
	}
	return s
}

func (s *source) Feeds(context.Context) []integrations.Feed {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []integrations.Feed
	for _, id := range s.order {
		out = append(out, integrations.Feed{ID: id, On: s.on[id], Fetch: func(context.Context) ([]integrations.FeedItem, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.fetched[id]++
			return s.found[id], s.err
		}})
	}
	return out
}

func (s *source) Clones(dirs []string) map[string][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scanned = append(s.scanned, dirs)
	return s.clones
}

func (s *source) count(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fetched[id]
}

func (s *source) scans() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.scanned)
}

func (s *source) set(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn()
}

func newPoller(t *testing.T, src *source, dirs ...string) (*feed.Poller, *hub.Hub, *kb.Store) {
	t.Helper()
	st := testdb.Open(t)
	k := testdb.KB(t, st)
	h := hub.New(k, st)
	return feed.New(h, src, func() []string { return dirs }), h, k
}

func feedItems(h *hub.Hub) []hub.Item {
	var out []hub.Item
	for _, it := range h.State().Items {
		if it.Source != "" {
			out = append(out, it)
		}
	}
	return out
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for range 200 {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("never happened: %s", what)
}

func TestStepHandsWhatAFeedFoundToTheHub(t *testing.T) {
	src := newSource("github")
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	src.found["github"] = []integrations.FeedItem{{Key: "acme/widgets#7:ready", Kind: "ready-to-merge", Urgency: "whenever", Title: "Ready",
		Ask: "Merge it.", Summary: "facts", URL: "https://example.com/7", Repo: "acme/widgets", At: at}}
	p, h, _ := newPoller(t, src)
	p.Step(ctx, false)

	got := feedItems(h)
	if len(got) != 1 {
		t.Fatalf("items = %+v", got)
	}
	want := hub.Item{ID: got[0].ID, Kind: "ready-to-merge", Urgency: "whenever", Title: "Ready", Ask: "Merge it.", Summary: "facts",
		URL: "https://example.com/7", Repo: "acme/widgets", At: at, Source: "github", Replies: []string{}}
	if !reflect.DeepEqual(got[0], want) {
		t.Errorf("item =\n%+v, want\n%+v", got[0], want)
	}
	if !regexp.MustCompile(`^github-[0-9a-f]{12}$`).MatchString(got[0].ID) {
		t.Errorf("id = %q, want one safe to put in a page", got[0].ID)
	}
}

func TestItemIDsFollowTheKey(t *testing.T) {
	src := newSource("github", "jira")
	src.found["github"] = []integrations.FeedItem{{Key: "a"}, {Key: "b"}}
	src.found["jira"] = []integrations.FeedItem{{Key: "a"}}
	p, h, _ := newPoller(t, src)
	p.Step(ctx, false)
	first := feedItems(h)
	if len(first) != 3 || first[0].ID == first[1].ID || first[0].ID == first[2].ID {
		t.Fatalf("ids = %q, %q, %q; want one each, even for the same key in two feeds", first[0].ID, first[1].ID, first[2].ID)
	}

	src.set(func() { src.found["github"] = []integrations.FeedItem{{Key: "b", Title: "changed"}, {Key: "a"}} })
	p.Step(ctx, true)
	second := feedItems(h)
	if second[0].ID != first[1].ID || second[1].ID != first[0].ID {
		t.Error("an item's id changed between collections, so a dismissed item would come back")
	}
}

func TestStepCollectsOnlyWhatIsDue(t *testing.T) {
	src := newSource("github")
	p, _, _ := newPoller(t, src)

	p.Step(ctx, false)
	p.Step(ctx, false)
	if n := src.count("github"); n != 1 {
		t.Fatalf("collected %d times in two steps, want once: each collection is a request to GitHub", n)
	}
	p.Step(ctx, true)
	if n := src.count("github"); n != 2 {
		t.Errorf("collected %d times, want a second when asked to check now", n)
	}
	p.Step(ctx, false)
	if n := src.count("github"); n != 2 {
		t.Errorf("collected %d times, want checking now to restart the wait", n)
	}
	p.Every = time.Nanosecond
	time.Sleep(time.Millisecond)
	p.Step(ctx, false)
	if n := src.count("github"); n != 3 {
		t.Errorf("collected %d times, want a third once the feed came due", n)
	}
}

func TestStepDropsAFeedThatIsOff(t *testing.T) {
	src := newSource("github", "jira")
	src.found["github"] = []integrations.FeedItem{{Key: "a"}}
	src.found["jira"] = []integrations.FeedItem{{Key: "b"}}
	p, h, _ := newPoller(t, src)
	p.Step(ctx, false)

	src.set(func() { src.on["github"] = false })
	p.Step(ctx, true)
	if got := feedItems(h); len(got) != 1 || got[0].Source != "jira" {
		t.Fatalf("items = %+v, want only the feed still on", got)
	}
	if _, kept := h.State().Feeds["github"]; kept || src.count("github") != 1 {
		t.Errorf("a feed that is off was still reported or collected (%d collections)", src.count("github"))
	}

	src.set(func() { src.on["github"] = true })
	p.Step(ctx, false)
	if got := feedItems(h); len(got) != 2 || src.count("github") != 2 {
		t.Errorf("items = %+v after turning it back on, want it collected at once rather than when next due", got)
	}
	if src.count("jira") != 2 {
		t.Errorf("jira collected %d times, want 2: once at first and once when asked", src.count("jira"))
	}
}

func TestStepReportsAFailedCollection(t *testing.T) {
	src := newSource("github")
	src.found["github"] = []integrations.FeedItem{{Key: "a"}}
	p, h, _ := newPoller(t, src)
	p.Step(ctx, false)

	src.set(func() { src.found["github"], src.err = nil, errors.New("gh: timed out") })
	p.Step(ctx, true)
	if f := h.State().Feeds["github"]; f.Error != "gh: timed out" || f.Count != 1 || len(feedItems(h)) != 1 {
		t.Errorf("feed = %+v with %d items, want the error shown and the earlier item kept", f, len(feedItems(h)))
	}
	p.Step(ctx, false)
	if n := src.count("github"); n != 2 {
		t.Errorf("collected %d times, want a failed feed to wait its turn like any other", n)
	}
}

func TestStepSearchesTheEffortsDirectories(t *testing.T) {
	src := newSource("github")
	src.found["github"] = []integrations.FeedItem{{Key: "a", Repo: "acme/widgets"}}
	src.clones = map[string][]string{"/work/a": {"acme/widgets"}}
	p, h, k := newPoller(t, src, "/work/a", "/work/b")
	e, err := k.Create("A", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := k.AddDir(e.ID, "/work/a"); err != nil {
		t.Fatal(err)
	}

	p.Step(ctx, false)
	if len(src.scanned) != 1 || !reflect.DeepEqual(src.scanned[0], []string{"/work/a", "/work/b"}) {
		t.Errorf("searched %v, want the efforts' directories", src.scanned)
	}
	if got := feedItems(h); len(got) != 1 || got[0].Effort != e.ID || got[0].EffortVia != kb.ViaDir {
		t.Errorf("items = %+v, want the item filed by the checkout that was found", got)
	}

	src.set(func() { src.clones = nil })
	p.Step(ctx, false)
	if got := feedItems(h); got[0].Effort != "" || src.count("github") != 1 {
		t.Errorf("after the checkout went away: effort %q after %d collections; want it unsorted without asking GitHub again", got[0].Effort, src.count("github"))
	}
}

func TestRunCollectsWhenAsked(t *testing.T) {
	src := newSource("github")
	p, _, _ := newPoller(t, src)
	p.Tick = time.Hour
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()

	eventually(t, "a first collection without waiting for a tick", func() bool { return src.count("github") == 1 })

	p.Rescan()
	eventually(t, "a search of the directories when asked", func() bool { return src.scans() == 2 })
	if n := src.count("github"); n != 1 {
		t.Errorf("collected %d times, want a rescan to leave GitHub alone", n)
	}

	p.Check()
	eventually(t, "a collection when asked", func() bool { return src.count("github") == 2 })
	p.Check()
	p.Check()
	p.Check()
	eventually(t, "another collection, without blocking whoever asked", func() bool { return src.count("github") >= 3 })

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop when its context ended")
	}
}

func TestRunTicks(t *testing.T) {
	src := newSource("github")
	p, _, _ := newPoller(t, src)
	p.Tick, p.Every = 5*time.Millisecond, 20*time.Millisecond
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go p.Run(ctx)

	eventually(t, "repeated searches on the tick", func() bool { return src.scans() >= 4 })
	eventually(t, "a second collection once the feed came due", func() bool { return src.count("github") >= 2 })
	if scans, n := src.scans(), src.count("github"); n >= scans {
		t.Errorf("%d collections in %d ticks, want collections only when due", n, scans)
	}
}

func TestDefaultsAreGentle(t *testing.T) {
	p, _, _ := newPoller(t, newSource())
	if p.Every < time.Minute || p.Tick < time.Second || p.Tick >= p.Every {
		t.Errorf("Every = %v, Tick = %v; want feeds collected minutes apart", p.Every, p.Tick)
	}
}
