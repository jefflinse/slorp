package claude

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jefflinse/slorp/internal/hub"
)

const (
	// pluginName is the Slorp plugin in the marketplace at pluginSource,
	// which Claude Code keeps as pluginMarket, and pluginID what it calls
	// the plugin once installed from there.
	pluginName   = "slorp"
	pluginMarket = "slorp"
	pluginID     = pluginName + "@" + pluginMarket
	pluginSource = "https://github.com/jefflinse/slorp.git"

	// pluginEvery is how often Claude Code is asked whether it has the plugin.
	pluginEvery = time.Minute
	// installTimeout bounds an install, which fetches the marketplace.
	installTimeout = 2 * time.Minute
	// offerRetry is how long after the marketplace could not be asked for
	// the plugin's newest version it is asked again.
	offerRetry = time.Hour
)

// plugin asks Claude Code whether it has the Slorp plugin, and adds the
// version its marketplace last offered when that is newer. With Plugin set,
// the sessions started here are handed it directly, which counts as having it.
func (a *Adapter) plugin(ctx context.Context) (hub.Plugin, error) {
	if a.Plugin != "" {
		return hub.Plugin{Installed: true, Enabled: true}, nil
	}
	out, err := a.run(ctx, launchTimeout, "", "plugin", "list", "--json")
	if err != nil {
		return hub.Plugin{}, err
	}
	var rows []struct {
		ID      string `json:"id"`
		Version string `json:"version"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return hub.Plugin{}, err
	}
	for _, r := range rows {
		if r.ID == pluginID {
			p := hub.Plugin{Installed: true, Enabled: r.Enabled, Version: r.Version}
			a.offer.Lock()
			if newer(a.offer.version, r.Version) {
				p.Latest = a.offer.version
			}
			a.offer.Unlock()
			return p, nil
		}
	}
	return hub.Plugin{}, nil
}

// checkPlugin tells the hub whether Claude Code has the plugin, once every
// pluginEvery. While it has, the marketplace is asked for a newer version
// when first seen and every Updates after, without holding up the caller.
func (a *Adapter) checkPlugin(ctx context.Context) {
	if time.Since(a.pluginAt) < pluginEvery {
		return
	}
	a.pluginAt = time.Now()
	p, err := a.plugin(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("claude: plugin: %v", err)
		}
		return
	}
	a.Hub.SetPlugin(Agent, p)
	if !p.Installed || a.Plugin != "" || a.Updates <= 0 {
		return
	}
	a.offer.Lock()
	due := !time.Now().Before(a.offer.again)
	if due {
		a.offer.again = time.Now().Add(offerRetry)
	}
	a.offer.Unlock()
	if due {
		go func() {
			if err := a.checkOffer(ctx); err != nil && ctx.Err() == nil {
				log.Printf("claude: plugin updates: %v", err)
			}
		}()
	}
}

// checkOffer has Claude Code fetch the plugin's marketplace afresh, notes the
// version it lists, and tells the hub when that is newer than the one
// installed.
func (a *Adapter) checkOffer(ctx context.Context) error {
	v, err := a.offered(ctx)
	if err != nil {
		return err
	}
	a.offer.Lock()
	a.offer.version, a.offer.again = v, time.Now().Add(a.Updates)
	a.offer.Unlock()
	p, err := a.plugin(ctx)
	if err != nil {
		return err
	}
	a.Hub.SetPlugin(Agent, p)
	return nil
}

// offered returns the version of the plugin its marketplace lists, once
// Claude Code has fetched the marketplace afresh.
func (a *Adapter) offered(ctx context.Context) (string, error) {
	if _, err := a.run(ctx, installTimeout, "", "plugin", "marketplace", "update", pluginMarket); err != nil {
		return "", err
	}
	out, err := a.run(ctx, launchTimeout, "", "plugin", "marketplace", "list", "--json")
	if err != nil {
		return "", err
	}
	var markets []struct {
		Name string `json:"name"`
		Dir  string `json:"installLocation"`
	}
	if err := json.Unmarshal([]byte(out), &markets); err != nil {
		return "", err
	}
	for _, m := range markets {
		if m.Name != pluginMarket {
			continue
		}
		b, err := os.ReadFile(filepath.Join(m.Dir, ".claude-plugin", "marketplace.json"))
		if err != nil {
			return "", err
		}
		var listing struct {
			Plugins []struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"plugins"`
		}
		if err := json.Unmarshal(b, &listing); err != nil {
			return "", err
		}
		for _, p := range listing.Plugins {
			if p.Name == pluginName {
				return p.Version, nil
			}
		}
	}
	return "", errors.New("Claude Code's marketplaces do not list the Slorp plugin")
}

