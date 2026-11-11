package gateway_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"ai-foundation/gateway/internal/config"
	"ai-foundation/gateway/internal/hook"
	"ai-foundation/gateway/internal/store"
)

const hookSubject = "aif.hook.pre_request.v1"

func withHook(failOpen bool, models ...string) func(*config.Config) {
	return func(c *config.Config) {
		c.NATS.Hooks.PreRequest = config.Hook{Subject: hookSubject, Timeout: 300 * time.Millisecond, FailOpen: failOpen, Models: models}
	}
}

// extension connects to NATS like an external extension would: own connection, queue group.
func (e *env) extension(decide func(hook.Request) hook.Decision) *[]hook.Request {
	e.t.Helper()
	nc, err := nats.Connect(e.natsURL)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(nc.Close)
	var mu sync.Mutex
	seen := &[]hook.Request{}
	_, err = nc.QueueSubscribe(hookSubject, "test-ext", func(m *nats.Msg) {
		var req hook.Request
		_ = json.Unmarshal(m.Data, &req)
		mu.Lock()
		*seen = append(*seen, req)
		mu.Unlock()
		out, _ := json.Marshal(decide(req))
		_ = m.Respond(out)
	})
	if err != nil {
		e.t.Fatal(err)
	}
	_ = nc.Flush()
	return seen
}

func TestUsagePublishedToJetStream(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	secret, keyID := e.createKey(`{"tenant_id":"acme","app_id":"chat.bot"}`)
	for range 2 {
		if code, out := e.do(e.gw.URL, "POST", "/v1/chat/completions", secret, `{"model":"small"}`); code != 200 {
			t.Fatalf("got %d %s", code, out)
		}
	}
	js, _ := jetstream.New(e.app.Bus.Conn())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cons, err := js.OrderedConsumer(ctx, "AIF_USAGE", jetstream.OrderedConsumerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		msg, err := cons.Next(jetstream.FetchMaxWait(3 * time.Second))
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		// "." in a label is escaped so tenant and app stay one subject token each.
		if msg.Subject() != "aif.usage.v1.acme.chat%2Ebot" {
			t.Fatalf("subject = %q", msg.Subject())
		}
		var r store.UsageRecord
		if err := json.Unmarshal(msg.Data(), &r); err != nil {
			t.Fatal(err)
		}
		if r.KeyID != keyID || r.AppID != "chat.bot" || r.TotalTokens != 1500 || msg.Headers().Get("Nats-Msg-Id") != r.ID {
			t.Fatalf("record wrong: %+v headers=%v", r, msg.Headers())
		}
	}
}

func TestPreRequestHookAllowDenyModify(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withHook(false))
	seen := e.extension(func(r hook.Request) hook.Decision {
		switch {
		case strings.Contains(string(r.Body), "please deny"):
			return hook.Decision{Action: hook.Deny, Status: 429, Code: "budget_exceeded", Message: "daily limit reached"}
		case strings.Contains(string(r.Body), "please modify"):
			return hook.Decision{Action: hook.Modify, Body: json.RawMessage(`{"model":"fake/vendor/rewritten","note":"by hook"}`)}
		}
		return hook.Decision{Action: hook.Allow}
	})
	secret, keyID := e.createKey(`{"tenant_id":"acme","app_id":"bot","metadata":{"plan":"free"}}`)

	// allow
	if code, _ := e.do(e.gw.URL, "POST", "/v1/chat/completions", secret, `{"model":"small","user":"u1"}`); code != 200 {
		t.Fatalf("allow: got %d", code)
	}
	r := (*seen)[0]
	if r.RequestID == "" || r.Key.ID != keyID || r.Key.TenantID != "acme" || r.Key.Metadata["plan"] != "free" ||
		r.EndUser != "u1" || r.Model != "small" || r.UpstreamModel != "vendor/small-v1" || r.Endpoint != "/chat/completions" ||
		!strings.Contains(string(r.Body), `"user":"u1"`) {
		t.Fatalf("hook request wrong: %+v", r)
	}

	// deny: the extension's status and message reach the client, no upstream call
	code, out := e.do(e.gw.URL, "POST", "/v1/chat/completions", secret, `{"model":"small","messages":"please deny"}`)
	if code != 429 || !strings.Contains(out, "budget_exceeded") || !strings.Contains(out, "daily limit reached") {
		t.Fatalf("deny: got %d %s", code, out)
	}
	if len(*e.upstream) != 1 {
		t.Fatalf("denied request reached upstream")
	}

	// modify: the replacement body is routed again
	if code, out := e.do(e.gw.URL, "POST", "/v1/chat/completions", secret, `{"model":"small","messages":"please modify"}`); code != 200 {
		t.Fatalf("modify: got %d %s", code, out)
	}
	sent := (*e.upstream)[1]
	if sent["model"] != "vendor/rewritten" || sent["note"] != "by hook" {
		t.Fatalf("upstream did not get modified body: %v", sent)
	}

	recs := e.usage() // newest first
	if len(recs) != 3 || recs[0].Model != "fake/vendor/rewritten" || recs[1].StatusCode != 429 ||
		!strings.Contains(recs[1].Error, "budget_exceeded") {
		t.Fatalf("usage wrong: %+v", recs)
	}
}

