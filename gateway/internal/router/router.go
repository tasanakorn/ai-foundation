// Package router maps a client-facing model name to a provider and upstream model.
package router

import (
	"sort"
	"strings"

	"ai-foundation/gateway/internal/config"
	"ai-foundation/gateway/internal/provider"
)

type Route struct {
	Model    string // name the client sent
	Provider provider.Provider
	Upstream string // name sent to the provider
	Price    *config.Price
}

type ModelInfo struct {
	ID       string        `json:"id"`
	Provider string        `json:"provider"`
	Upstream string        `json:"upstream"`
	Price    *config.Price `json:"price,omitempty"`
}

type Router struct {
	providers   map[string]provider.Provider
	models      map[string]config.Model
	passthrough bool
}

func New(models []config.Model, providers map[string]provider.Provider, passthrough bool) *Router {
	m := make(map[string]config.Model, len(models))
	for _, x := range models {
		m[x.Name] = x
	}
	return &Router{providers: providers, models: m, passthrough: passthrough}
}

// Resolve finds a route. Order:
//  1. exact alias from config ("gpt-4o-mini")
//  2. if passthrough is on: "<provider>/<upstream model>" ("openrouter/anthropic/claude-sonnet-4.5")
func (r *Router) Resolve(model string) (Route, bool) {
	if m, ok := r.models[model]; ok {
		return Route{Model: model, Provider: r.providers[m.Provider], Upstream: m.Upstream, Price: m.Price}, true
	}
	if r.passthrough {
		if i := strings.IndexByte(model, '/'); i > 0 && i < len(model)-1 {
			if p, ok := r.providers[model[:i]]; ok {
				return Route{Model: model, Provider: p, Upstream: model[i+1:]}, true
			}
		}
	}
	return Route{}, false
}

// Models lists configured aliases, sorted by name.
func (r *Router) Models() []ModelInfo {
	out := make([]ModelInfo, 0, len(r.models))
	for _, m := range r.models {
		out = append(out, ModelInfo{ID: m.Name, Provider: m.Provider, Upstream: m.Upstream, Price: m.Price})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Providers lists provider names, sorted.
func (r *Router) Providers() []string {
	out := make([]string, 0, len(r.providers))
	for name := range r.providers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (r *Router) Passthrough() bool { return r.passthrough }
