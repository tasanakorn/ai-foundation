// Package router maps a client-facing model name to its targets: a provider and the
// upstream model on it (spec-0005). An alias with several targets picks one with the
// same policy engine as provider accounts, and falls back to the next on errors
// another target could fix.
package router

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sort"
	"strings"

	"ai-foundation/gateway/internal/config"
	"ai-foundation/gateway/internal/provider"
)

// Target is one backend of an alias.
type Target struct {
	Provider provider.Provider
	Upstream string        // model name sent to the provider
	Price    *config.Price // price on this provider
	Formats  []string      // API formats this target serves
}

func (t Target) serves(format string) bool { return slices.Contains(t.Formats, format) }

// Route is a resolved model name. Provider, Upstream and Price are one target: the
// first in config order after Resolve, the planned first after With(Plan(...)[0]),
// and the one that answered after With(the target Call returned).
type Route struct {
	Model    string // name the client sent
	Provider provider.Provider
	Upstream string
	Price    *config.Price
	Formats  []string // API formats that may use this route
	Targets  []Target

	alias *alias // selection state; nil for "<provider>/<model>" names
}

type alias struct {
	model       config.Model
	sel         *provider.Selector
	maxAttempts int
}

// Serves reports whether a client in this API format may use the route.
func (r Route) Serves(format string) bool { return slices.Contains(r.Formats, format) }

// With returns the route with t as its current target.
func (r Route) With(t Target) Route {
	r.Provider, r.Upstream, r.Price = t.Provider, t.Upstream, t.Price
	return r
}

// Plan lists the targets to try for one request, in policy order: only targets that
// serve format, and only those whose provider has a usable account (spec-0005 section
// 3). When none is usable, the targets that serve format are tried in config order.
func (r Route) Plan(format string, id provider.Identity) []Target {
	usable := func(i int) bool {
		t := r.Targets[i]
		if !t.serves(format) {
			return false
		}
		pool, ok := provider.AsPool(t.Provider)
		return !ok || pool.Available()
	}
	var idx []int
	if r.alias != nil {
		idx = r.alias.sel.Order(id, usable)
	} else {
		for i := range r.Targets {
			if usable(i) {
				idx = append(idx, i)
			}
		}
	}
	if len(idx) == 0 {
		for i, t := range r.Targets {
			if t.serves(format) {
				idx = append(idx, i)
			}
		}
	}
	out := make([]Target, 0, len(idx))
	for _, i := range idx {
		out = append(out, r.Targets[i])
	}
	return out
}

// Call sends the request to the planned targets in order until one answers, with the
// failover rules of spec-0004 section 5: only before the first byte, only on errors
// another target could fix. The pools report each outcome through the request's
// provider.Trace; without one (a hand-built provider) there is no failover. Call
// returns the response and the target that gave it.
func (r Route) Call(ctx context.Context, plan []Target, send func(Target) (*http.Response, error)) (*http.Response, Target, error) {
	if len(plan) == 0 {
		return nil, Target{}, errNoTarget
	}
	tr := provider.TraceFrom(ctx)
	if tr != nil && r.alias != nil && r.alias.maxAttempts > 0 {
		tr.Limit = r.alias.maxAttempts
	}
	var resp *http.Response
	var err error
	var used Target
	for i, t := range plan {
		resp, err = send(t)
		used = t
		if ctx.Err() != nil || tr == nil {
			return resp, used, err
		}
		retry := tr.Outcome != "" && tr.Outcome != provider.OutcomeOK && tr.Outcome != provider.OutcomeClientError
		last := i == len(plan)-1 || tr.Limit > 0 && tr.Attempts >= tr.Limit
		if !retry || last {
			return resp, used, err
		}
		if resp != nil {
			resp.Body.Close()
		}
	}
	return resp, used, err
}

var errNoTarget = errors.New("no target serves this API format")

