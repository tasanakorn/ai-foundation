package bus_test

import (
	"context"
	"testing"
	"time"

	"ai-foundation/gateway/internal/bus"
	"ai-foundation/gateway/internal/natstest"
	"ai-foundation/gateway/internal/provider"
)

func node(t *testing.T, url string) (*bus.Bus, *bus.AccountSync, chan provider.AccountEvent) {
	t.Helper()
	b, err := bus.Connect(context.Background(), natstest.Config(t, url), quiet)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close(context.Background()) })
	s, err := b.AccountSync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan provider.AccountEvent, 16)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := s.Watch(ctx, func(ev provider.AccountEvent) { got <- ev }); err != nil {
		t.Fatal(err)
	}
	return b, s, got
}

func next(t *testing.T, ch chan provider.AccountEvent) provider.AccountEvent {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("no account state event")
	}
	return provider.AccountEvent{}
}

func none(t *testing.T, ch chan provider.AccountEvent) {
	t.Helper()
	select {
	case ev := <-ch:
		t.Fatalf("unexpected event %+v", ev)
	case <-time.After(300 * time.Millisecond):
	}
}

// A rest on one node reaches the others, a node that starts later loads it, and a
// node never gets its own events back.
func TestAccountSyncSharesRests(t *testing.T) {
	url := natstest.Run(t)
	b1, s1, got1 := node(t, url)
	_, _, got2 := node(t, url)

	until := time.Now().Add(time.Minute).UTC().Truncate(time.Millisecond)
	s1.Publish(provider.AccountEvent{Provider: "anthropic", Account: "max-a", Until: &until, LastError: "429 Too Many Requests", At: time.Now()})
	ev := next(t, got2)
	if ev.Provider != "anthropic" || ev.Account != "max-a" || ev.Until == nil || !ev.Until.Equal(until) ||
		ev.LastError != "429 Too Many Requests" || ev.Node != b1.NodeID {
		t.Fatalf("node 2 got %+v", ev)
	}
	none(t, got1)

	_, _, got3 := node(t, url) // starts after the rest
	if ev := next(t, got3); ev.Account != "max-a" || ev.Until == nil {
		t.Fatalf("late node got %+v", ev)
	}

	s1.Publish(provider.AccountEvent{Provider: "anthropic", Account: "max-a", At: time.Now()}) // rest ended
	if ev := next(t, got2); ev.Until != nil {
		t.Fatalf("rest should have ended: %+v", ev)
	}
}

// Without JetStream there is no bucket, and the caller keeps per-node state.
func TestAccountSyncNeedsJetStream(t *testing.T) {
	cfg := natstest.Config(t, natstest.RunWithoutJetStream(t))
	off := false
	cfg.Usage.JetStream = &off
	b, err := bus.Connect(context.Background(), cfg, quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(context.Background())
	if _, err := b.AccountSync(context.Background()); err == nil {
		t.Fatal("AccountSync without JetStream should fail")
	}
}

func TestStateKey(t *testing.T) {
	for in, want := range map[[2]string]string{
		{"anthropic", "max-a"}:   "anthropic.max-a",
		{"my_prov-2", "default"}: "my_prov-2.default",
		{"open.ai", "a"}:         "b64_b3Blbi5haQ.a",
	} {
		if got := bus.StateKey(in[0], in[1]); got != want {
			t.Errorf("StateKey(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}
