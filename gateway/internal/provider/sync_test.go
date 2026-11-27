package provider

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"ai-foundation/gateway/internal/config"
)

type syncRec struct {
	mu  sync.Mutex
	got []AccountEvent
}

func (s *syncRec) Publish(ev AccountEvent) {
	s.mu.Lock()
	s.got = append(s.got, ev)
	s.mu.Unlock()
}

func (s *syncRec) take() []AccountEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.got
	s.got = nil
	return out
}

// A rest and the success that ends it are shared; plain successes are not.
func TestPoolPublishesStateChanges(t *testing.T) {
	p, f, _ := newTestPool(t, "openai", config.PolicyPriority, config.StickyNone, "a", -1, "b", -1)
	rec := &syncRec{}
	p.sync = rec

	call(t, p, Identity{})
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("plain success published %+v", got)
	}

	f.set("a", 503, nil, `{}`)
	call(t, p, Identity{})
	got := rec.take()
	if len(got) != 1 || got[0].Provider != "p" || got[0].Account != "a" || !got[0].Resting(time.Now()) ||
		got[0].LastError != "503 Service Unavailable" || got[0].LastErrorAt == nil || got[0].At.IsZero() {
		t.Fatalf("rest: %+v", got)
	}

	// Every account resting: a is tried first again, succeeds, and its rest ends.
	f.set("a", 0, nil, "")
	p.members[1].until = time.Now().Add(time.Hour)
	call(t, p, Identity{})
	got = rec.take()
	if len(got) != 1 || got[0].Account != "a" || got[0].Until != nil || got[0].LastError != "" {
		t.Fatalf("rest ended: %+v", got)
	}
	if rs := p.Resting(); len(rs) != 1 || rs[0].Account != "b" {
		t.Fatalf("resting: %+v", rs)
	}
}

// A rest from another node makes this node skip the account; older news is ignored.
func TestPoolApply(t *testing.T) {
	p, f, _ := newTestPool(t, "openai", config.PolicyPriority, config.StickyNone, "a", -1, "b", -1)
	now := time.Now()
	until, errAt := now.Add(time.Minute), now
	ev := AccountEvent{Provider: "p", Account: "a", Until: &until, LastError: "429 Too Many Requests", LastErrorAt: &errAt, Node: "n2", At: now}

	if !p.Apply(ev) {
		t.Fatal("first event not applied")
	}
	if p.Apply(ev) {
		t.Fatal("same event applied twice")
	}
	if _, _, tr := call(t, p, Identity{}); tr.Account != "b" || tr.Attempts != 1 {
		t.Fatalf("a should rest: %+v", tr)
	}
	if a := p.State().Accounts[0]; a.State != "cooldown" || a.LastError != "429 Too Many Requests" {
		t.Fatalf("state: %+v", a)
	}

	older := AccountEvent{Provider: "p", Account: "a", At: now.Add(-time.Second)}
	if p.Apply(older) {
		t.Fatal("older event applied")
	}
	for _, bad := range []AccountEvent{
		{Provider: "other", Account: "a", At: now.Add(time.Second)},
		{Provider: "p", Account: "zzz", At: now.Add(time.Second)},
	} {
		if p.Apply(bad) {
			t.Fatalf("applied %+v", bad)
		}
	}

	// A newer "ok" from another node ends the rest here too.
	if !p.Apply(AccountEvent{Provider: "p", Account: "a", At: now.Add(time.Second)}) {
		t.Fatal("newer ok not applied")
	}
	f.set("b", 500, http.Header{}, `{}`)
	if _, _, tr := call(t, p, Identity{}); tr.Account != "a" || tr.Attempts != 1 {
		t.Fatalf("a should be back: %+v", tr)
	}
}

// A local change after an applied event wins over it (newest at).
func TestPoolLocalChangeAfterApply(t *testing.T) {
	p, f, _ := newTestPool(t, "openai", config.PolicyPriority, config.StickyNone, "a", -1, "b", -1)
	past := time.Now().Add(-time.Minute)
	until := past.Add(time.Second) // already over
	p.Apply(AccountEvent{Provider: "p", Account: "a", Until: &until, At: past})
	f.set("a", 503, nil, `{}`)
	call(t, p, Identity{})
	if p.Apply(AccountEvent{Provider: "p", Account: "a", At: past.Add(time.Second)}) {
		t.Fatal("stale event overwrote a newer local rest")
	}
	if a := p.State().Accounts[0]; a.State != "cooldown" {
		t.Fatalf("a: %+v", a)
	}
}
