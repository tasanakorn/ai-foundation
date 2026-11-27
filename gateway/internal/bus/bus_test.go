package bus_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"ai-foundation/gateway/internal/bus"
	"ai-foundation/gateway/internal/hook"
	"ai-foundation/gateway/internal/natstest"
	"ai-foundation/gateway/internal/store"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestToken(t *testing.T) {
	for in, want := range map[string]string{
		"":       "_",
		"acme":   "acme",
		"a_b-C9": "a_b-C9",
		"a.b":    "a%2Eb",
		"x y*>":  "x%20y%2A%3E",
		"100%":   "100%25",
		"é":      "%C3%A9",
	} {
		if got := bus.Token(in); got != want {
			t.Errorf("Token(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConnectFailsFast(t *testing.T) {
	cfg := natstest.Config(t, "nats://127.0.0.1:1")
	cfg.ConnectTimeout = 200 * time.Millisecond
	_, err := bus.Connect(context.Background(), cfg, quiet)
	if err == nil || !strings.Contains(err.Error(), "NATS is required") {
		t.Fatalf("err = %v", err)
	}
}

func TestJetStreamSinkDedupsRetries(t *testing.T) {
	url := natstest.Run(t)
	b, err := bus.Connect(context.Background(), natstest.Config(t, url), quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(context.Background())
	recs := []store.UsageRecord{{ID: "req_1", TenantID: "t"}, {ID: "req_2", TenantID: "t", AppID: "a"}}
	sink := b.UsageSink()
	for range 2 { // second write = a retry of the same batch
		if err := sink.Write(context.Background(), recs); err != nil {
			t.Fatal(err)
		}
	}
	js, _ := jetstream.New(b.Conn())
	st, err := js.Stream(context.Background(), "AIF_USAGE")
	if err != nil {
		t.Fatal(err)
	}
	info, _ := st.Info(context.Background())
	if info.State.Msgs != 2 {
		t.Fatalf("stream has %d messages, want 2 (dedup by request ID)", info.State.Msgs)
	}
}

func TestCoreSinkWithoutJetStream(t *testing.T) {
	url := natstest.Run(t)
	cfg := natstest.Config(t, url)
	off := false
	cfg.Usage.JetStream = &off
	b, err := bus.Connect(context.Background(), cfg, quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(context.Background())

	sub, _ := b.Conn().SubscribeSync("aif.usage.v1.>")
	_ = b.Conn().Flush()
	if err := b.UsageSink().Write(context.Background(), []store.UsageRecord{{ID: "req_1"}}); err != nil {
		t.Fatal(err)
	}
	m, err := sub.NextMsg(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var r store.UsageRecord
	_ = json.Unmarshal(m.Data, &r)
	if m.Subject != "aif.usage.v1._._" || r.ID != "req_1" {
		t.Fatalf("got %s %+v", m.Subject, r)
	}
}

func TestPreRequestHookOmitsBigBody(t *testing.T) {
	url := natstest.Run(t)
	b, err := bus.Connect(context.Background(), natstest.Config(t, url), quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(context.Background())

	got := make(chan map[string]any, 1)
	_, _ = b.Conn().Subscribe("hook.test", func(m *nats.Msg) {
		var req map[string]any
		_ = json.Unmarshal(m.Data, &req)
		got <- req
		_ = m.Respond([]byte(`{"action":"modify","body":{"model":"x"}}`))
	})
	big := `"` + strings.Repeat("x", int(b.Conn().MaxPayload())) + `"`
	h := b.PreRequestHook("hook.test")
	_, err = h.PreRequest(context.Background(), &hook.Request{Body: json.RawMessage(big)})
	if err == nil || !strings.Contains(err.Error(), "did not receive") {
		t.Fatalf("modify of omitted body should fail, err = %v", err)
	}
	req := <-got
	if req["body_omitted"] != true || req["body"] != nil {
		t.Fatalf("hook got body_omitted=%v body=%v", req["body_omitted"], req["body"] != nil)
	}
}

// catalog_changed is a public contract (spec-0001 section 8.1): pin its subject and fields.
func TestCatalogChangedEvent(t *testing.T) {
	url := natstest.Run(t)
	b, err := bus.Connect(context.Background(), natstest.Config(t, url), quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(context.Background())
	raw, err := b.Conn().SubscribeSync("aif.control.v1.catalog_changed")
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan int64, 1)
	if err := b.OnCatalogChanged(func(rev int64) { got <- rev }); err != nil {
		t.Fatal(err)
	}
	if err := b.PublishCatalogChanged(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	if rev := <-got; rev != 7 {
		t.Fatalf("handler got revision %d", rev)
	}
	m, err := raw.NextMsg(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var ev map[string]any
	_ = json.Unmarshal(m.Data, &ev)
	if ev["type"] != "catalog_changed" || ev["revision"] != float64(7) || ev["node"] != b.NodeID || ev["at"] == nil || len(ev) != 4 {
		t.Fatalf("payload: %s", m.Data)
	}
}
