package opencode

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"

	"github.com/jefflinse/slorp/internal/hub"
)

type nativeAgent struct {
	Name        string       `json:"id"`
	Description string       `json:"description"`
	Mode        string       `json:"mode"`
	Hidden      bool         `json:"hidden"`
	Model       *nativeModel `json:"model"`
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
	var models []struct {
		ID, ProviderID, Name, Type, Status string
		Enabled                            bool
		Capabilities                       struct{ Input, Output []string }
		Variants                           []struct{ ID string }
	}
	if err := r.request(ctx, http.MethodGet, "/api/model", nil, &models); err != nil {
		return c, err
	}
	var providers []struct{ ID, Name string }
	if err := r.request(ctx, http.MethodGet, "/api/provider", nil, &providers); err != nil {
		return c, err
	}
	names := make(map[string]string, len(providers))
	for _, provider := range providers {
		names[provider.ID] = provider.Name
	}
	for _, model := range models {
		if !model.Enabled || model.Status == "deprecated" || model.ID == "" || model.ProviderID == "" ||
			(model.Type != "" && model.Type != "chat") || !slices.Contains(model.Capabilities.Input, "text") || !slices.Contains(model.Capabilities.Output, "text") {
			continue
		}
		choice := hub.LaunchModel{ID: model.ProviderID + "/" + model.ID, Name: model.Name, Provider: names[model.ProviderID], Variants: []string{}}
		for _, variant := range model.Variants {
			if variant.ID != "" {
				choice.Variants = append(choice.Variants, variant.ID)
			}
		}
		slices.Sort(choice.Variants)
		c.Models = append(c.Models, choice)
	}
	var agents []nativeAgent
	if err := r.request(ctx, http.MethodGet, "/api/agent", nil, &agents); err != nil {
		return c, err
	}
	for _, agent := range agents {
		if agent.Name == "" || agent.Hidden || agent.Mode != "primary" && agent.Mode != "all" {
			continue
		}
		c.agents = append(c.agents, agent)
		c.Agents = append(c.Agents, hub.LaunchAgent{Name: agent.Name, Description: agent.Description})
	}
	// V2 orders the agent list with the effective default first, including plugins.
	if len(c.agents) > 0 {
		c.Defaults.AgentName = c.agents[0].Name
	}
	if c.agent(c.Defaults.AgentName) == nil {
		return c, fmt.Errorf("%w: unknown or unavailable default root agent %q", hub.ErrLaunchInvalid, c.Defaults.AgentName)
	}
	var model *nativeModel
	if err := r.request(ctx, http.MethodGet, "/api/model/default", nil, &model); err != nil {
		return c, err
	}
	if model != nil {
		c.baseDefault = model.ProviderID + "/" + model.ID
	}
	c.Defaults.Model = c.baseDefault
	if agent := c.agent(c.Defaults.AgentName); agent != nil && agent.Model != nil {
		c.Defaults.Model = agent.Model.ProviderID + "/" + agent.Model.ID
		if m := c.model(c.Defaults.Model); m != nil && slices.Contains(m.Variants, agent.Model.Variant) {
			c.Defaults.Variant = agent.Model.Variant
		}
	}
	sort.Slice(c.Agents, func(i, j int) bool { return c.Agents[i].Name < c.Agents[j].Name })
	sort.Slice(c.Models, func(i, j int) bool { return c.Models[i].ID < c.Models[j].ID })
	return c, nil
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
			l.Model = a.Model.ProviderID + "/" + a.Model.ID
		}
	}
	m := c.model(l.Model)
	if m == nil {
		return fmt.Errorf("%w: unknown or unavailable provider/model %q", hub.ErrLaunchInvalid, l.Model)
	}
	if l.Variant == "" && a.Model != nil && slices.Contains(m.Variants, a.Model.Variant) {
		l.Variant = a.Model.Variant
	}
	if l.Variant != "" && !slices.Contains(m.Variants, l.Variant) {
		return fmt.Errorf("%w: variant %q is unavailable for %s", hub.ErrLaunchInvalid, l.Variant, l.Model)
	}
	return nil
}
