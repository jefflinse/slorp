package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"

	"github.com/jefflinse/slorp/internal/hub"
)

type nativeAgent struct {
	Name        string                                `json:"name"`
	Description string                                `json:"description"`
	Mode        string                                `json:"mode"`
	Hidden      bool                                  `json:"hidden"`
	Model       *struct{ ProviderID, ModelID string } `json:"model"`
	Variant     string                                `json:"variant"`
}

type catalog struct {
	hub.LaunchOptions
	agents      []nativeAgent
	baseDefault string
}

// LaunchOptions returns only public, usable choices from the requested directory.
func (a *Adapter) LaunchOptions(ctx context.Context, dir string) (hub.LaunchOptions, error) {
	ctx, release, err := a.operation(ctx)
	if err != nil {
		return hub.LaunchOptions{}, err
	}
	defer release()
	r, err := a.configured(ctx, dir)
	if err != nil {
		return hub.LaunchOptions{}, err
	}
	defer a.finish(r)
	c, err := r.catalog(ctx)
	return c.LaunchOptions, err
}

func (r *runtime) catalog(ctx context.Context) (catalog, error) {
	c := catalog{LaunchOptions: hub.LaunchOptions{Agents: []hub.LaunchAgent{}, Models: []hub.LaunchModel{}}}
	var agents []nativeAgent
	if err := r.request(ctx, http.MethodGet, "/agent", nil, &agents); err != nil {
		return c, err
	}
	var providers struct {
		All []struct {
			ID, Name string
			Models   map[string]struct {
				ID, Name, Status string
				Capabilities     struct{ Input, Output struct{ Text bool } }
				Variants         map[string]struct{ Disabled bool }
			}
		}
		Connected []string
		Default   map[string]string
	}
	if err := r.request(ctx, http.MethodGet, "/provider", nil, &providers); err != nil {
		return c, err
	}
	var config struct {
		Model        string                     `json:"model"`
		DefaultAgent string                     `json:"default_agent"`
		Provider     map[string]json.RawMessage `json:"provider"`
		Agent        json.RawMessage            `json:"agent"`
	}
	if err := r.request(ctx, http.MethodGet, "/config", nil, &config); err != nil {
		return c, err
	}
	for _, a := range agents {
		if a.Hidden || a.Name == "" || (a.Mode != "primary" && a.Mode != "all") {
			continue
		}
		c.agents = append(c.agents, a)
		c.Agents = append(c.Agents, hub.LaunchAgent{Name: a.Name, Description: a.Description})
	}
	for _, p := range providers.All {
		if p.ID == "" || !slices.Contains(providers.Connected, p.ID) {
			continue
		}
		for key, m := range p.Models {
			if m.Status == "deprecated" || !m.Capabilities.Input.Text || !m.Capabilities.Output.Text {
				continue
			}
			if m.ID == "" {
				m.ID = key
			}
			if m.ID == "" {
				continue
			}
			choice := hub.LaunchModel{ID: p.ID + "/" + m.ID, Name: m.Name, Provider: p.Name, Variants: []string{}}
			for name, v := range m.Variants {
				if name != "" && !v.Disabled {
					choice.Variants = append(choice.Variants, name)
				}
			}
			slices.Sort(choice.Variants)
			c.Models = append(c.Models, choice)
		}
	}
	sort.Slice(c.Agents, func(i, j int) bool { return c.Agents[i].Name < c.Agents[j].Name })
	sort.Slice(c.Models, func(i, j int) bool { return c.Models[i].ID < c.Models[j].ID })
	if config.DefaultAgent != "" {
		if c.agent(config.DefaultAgent) == nil {
			return c, fmt.Errorf("%w: unknown or unavailable default root agent %q", hub.ErrLaunchInvalid, config.DefaultAgent)
		}
		c.Defaults.AgentName = config.DefaultAgent
	} else {
		// Native defaultInfo uses insertion order, not the alphabetic /agent list.
		var names map[string]struct{ Name string }
		if len(config.Agent) > 0 {
			if err := json.Unmarshal(config.Agent, &names); err != nil {
				return c, fmt.Errorf("reading native agent names: %w", err)
			}
		}
		for _, name := range []string{"build", "plan", "general", "explore", "compaction", "title", "summary"} {
			if override := names[name].Name; override != "" {
				name = override
			}
			if c.agent(name) != nil {
				c.Defaults.AgentName = name
				break
			}
		}
		if c.Defaults.AgentName == "" && len(c.agents) > 0 {
			decoder := json.NewDecoder(bytes.NewReader(config.Agent))
			if token, err := decoder.Token(); err == nil && token == json.Delim('{') {
				for decoder.More() {
					token, err := decoder.Token()
					if err != nil {
						return c, fmt.Errorf("reading native agent order: %w", err)
					}
					var entry struct{ Name string }
					if err := decoder.Decode(&entry); err != nil {
						return c, fmt.Errorf("reading native agent: %w", err)
					}
					name, _ := token.(string)
					if entry.Name != "" {
						name = entry.Name
					}
					if c.agent(name) != nil {
						c.Defaults.AgentName = name
						break
					}
				}
			}
		}
	}
	c.baseDefault = config.Model
	if c.baseDefault == "" {
		c.baseDefault = r.recentModel(c)
	}
	if c.baseDefault == "" {
		for _, provider := range providers.Connected {
			if len(config.Provider) > 0 {
				if _, ok := config.Provider[provider]; !ok {
					continue
				}
			}
			c.baseDefault = provider + "/" + providers.Default[provider]
			break
		}
	}
	c.Defaults.Model = c.baseDefault
	if a := c.agent(c.Defaults.AgentName); a != nil {
		if a.Model != nil {
			c.Defaults.Model = a.Model.ProviderID + "/" + a.Model.ModelID
		}
		if m := c.model(c.Defaults.Model); m != nil && slices.Contains(m.Variants, a.Variant) {
			c.Defaults.Variant = a.Variant
		}
	}
	return c, nil
}

