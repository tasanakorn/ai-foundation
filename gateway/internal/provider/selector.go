package provider

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
	"sort"
	"sync"

	"ai-foundation/gateway/internal/config"
)

// Selector orders weighted choices by a policy: the accounts of a provider (spec-0004
// section 4) and the targets of a model alias (spec-0005), one engine for both (D32).
// It holds only the round-robin position; callers say which choices are usable.
type Selector struct {
	policy, sticky string
	names          []string
	weights        []int

	mu      sync.Mutex
	current []int // smooth weighted round robin
}

// NewSelector makes a selector over names with their weights (round_robin only).
func NewSelector(policy, sticky string, names []string, weights []int) *Selector {
	return &Selector{policy: policy, sticky: sticky, names: names, weights: weights, current: make([]int, len(names))}
}

// Order returns the usable choices (indexes), first pick first:
//   - priority: config order;
//   - round_robin: the weighted pick (or, with sticky, the identity's choice by weighted
//     rendezvous hashing), then the other choices, then weight 0 ones (drained).
func (s *Selector) Order(id Identity, usable func(int) bool) []int {
	var active, drained []int
	for i := range s.names {
		if !usable(i) {
			continue
		}
		if s.policy == config.PolicyRoundRobin && s.weights[i] == 0 {
			drained = append(drained, i)
		} else {
			active = append(active, i)
		}
	}
	if s.policy != config.PolicyRoundRobin || len(active) == 0 {
		return append(active, drained...)
	}
	if key := s.stickyKey(id); key != "" {
		sort.SliceStable(active, func(a, b int) bool { return s.score(key, active[a]) > s.score(key, active[b]) })
		return append(active, drained...)
	}
	first := s.swrr(active)
	out := make([]int, 0, len(active)+len(drained))
	out = append(out, first)
	for _, i := range active {
		if i != first {
			out = append(out, i)
		}
	}
	return append(out, drained...)
}

// swrr is smooth weighted round robin (the nginx method): picks follow the weights
// and spread evenly inside each cycle (3:1 gives A A B A).
func (s *Selector) swrr(active []int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total, best := 0, -1
	for _, i := range active {
		s.current[i] += s.weights[i]
		total += s.weights[i]
		if best < 0 || s.current[i] > s.current[best] {
			best = i
		}
	}
	s.current[best] -= total
	return best
}

func (s *Selector) stickyKey(id Identity) string {
	switch s.sticky {
	case config.StickyKey:
		return id.KeyID
	case config.StickyEndUser:
		if id.EndUser != "" {
			return "u:" + id.EndUser
		}
		return id.KeyID
	case config.StickySession:
		if id.Session != "" {
			return "s:" + id.Session
		}
		return id.KeyID
	}
	return ""
}

// score is weighted rendezvous hashing: the choice with the highest score gets the
// identity, and a choice's share of identities follows its weight.
func (s *Selector) score(key string, i int) float64 {
	h := sha256.Sum256([]byte(key + "\x00" + s.names[i]))
	u := (float64(binary.BigEndian.Uint64(h[:8])>>11) + 0.5) / (1 << 53) // uniform in (0, 1)
	return float64(s.weights[i]) / -math.Log(u)
}

// Inherit takes the round-robin position of choices with the same names (a catalog reload).
func (s *Selector) Inherit(old *Selector) {
	if old == nil || old == s {
		return
	}
	old.mu.Lock()
	prev := make(map[string]int, len(old.names))
	for i, n := range old.names {
		prev[n] = old.current[i]
	}
	old.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, n := range s.names {
		s.current[i] = prev[n]
	}
}