func TestPreRequestHookModifyStillChecksAllowList(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withHook(false))
	e.extension(func(hook.Request) hook.Decision {
		return hook.Decision{Action: hook.Modify, Body: json.RawMessage(`{"model":"no-usage"}`)}
	})
	secret, _ := e.createKey(`{"allowed_models":["small"]}`)
	if code, _ := e.do(e.gw.URL, "POST", "/v1/chat/completions", secret, `{"model":"small"}`); code != 403 {
		t.Fatalf("got %d, want 403", code)
	}
}

func TestPreRequestHookFailClosedAndOpen(t *testing.T) {
	t.Parallel()
	closed := newEnv(t, withHook(false))
	secret, _ := closed.createKey(`{}`)
	code, out := closed.do(closed.gw.URL, "POST", "/v1/chat/completions", secret, `{"model":"small"}`)
	if code != http.StatusServiceUnavailable || !strings.Contains(out, "hook_unavailable") {
		t.Fatalf("fail closed: got %d %s", code, out)
	}
	if r := closed.usage(); len(r) != 1 || r[0].StatusCode != 503 {
		t.Fatalf("fail closed not recorded: %+v", r)
	}

	open := newEnv(t, withHook(true))
	secret, _ = open.createKey(`{}`)
	if code, _ := open.do(open.gw.URL, "POST", "/v1/chat/completions", secret, `{"model":"small"}`); code != 200 {
		t.Fatalf("fail open: got %d", code)
	}

	// a slow extension counts as no answer
	slow := newEnv(t, withHook(false))
	slow.extension(func(hook.Request) hook.Decision {
		time.Sleep(time.Second)
		return hook.Decision{Action: hook.Allow}
	})
	secret, _ = slow.createKey(`{}`)
	if code, _ := slow.do(slow.gw.URL, "POST", "/v1/chat/completions", secret, `{"model":"small"}`); code != 503 {
		t.Fatalf("timeout: got %d", code)
	}
}

func TestPreRequestHookModelFilter(t *testing.T) {
	t.Parallel()
	// No extension runs and the hook fails closed, but "small" is outside the filter.
	e := newEnv(t, withHook(false, "fake/*"))
	secret, _ := e.createKey(`{}`)
	if code, _ := e.do(e.gw.URL, "POST", "/v1/chat/completions", secret, `{"model":"small"}`); code != 200 {
		t.Fatalf("filtered model: got %d", code)
	}
	if code, _ := e.do(e.gw.URL, "POST", "/v1/chat/completions", secret, `{"model":"fake/vendor/x"}`); code != 503 {
		t.Fatalf("matched model: got %d", code)
	}
}

func TestKeyRevokeReachesAllNodes(t *testing.T) {
	t.Parallel()
	a := newEnv(t)
	b := a.peer() // same database and NATS, separate auth cache
	secret, id := a.createKey(`{}`)

	// Warm node b's cache. Without the control event it would trust the key for 30 s.
	if code, _ := b.do(b.gw.URL, "GET", "/v1/models", secret, ""); code != 200 {
		t.Fatalf("node b before revoke: %d", code)
	}
	if code, _ := a.do(a.admin.URL, "DELETE", "/admin/v1/keys/"+id, masterKey, ""); code != 204 {
		t.Fatalf("revoke on node a: %d", code)
	}
	deadline := time.Now().Add(time.Second)
	for {
		code, _ := b.do(b.gw.URL, "GET", "/v1/models", secret, "")
		if code == 401 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("node b still accepts the revoked key after 1s (status %d)", code)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestHealthzReportsNATS(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if code, _ := e.do(e.gw.URL, "GET", "/healthz", "", ""); code != 200 {
		t.Fatalf("healthz: %d", code)
	}
	e.app.Bus.Conn().Close()
	if code, out := e.do(e.gw.URL, "GET", "/healthz", "", ""); code != 503 || !strings.Contains(out, "nats") {
		t.Fatalf("healthz without nats: %d %s", code, out)
	}
}