func (r *runtime) recentModel(c catalog) string {
	state := runtimeEnv(r, "XDG_STATE_HOME")
	if state == "" {
		state = filepath.Join(runtimeEnv(r, "HOME"), ".local", "state")
	}
	data, err := os.ReadFile(filepath.Join(state, "opencode", "model.json"))
	if err != nil {
		return "" // Native ignores unreadable or malformed recent-model state.
	}
	var stored struct {
		Recent []json.RawMessage `json:"recent"`
	}
	if json.Unmarshal(data, &stored) != nil {
		return ""
	}
	for _, raw := range stored.Recent {
		var entry struct{ ProviderID, ModelID string }
		if json.Unmarshal(raw, &entry) == nil && c.model(entry.ProviderID+"/"+entry.ModelID) != nil {
			return entry.ProviderID + "/" + entry.ModelID
		}
	}
	return ""
}

func (c catalog) agent(name string) *nativeAgent {
	for _, a := range c.agents {
		if a.Name == name {
			return &a
		}
	}
	return nil
}

func (c catalog) model(id string) *hub.LaunchModel {
	for _, m := range c.Models {
		if m.ID == id {
			return &m
		}
	}
	return nil
}

func (c catalog) choose(l *hub.Launch) error {
	if l.AgentName == "" {
		l.AgentName = c.Defaults.AgentName
	}
	a := c.agent(l.AgentName)
	if a == nil {
		return fmt.Errorf("%w: unknown or unavailable root agent %q", hub.ErrLaunchInvalid, l.AgentName)
	}
	if l.Model == "" {
		l.Model = c.baseDefault
		if a.Model != nil {
			l.Model = a.Model.ProviderID + "/" + a.Model.ModelID
		}
	}
	m := c.model(l.Model)
	if m == nil {
		return fmt.Errorf("%w: unknown or unavailable provider/model %q", hub.ErrLaunchInvalid, l.Model)
	}
	if l.Variant == "" && slices.Contains(m.Variants, a.Variant) {
		l.Variant = a.Variant
	}
	if l.Variant != "" && !slices.Contains(m.Variants, l.Variant) {
		return fmt.Errorf("%w: variant %q is unavailable for %s", hub.ErrLaunchInvalid, l.Variant, l.Model)
	}
	return nil
}