// newer reports whether version a comes after b, reading each as dotted
// numbers such as "0.1.5". A version that does not read so is neither newer
// nor older than any other.
func newer(a, b string) bool {
	as, bs := dotted(a), dotted(b)
	if as == nil || bs == nil {
		return false
	}
	for len(as) < len(bs) {
		as = append(as, 0)
	}
	for len(bs) < len(as) {
		bs = append(bs, 0)
	}
	return slices.Compare(as, bs) > 0
}

// dotted reads a version's dotted numbers, or returns nil when it has
// anything else.
func dotted(v string) (ns []int) {
	for part := range strings.SplitSeq(v, ".") {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return nil
		}
		ns = append(ns, n)
	}
	return ns
}

// Update brings the Slorp plugin up to the version its marketplace now
// lists. Sessions already open load it when they next reload their plugins.
// It fails when Plugin is set, since the plugin then comes from that
// directory.
func (a *Adapter) Update(ctx context.Context) error {
	if a.Plugin != "" {
		return errors.New("Slorp was started with -plugin, which hands sessions the plugin from a directory, so there is nothing to update")
	}
	v, err := a.offered(ctx)
	if err != nil {
		return err
	}
	a.offer.Lock()
	a.offer.version = v
	a.offer.Unlock()
	if _, err := a.run(ctx, installTimeout, "", "plugin", "update", pluginID); err != nil {
		return err
	}
	p, err := a.plugin(ctx)
	if err != nil {
		return err
	}
	a.Hub.SetPlugin(Agent, p)
	return nil
}

// Install gives Claude Code the Slorp plugin, set to reach Slorp at URL, or
// turns it back on when it is installed and off. Sessions already open load
// it when they next reload their plugins.
func (a *Adapter) Install(ctx context.Context) error {
	p, err := a.plugin(ctx)
	if err != nil {
		return err
	}
	switch {
	case p.Installed && p.Enabled:
	case p.Installed:
		_, err = a.run(ctx, launchTimeout, "", "plugin", "enable", pluginID)
	default:
		args := []string{"plugin", "install", pluginName, "--marketplace", pluginSource}
		if a.URL != "" {
			args = append(args, "--config", "url="+a.URL)
		}
		_, err = a.run(ctx, installTimeout, "", args...)
	}
	if err != nil {
		return err
	}
	if p, err = a.plugin(ctx); err != nil {
		return err
	}
	a.Hub.SetPlugin(Agent, p)
	return nil
}

// Uninstall takes the Slorp plugin away from Claude Code. Sessions already
// open keep it until they next reload their plugins. It fails when Plugin
// is set, since the plugin then comes from that directory.
func (a *Adapter) Uninstall(ctx context.Context) error {
	if a.Plugin != "" {
		return errors.New("Slorp was started with -plugin, which hands sessions the plugin from a directory, so there is nothing to uninstall")
	}
	p, err := a.plugin(ctx)
	if err != nil {
		return err
	}
	if p.Installed {
		if _, err := a.run(ctx, launchTimeout, "", "plugin", "uninstall", pluginID); err != nil {
			return err
		}
	}
	if p, err = a.plugin(ctx); err != nil {
		return err
	}
	a.Hub.SetPlugin(Agent, p)
	return nil
}
