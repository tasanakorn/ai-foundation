package provider

import "time"

// AccountEvent is the state of one account, shared by all nodes (spec-0004 section 4.4,
// D40). It is the JSON value of the NATS KV bucket, a public contract: add fields only.
type AccountEvent struct {
	Provider    string     `json:"provider"`
	Account     string     `json:"account"`
	Until       *time.Time `json:"until,omitempty"` // nil = not resting
	LastError   string     `json:"last_error,omitempty"`
	LastErrorAt *time.Time `json:"last_error_at,omitempty"`
	Node        string     `json:"node"`
	At          time.Time  `json:"at"` // when the state changed; the newest wins
}

// Resting reports whether the event rests the account at now.
func (e AccountEvent) Resting(now time.Time) bool { return e.Until != nil && now.Before(*e.Until) }

// StateSync shares account state with the other nodes. Publish must not block.
type StateSync interface {
	Publish(AccountEvent)
}

// event is m's state as an AccountEvent. Call with Pool.mu held.
func (p *Pool) event(m *member) AccountEvent {
	ev := AccountEvent{Provider: p.name, Account: m.name, LastError: m.lastErr, At: m.changed}
	if !m.until.IsZero() {
		u := m.until.UTC()
		ev.Until = &u
	}
	if !m.lastAt.IsZero() {
		at := m.lastAt.UTC()
		ev.LastErrorAt = &at
	}
	return ev
}

// Apply takes the state of one account from another node, when it is newer than what
// this node has. It reports whether anything changed.
func (p *Pool) Apply(ev AccountEvent) bool {
	if ev.Provider != p.name {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, m := range p.members {
		if m.name != ev.Account {
			continue
		}
		if !ev.At.After(m.changed) {
			return false
		}
		m.until, m.lastErr, m.lastAt, m.changed = time.Time{}, ev.LastError, time.Time{}, ev.At
		if ev.Until != nil {
			m.until = *ev.Until
		}
		if ev.LastErrorAt != nil {
			m.lastAt = *ev.LastErrorAt
		}
		return true
	}
	return false
}

// Resting lists the accounts that rest now, for writing them again after a reconnect.
func (p *Pool) Resting() []AccountEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	var out []AccountEvent
	for _, m := range p.members {
		if now.Before(m.until) {
			out = append(out, p.event(m))
		}
	}
	return out
}
