package gateway_test

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ai-foundation/gateway/internal/config"
	"ai-foundation/gateway/internal/hook"
	"ai-foundation/gateway/internal/store"
)

// anthSeen is one request as the fake Anthropic upstream received it.
type anthSeen struct {
	path, query string
	header      http.Header
	body        string
}

type fakeAnthropic struct {
	srv     *httptest.Server
	mu      sync.Mutex
	seen    []anthSeen
	release chan struct{} // the stream waits on it after its first event
	waited  atomic.Bool   // the stream gave up waiting: the gateway buffered
}

func (f *fakeAnthropic) last(t *testing.T) anthSeen {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.seen) == 0 {
		t.Fatal("upstream saw no request")
	}
	return f.seen[len(f.seen)-1]
}

const (
	anthMessage = `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],` +
		`"usage":{"input_tokens":100,"cache_creation_input_tokens":50,"cache_read_input_tokens":1000,"output_tokens":20}}`
	anthRateLimit = `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`
)

var anthStreamFirst = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_2","usage":{"input_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":200,"output_tokens":1}}}` + "\n\n" +
	"event: ping\ndata: {\"type\": \"ping\"}\n\n"

var anthStreamRest = "event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":30,"output_tokens_details":{"thinking_tokens":12}}}` + "\n\n" +
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

func newFakeAnthropic(t *testing.T) *fakeAnthropic {
	f := &fakeAnthropic{release: make(chan struct{})}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.seen = append(f.seen, anthSeen{r.URL.Path, r.URL.RawQuery, r.Header.Clone(), string(b)})
		f.mu.Unlock()
		var req struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.Unmarshal(b, &req)
		w.Header().Set("Request-Id", "req_upstream_1")
		switch {
		case r.URL.Path == "/v1/messages/count_tokens":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"input_tokens":42}`)
		case req.Model == "claude-rate":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "7")
			w.Header().Set("X-Should-Retry", "true")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, anthRateLimit)
		case req.Stream:
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			io.WriteString(w, anthStreamFirst)
			w.(http.Flusher).Flush()
			select { // the gateway must deliver the first events before the stream ends
			case <-f.release:
			case <-time.After(3 * time.Second):
				f.waited.Store(true)
			}
			io.WriteString(w, anthStreamRest)
		default:
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, anthMessage)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// withAnthropic adds provider "claude" (type anthropic) and three aliases.
func withAnthropic(f *fakeAnthropic) func(*config.Config) {
	return func(c *config.Config) {
		c.Providers = append(c.Providers, config.Provider{Name: "claude", Type: config.TypeAnthropic,
			BaseURL: f.srv.URL + "/v1", APIKey: "anth-secret", OpenAIFormat: config.OpenAIFormatCompat})
		c.Models = append(c.Models,
			config.Model{Name: "sonnet", Provider: "claude", Upstream: "claude-up-1", Price: &config.Price{Input: 2, Output: 10, CachedInput: 0.2}},
			config.Model{Name: "claude-rate", Provider: "claude", Upstream: "claude-rate"},
			config.Model{Name: "claude-native-only", Provider: "claude", Upstream: "claude-up-1", Formats: []string{config.FormatAnthropic}},
		)
	}
}

