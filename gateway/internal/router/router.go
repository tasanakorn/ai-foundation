// Package router maps a client-facing model name to a provider and upstream model.
package router

import (
	"slices"
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
	Formats  []string // API formats that may use this route
}

// Serves reports whether a client in this API format may use the route.
func (r Route) Serves(format string) bool { return slices.Contains(r.Formats, format) }

type ModelInfo struct {
	ID       string        `json:"id"`
	Provider string        `json:"provider"`
	Upstream string        `json:"upstream"`
	Price    *config.Price `json:"price,omitempty"`
	Formats  []string      `json:"formats"`
}

// Source gives the router of the current catalog snapshot (spec-0006 section 5).
// A request calls Router once and keeps the result, so a reload never changes it halfway.
type Source interface {
	Router() *Router
}

type Router struct {
	providers   map[string]provider.Provider
	models      map[string]config.Model
	passthrough bool
}

func New(models []config.Model, providers map[string]provider.Provider, passthrough bool) *Router {
	m := make(map[string]config.Model, len(models))
	for _, x := range models {
		if len(x.Formats) == 0 { // config.Parse sets these; this covers hand-built configs
			x.Formats = providerFormats(providers[x.Provider])
		}
		m[x.Name] = x
	}
	return &Router{providers: providers, models: m, passthrough: passthrough}
}

// Resolve finds a route. Order:
//  1. exact alias from config ("gpt-4o-mini")
//  2. if passthrough is on: "<provider>/<upstream model>" ("openrouter/anthropic/claude-sonnet-4.5")
func (r *Router) Resolve(model string) (Route, bool) {
	if m, ok := r.models[model]; ok {
		return Route{Model: model, Provider: r.providers[m.Provider], Upstream: m.Upstream, Price: m.Price, Formats: m.Formats}, true
	}
	if r.passthrough {
		if i := strings.IndexByte(model, '/'); i > 0 && i < len(model)-1 {
			if p, ok := r.providers[model[:i]]; ok {
				return Route{Model: model, Provider: p, Upstream: model[i+1:], Formats: providerFormats(p)}, true
			}
		}
	}
	return Route{}, false
}

// Models lists configured aliases, sorted by name.
func (r *Router) Models() []ModelInfo {
	out := make([]ModelInfo, 0, len(r.models))
	for _, m := range r.models {
		out = append(out, ModelInfo{ID: m.Name, Provider: m.Provider, Upstream: m.Upstream, Price: m.Price, Formats: m.Formats})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Provider returns a provider by name.
func (r *Router) Provider(name string) (provider.Provider, bool) {
	p, ok := r.providers[name]
	return p, ok
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

// Router makes a fixed *Router a Source (tests, single-snapshot setups).
func (r *Router) Router() *Router { return r }

// providerFormats is the default format list for "<provider>/<model>" names (spec-0002 section 4.4).
func providerFormats(p provider.Provider) []string {
	if n, ok := p.(provider.Native); ok {
		return []string{config.FormatOpenAI, n.NativeFormat()}
	}
	return []string{config.FormatOpenAI}
}