// TargetInfo is one target as the admin API shows it.
type TargetInfo struct {
	Provider string        `json:"provider"`
	Upstream string        `json:"upstream"`
	Price    *config.Price `json:"price,omitempty"`
	Weight   int           `json:"weight"`
	Formats  []string      `json:"formats"`
}

type ModelInfo struct {
	ID       string        `json:"id"`
	Provider string        `json:"provider"` // first target
	Upstream string        `json:"upstream"` // first target
	Price    *config.Price `json:"price,omitempty"`
	Formats  []string      `json:"formats"`
	Policy   string        `json:"policy"`
	Targets  []TargetInfo  `json:"targets"`
}

type Router struct {
	providers   map[string]provider.Provider
	models      map[string]*alias
	passthrough bool
}

// Source gives the router of the current catalog snapshot (spec-0006 section 5).
// A request calls Router once and keeps the result, so a reload never changes it halfway.
type Source interface {
	Router() *Router
}

// New builds a router. models must be normalized (config.NormalizeCatalog): every
// model has at least one target.
func New(models []config.Model, providers map[string]provider.Provider, passthrough bool) *Router {
	m := make(map[string]*alias, len(models))
	for _, x := range models {
		names, weights := make([]string, len(x.Targets)), make([]int, len(x.Targets))
		for i, t := range x.Targets {
			names[i], weights[i] = t.Key(), t.WeightOr()
		}
		m[x.Name] = &alias{model: x, sel: provider.NewSelector(x.Policy, x.Sticky, names, weights), maxAttempts: x.MaxAttempts}
	}
	return &Router{providers: providers, models: m, passthrough: passthrough}
}

// Resolve finds a route. Order:
//  1. exact alias from config ("gpt-4o-mini")
//  2. if passthrough is on: "<provider>/<upstream model>" ("openrouter/anthropic/claude-sonnet-4.5")
func (r *Router) Resolve(model string) (Route, bool) {
	if a, ok := r.models[model]; ok {
		rt := Route{Model: model, Formats: a.model.Formats, alias: a}
		for _, t := range a.model.Targets {
			rt.Targets = append(rt.Targets, Target{Provider: r.providers[t.Provider], Upstream: t.Upstream, Price: t.Price, Formats: t.Formats})
		}
		return rt.With(rt.Targets[0]), true
	}
	if r.passthrough {
		if i := strings.IndexByte(model, '/'); i > 0 && i < len(model)-1 {
			if p, ok := r.providers[model[:i]]; ok {
				t := Target{Provider: p, Upstream: model[i+1:], Formats: providerFormats(p)}
				return Route{Model: model, Formats: t.Formats, Targets: []Target{t}}.With(t), true
			}
		}
	}
	return Route{}, false
}

// Models lists configured aliases, sorted by name.
func (r *Router) Models() []ModelInfo {
	out := make([]ModelInfo, 0, len(r.models))
	for _, a := range r.models {
		m := a.model
		info := ModelInfo{ID: m.Name, Formats: m.Formats, Policy: m.Policy, Targets: []TargetInfo{}}
		for _, t := range m.Targets {
			info.Targets = append(info.Targets, TargetInfo{Provider: t.Provider, Upstream: t.Upstream, Price: t.Price, Weight: t.WeightOr(), Formats: t.Formats})
		}
		info.Provider, info.Upstream, info.Price = m.Targets[0].Provider, m.Targets[0].Upstream, m.Targets[0].Price
		out = append(out, info)
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

// InheritState takes the round-robin positions of aliases with the same names (a catalog reload).
func (r *Router) InheritState(old *Router) {
	if old == nil || old == r {
		return
	}
	for name, a := range r.models {
		if o, ok := old.models[name]; ok {
			a.sel.Inherit(o.sel)
		}
	}
}

// providerFormats is the default format list for "<provider>/<model>" names (spec-0002 section 4.4).
func providerFormats(p provider.Provider) []string {
	if n, ok := p.(provider.Native); ok {
		return []string{config.FormatOpenAI, n.NativeFormat()}
	}
	return []string{config.FormatOpenAI}
}
