package claude

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"time"

	"github.com/jefflinse/slorp/internal/hub"
)

const (
	// pluginName is the Slorp plugin in the marketplace at pluginSource,
	// and pluginID what Claude Code calls it once installed from there.
	pluginName   = "slorp"
	pluginID     = "slorp@slorp"
	pluginSource = "https://github.com/jefflinse/slorp.git"

	// pluginEvery is how often Claude Code is asked whether it has the plugin.
	pluginEvery = time.Minute
	// installTimeout bounds an install, which fetches the marketplace.
	installTimeout = 2 * time.Minute
)

// plugin asks Claude Code whether it has the Slorp plugin. With Plugin set,
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
			return hub.Plugin{Installed: true, Enabled: r.Enabled, Version: r.Version}, nil
		}
	}
	return hub.Plugin{}, nil
}

// checkPlugin tells the hub whether Claude Code has the plugin, once every
// pluginEvery.
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