// anthDo sends a request the way the Anthropic SDK does: aif key in x-api-key.
func (e *env) anthDo(base, method, path, key, body string, hdr ...string) (*http.Response, string) {
	e.t.Helper()
	req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
	req.Header.Set("X-Api-Key", key)
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func anthErrType(t *testing.T, body string) string {
	t.Helper()
	var e struct {
		Type  string
		Error struct{ Type, Message string }
	}
	if json.Unmarshal([]byte(body), &e) != nil || e.Type != "error" || e.Error.Message == "" {
		t.Fatalf("not an Anthropic error envelope: %s", body)
	}
	return e.Error.Type
}

func TestAnthropicMessagesPassthrough(t *testing.T) {
	t.Parallel()
	f := newFakeAnthropic(t)
	e := newEnv(t, withAnthropic(f))
	secret, keyID := e.createKey(`{"tenant_id":"acme","app_id":"coder"}`)

	// Odd spacing, cache_control and a nested "model" key must survive untouched.
	body := `{ "system":[{"type":"text","text":"x-anthropic-billing-header: a"},{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],` +
		"\n\t\"model\" :  \"sonnet\" ,\"max_tokens\":64,\"messages\":[{\"role\":\"user\",\"content\":[{\"type\":\"text\",\"text\":\"<b>&\",\"model\":\"sonnet\"}]}]}"
	resp, out := e.anthDo(e.gw.URL, "POST", "/anthropic/v1/messages?beta=true", secret, body,
		"Anthropic-Version", "2023-06-01", "Anthropic-Beta", "prompt-caching-2024-07-31,interleaved-thinking-2025-05-14",
		"User-Agent", "claude-cli/9.9.9", "X-Claude-Code-Session-Id", "sess-1",
		"Anthropic-Workspace-Id", "wrkspc_client", "X-Stainless-Os", "MacOS")
	if resp.StatusCode != 200 || out != anthMessage {
		t.Fatalf("got %d %s", resp.StatusCode, out)
	}
	if resp.Header.Get("Request-Id") != "req_upstream_1" || resp.Header.Get("X-Request-Id") == "" {
		t.Fatalf("response headers: %v", resp.Header)
	}

	up := f.last(t)
	want := strings.Replace(body, `"model" :  "sonnet"`, `"model" :  "claude-up-1"`, 1)
	if up.path != "/v1/messages" || up.query != "beta=true" || up.body != want {
		t.Fatalf("upstream got %s?%s\n%s\nwant\n%s", up.path, up.query, up.body, want)
	}
	for h, v := range map[string]string{
		"X-Api-Key": "anth-secret", "Authorization": "", "Anthropic-Version": "2023-06-01",
		"Anthropic-Beta": "prompt-caching-2024-07-31,interleaved-thinking-2025-05-14",
		"User-Agent":     "claude-cli/9.9.9", "X-Claude-Code-Session-Id": "sess-1",
		"Anthropic-Workspace-Id": "", "X-Stainless-Os": "",
	} {
		if got := up.header.Get(h); got != v {
			t.Errorf("upstream header %s = %q, want %q", h, got, v)
		}
	}

	recs := e.usage()
	if len(recs) != 1 {
		t.Fatalf("usage records: %d", len(recs))
	}
	r := recs[0]
	// D20: prompt = 100 + 1000 + 50; cached = 1000; cache write = 50.
	// Cost = (100*2 + 1000*0.2 + 50*2.5 + 20*10) / 1e6.
	if r.Format != "anthropic" || r.Endpoint != "/messages" || r.Model != "sonnet" || r.UpstreamModel != "claude-up-1" ||
		r.KeyID != keyID || r.PromptTokens != 1150 || r.CachedTokens != 1000 || r.CacheWriteTokens != 50 ||
		r.CompletionTokens != 20 || r.TotalTokens != 1170 || r.SessionID != "sess-1" || r.Estimated ||
		!near(r.CostUSD, 725e-6) {
		t.Fatalf("record: %+v", r)
	}
}

func TestAnthropicStreamUnbufferedAndMetered(t *testing.T) {
	t.Parallel()
	f := newFakeAnthropic(t)
	e := newEnv(t, withAnthropic(f))
	secret, _ := e.createKey(`{}`)

	req, _ := http.NewRequest("POST", e.gw.URL+"/anthropic/v1/messages",
		strings.NewReader(`{"model":"sonnet","stream":true,"max_tokens":64,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+secret) // Claude Code's ANTHROPIC_AUTH_TOKEN
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("got %d %v", resp.StatusCode, resp.Header)
	}
	// The first two events must arrive while the upstream is still holding the rest.
	br := bufio.NewReader(resp.Body)
	var got strings.Builder
	for got.Len() < len(anthStreamFirst) {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("stream read: %v (got %q)", err, got.String())
		}
		got.WriteString(line)
	}
	if got.String() != anthStreamFirst {
		t.Fatalf("first events changed:\n%q", got.String())
	}
	close(f.release)
	if f.waited.Load() {
		t.Fatal("first events arrived only after the upstream stream ended: the gateway buffers")
	}
	rest, _ := io.ReadAll(br)
	if string(rest) != anthStreamRest {
		t.Fatalf("rest changed:\n%q", rest)
	}

	r := e.usage()[0]
	if !r.Stream || r.Format != "anthropic" || r.PromptTokens != 210 || r.CachedTokens != 200 ||
		r.CompletionTokens != 30 || r.ReasoningTokens != 12 || r.TotalTokens != 240 || r.Estimated || r.StatusCode != 200 {
		t.Fatalf("record: %+v", r)
	}
}

func TestAnthropicUpstreamErrorPassesThrough(t *testing.T) {
	t.Parallel()
	f := newFakeAnthropic(t)
	e := newEnv(t, withAnthropic(f))
	secret, _ := e.createKey(`{}`)

	resp, out := e.anthDo(e.gw.URL, "POST", "/anthropic/v1/messages", secret, `{"model":"claude-rate","max_tokens":5,"messages":[]}`)
	if resp.StatusCode != 429 || out != anthRateLimit || resp.Header.Get("Retry-After") != "7" ||
		resp.Header.Get("X-Should-Retry") != "true" || resp.Header.Get("Request-Id") != "req_upstream_1" {
		t.Fatalf("got %d %v %s", resp.StatusCode, resp.Header, out)
	}
	if r := e.usage()[0]; r.StatusCode != 429 || !strings.Contains(r.Error, "slow down") || r.Format != "anthropic" {
		t.Fatalf("record: %+v", r)
	}
}

func TestAnthropicFormatsRoutesAndErrors(t *testing.T) {
	t.Parallel()
	f := newFakeAnthropic(t)
	e := newEnv(t, withAnthropic(f))
	secret, _ := e.createKey(`{}`)
	limited, _ := e.createKey(`{"allowed_models":["claude-rate"]}`)

	cases := []struct {
		name, method, path, key, body string
		status                        int
		errType                       string
	}{
		{"no key", "POST", "/anthropic/v1/messages", "", `{"model":"sonnet"}`, 401, "authentication_error"},
		{"openai-only alias", "POST", "/anthropic/v1/messages", secret, `{"model":"small"}`, 404, "not_found_error"},
		{"unknown model", "POST", "/anthropic/v1/messages", secret, `{"model":"nope"}`, 404, "not_found_error"},
		{"no model", "POST", "/anthropic/v1/messages", secret, `{"max_tokens":1}`, 400, "invalid_request_error"},
		{"not json", "POST", "/anthropic/v1/messages", secret, `[1]`, 400, "invalid_request_error"},
		{"allow-list", "POST", "/anthropic/v1/messages", limited, `{"model":"sonnet"}`, 403, "permission_error"},
		{"double v1", "POST", "/anthropic/v1/v1/messages", secret, `{"model":"sonnet"}`, 404, "not_found_error"},
	}
	for _, c := range cases {
		resp, out := e.anthDo(e.gw.URL, c.method, c.path, c.key, c.body)
		if resp.StatusCode != c.status || anthErrType(t, out) != c.errType {
			t.Errorf("%s: got %d %s", c.name, resp.StatusCode, out)
		}
		if c.name == "double v1" && !strings.Contains(out, "must not end in /v1") {
			t.Errorf("double v1: no hint in %s", out)
		}
	}

	// Each format lists only its own models.
	_, out := e.anthDo(e.gw.URL, "GET", "/anthropic/v1/models", secret, "")
	var am struct {
		Data    []struct{ ID, Type string }
		HasMore bool   `json:"has_more"`
		FirstID string `json:"first_id"`
	}
	_ = json.Unmarshal([]byte(out), &am)
	if len(am.Data) != 3 || am.Data[0].ID != "claude-native-only" || am.Data[0].Type != "model" || am.FirstID != "claude-native-only" || am.HasMore {
		t.Fatalf("anthropic models: %s", out)
	}
	for _, pfx := range []string{"/v1", "/openai/v1"} {
		_, out := e.do(e.gw.URL, "GET", pfx+"/models", secret, "")
		if !strings.Contains(out, `"sonnet"`) || !strings.Contains(out, `"small"`) || strings.Contains(out, "claude-native-only") {
			t.Fatalf("%s/models: %s", pfx, out)
		}
	}
	if code, out := e.do(e.gw.URL, "POST", "/v1/chat/completions", secret, `{"model":"claude-native-only"}`); code != 404 {
		t.Fatalf("anthropic-only alias on /v1: %d %s", code, out)
	}
	if code, out := e.do(e.gw.URL, "POST", "/openai/v1/chat/completions", secret, `{"model":"small"}`); code != 200 {
		t.Fatalf("/openai/v1 alias: %d %s", code, out)
	}

	// count_tokens passes through and is logged at zero cost.
	resp, out := e.anthDo(e.gw.URL, "POST", "/anthropic/v1/messages/count_tokens", secret, `{"model":"sonnet","messages":[]}`)
	if resp.StatusCode != 200 || out != `{"input_tokens":42}` || f.last(t).path != "/v1/messages/count_tokens" {
		t.Fatalf("count_tokens: %d %s", resp.StatusCode, out)
	}
	var ct *store.UsageRecord
	for _, r := range e.usage() {
		if r.Endpoint == "/messages/count_tokens" {
			ct = &r
		}
	}
	if ct == nil || ct.CostUSD != 0 || ct.TotalTokens != 0 || ct.Estimated {
		t.Fatalf("count_tokens record: %+v", ct)
	}
}

func TestAnthropicDedicatedListener(t *testing.T) {
	t.Parallel()
	f := newFakeAnthropic(t)
	e := newEnv(t, withAnthropic(f))
	secret, _ := e.createKey(`{}`)
	srv := httptest.NewServer(e.app.Anthropic)
	t.Cleanup(srv.Close)

	if resp, out := e.anthDo(srv.URL, "POST", "/v1/messages", secret, `{"model":"sonnet","max_tokens":5,"messages":[]}`); resp.StatusCode != 200 || out != anthMessage {
		t.Fatalf("messages: %d %s", resp.StatusCode, out)
	}
	if code, _ := e.do(srv.URL, "POST", "/v1/chat/completions", secret, `{"model":"small"}`); code != 404 {
		t.Fatalf("OpenAI path on the Anthropic listener: %d", code)
	}
	if code, _ := e.do(srv.URL, "GET", "/healthz", "", ""); code != 200 {
		t.Fatalf("healthz: %d", code)
	}
}

func TestAnthropicPrefixOff(t *testing.T) {
	t.Parallel()
	f := newFakeAnthropic(t)
	e := newEnv(t, withAnthropic(f), func(c *config.Config) {
		off := ""
		c.Server.Anthropic.PathPrefix = &off
	})
	secret, _ := e.createKey(`{}`)
	if resp, _ := e.anthDo(e.gw.URL, "POST", "/anthropic/v1/messages", secret, `{"model":"sonnet"}`); resp.StatusCode != 404 {
		t.Fatalf("prefix off: %d", resp.StatusCode)
	}
}

func TestAnthropicHookOptIn(t *testing.T) {
	t.Parallel()
	f := newFakeAnthropic(t)

	// Default formats [openai]: fail-closed hook with no extension, yet Anthropic requests pass.
	e := newEnv(t, withAnthropic(f), withHook(false))
	secret, _ := e.createKey(`{}`)
	if resp, out := e.anthDo(e.gw.URL, "POST", "/anthropic/v1/messages", secret, `{"model":"sonnet"}`); resp.StatusCode != 200 {
		t.Fatalf("hook not opted in should skip: %d %s", resp.StatusCode, out)
	}

	// Opted in: the extension sees format anthropic, and a deny uses the Anthropic envelope.
	e2 := newEnv(t, withAnthropic(f), withHook(false), func(c *config.Config) {
		c.NATS.Hooks.PreRequest.Formats = []string{config.FormatOpenAI, config.FormatAnthropic}
	})
	seen := e2.extension(func(r hook.Request) hook.Decision {
		return hook.Decision{Action: hook.Deny, Status: 429, Code: "budget_exceeded", Message: "daily budget reached"}
	})
	secret2, _ := e2.createKey(`{}`)
	resp, out := e2.anthDo(e2.gw.URL, "POST", "/anthropic/v1/messages", secret2, `{"model":"sonnet"}`)
	if resp.StatusCode != 429 || anthErrType(t, out) != "rate_limit_error" || !strings.Contains(out, "daily budget reached") {
		t.Fatalf("deny: %d %s", resp.StatusCode, out)
	}
	if len(*seen) != 1 || (*seen)[0].Format != "anthropic" || (*seen)[0].Endpoint != "/messages" {
		t.Fatalf("hook saw %+v", *seen)
	}
	// OpenAI requests still carry format openai.
	e2.do(e2.gw.URL, "POST", "/v1/chat/completions", secret2, `{"model":"small"}`)
	if len(*seen) != 2 || (*seen)[1].Format != "openai" {
		t.Fatalf("hook saw %+v", *seen)
	}
}

func near(a, b float64) bool { d := a - b; return d < 1e-12 && d > -1e-12 }
