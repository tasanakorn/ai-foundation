package gateway_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"ai-foundation/gateway/internal/app"
	"ai-foundation/gateway/internal/bus"
	"ai-foundation/gateway/internal/config"
	"ai-foundation/gateway/internal/gateway"
	"ai-foundation/gateway/internal/natstest"
	"ai-foundation/gateway/internal/pgtest"
	"ai-foundation/gateway/internal/store"
	"ai-foundation/gateway/internal/store/postgres"
)

// Provider keys are references (spec-0006): the test configs read them from these variables.
func TestMain(m *testing.M) {
	os.Setenv("AIF_TEST_UPSTREAM_KEY", "upstream-secret")
	os.Setenv("AIF_TEST_ANTH_KEY", "anth-secret")
	pgtest.Main(m)
}

const (
	masterKey    = "test-master-key-123456"
	bootEmail    = "boot@example.com"
	bootPassword = "boot-password-123"
)

// env is one gateway node on the real wiring (app.New) with an in-process NATS server
// and its own PostgreSQL database.
type env struct {
	t        *testing.T
	gw       *httptest.Server
	admin    *httptest.Server
	app      *app.App
	st       store.Store
	natsURL  string
	upURL    string
	mut      []func(*config.Config)
	upstream *[]map[string]any // request bodies seen by the fake upstream
}

// fakeUpstream mimics an OpenAI-compatible API.
func fakeUpstream(t *testing.T, seen *[]map[string]any) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer upstream-secret" {
			t.Errorf("upstream got auth %q", r.Header.Get("Authorization"))
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		*seen = append(*seen, body)

		switch {
		case r.URL.Path == "/v1/embeddings":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"object":"list","data":[{"embedding":[0.1]}],"usage":{"prompt_tokens":5,"total_tokens":5}}`)
		case body["model"] == "broken":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"message":"rate limited"}}`)
		case body["stream"] == true:
			w.Header().Set("Content-Type", "text/event-stream")
			for _, c := range []string{
				`{"choices":[{"delta":{"content":"Hel"}}]}`,
				`{"choices":[{"delta":{"content":"lo"}}]}`,
				`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`,
			} {
				io.WriteString(w, "data: "+c+"\n\n")
				w.(http.Flusher).Flush()
			}
			io.WriteString(w, "data: [DONE]\n\n")
		case body["model"] == "no-usage":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"choices":[{"message":{"content":"12345678"}}]}`)
		default:
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":"hi"}}],
				"usage":{"prompt_tokens":1000,"completion_tokens":500,"total_tokens":1500,
				"prompt_tokens_details":{"cached_tokens":200}}}`)
		}
	}))
}

func newEnv(t *testing.T, mut ...func(*config.Config)) *env {
	t.Helper()
	seen := &[]map[string]any{}
	up := fakeUpstream(t, seen)
	t.Cleanup(up.Close)
	st, err := postgres.Open(context.Background(), pgtest.URL(t), postgres.Options{MaxConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, natsURL: natstest.Run(t), upURL: up.URL, mut: mut, upstream: seen}
	e.start()
	return e
}

// peer starts a second node that shares the database, NATS and upstream.
func (e *env) peer() *env {
	cp := *e
	cp.start()
	return &cp
}

func (e *env) start() {
	t := e.t
	cfg, err := config.Parse([]byte(`
admin: { master_key: ` + masterKey + `, bootstrap: { email: ` + bootEmail + `, password: ` + bootPassword + `, name: Boot } }
database: { url: "postgres://unused" }
metering: { flush_interval: 10ms }
nats: { url: ` + e.natsURL + ` }
passthrough: true
providers:
  - { name: fake, base_url: ` + e.upURL + `/v1, api_key: ${AIF_TEST_UPSTREAM_KEY} }
models:
  - { name: small, provider: fake, upstream: vendor/small-v1, price: { input: 1, output: 2, cached_input: 0.5 } }
  - { name: broken, provider: fake }
  - { name: no-usage, provider: fake, price: { input: 1, output: 1 } }
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range e.mut {
		m(cfg)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	nb, err := bus.Connect(context.Background(), cfg.NATS, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nb.Close(context.Background()) })
	a, err := app.New(context.Background(), cfg, e.st, nb, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(context.Background()) })
	e.app = a
	e.gw = httptest.NewServer(a.Public)
	t.Cleanup(e.gw.Close)
	e.admin = httptest.NewServer(a.Admin)
	t.Cleanup(e.admin.Close)
}

func (e *env) do(base, method, path, token, body string, hdr ...string) (int, string) {
	e.t.Helper()
	req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (e *env) createKey(body string) (secret, id string) {
	e.t.Helper()
	code, out := e.do(e.admin.URL, "POST", "/admin/v1/keys", masterKey, body)
	if code != http.StatusCreated {
		e.t.Fatalf("create key: %d %s", code, out)
	}
	var k struct{ Secret, ID string }
	_ = json.Unmarshal([]byte(out), &k)
	return k.Secret, k.ID
}

// usage flushes the recorder and returns all records, newest first.
func (e *env) usage() []store.UsageRecord {
	e.t.Helper()
	time.Sleep(50 * time.Millisecond)
	recs, err := e.st.ListUsage(context.Background(), store.UsageFilter{})
	if err != nil {
		e.t.Fatal(err)
	}
	return recs
}

func TestAuth(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if code, _ := e.do(e.gw.URL, "GET", "/v1/models", "", ""); code != 401 {
		t.Fatalf("no key: got %d", code)
	}
	if code, _ := e.do(e.gw.URL, "GET", "/v1/models", "sk-aif-wrong", ""); code != 401 {
		t.Fatalf("bad key: got %d", code)
	}
	if code, _ := e.do(e.admin.URL, "GET", "/admin/v1/keys", "wrong", ""); code != 401 {
		t.Fatalf("bad master key: got %d", code)
	}

	secret, id := e.createKey(`{"name":"a"}`)
	if code, _ := e.do(e.gw.URL, "GET", "/v1/models", secret, ""); code != 200 {
		t.Fatalf("good key: got %d", code)
	}
	if code, _ := e.do(e.admin.URL, "DELETE", "/admin/v1/keys/"+id, masterKey, ""); code != 204 {
		t.Fatalf("revoke: got %d", code)
	}
	if code, _ := e.do(e.gw.URL, "GET", "/v1/models", secret, ""); code != 401 {
		t.Fatalf("revoked key: got %d", code)
	}
}

func TestChatNonStreamMetersAndRewritesModel(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	secret, keyID := e.createKey(`{"tenant_id":"acme","app_id":"bot"}`)

	code, out := e.do(e.gw.URL, "POST", "/v1/chat/completions", secret,
		`{"model":"small","messages":[{"role":"user","content":"hi"}],"user":"u-body","custom_field":42}`,
		gateway.EndUserHeader, "u-header")
	if code != 200 || !strings.Contains(out, `"hi"`) {
		t.Fatalf("got %d %s", code, out)
	}
	sent := (*e.upstream)[0]
	if sent["model"] != "vendor/small-v1" || sent["custom_field"] != float64(42) {
		t.Fatalf("upstream body not rewritten/passed through: %v", sent)
	}

	recs := e.usage()
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	r := recs[0]
	if r.KeyID != keyID || r.TenantID != "acme" || r.AppID != "bot" || r.EndUser != "u-header" {
		t.Fatalf("owner labels wrong: %+v", r)
	}
	if r.PromptTokens != 1000 || r.CompletionTokens != 500 || r.CachedTokens != 200 || r.Estimated {
		t.Fatalf("tokens wrong: %+v", r)
	}
	// (800*1 + 200*0.5 + 500*2) / 1e6
	if want := 0.0019; r.CostUSD < want-1e-12 || r.CostUSD > want+1e-12 {
		t.Fatalf("cost = %v, want %v", r.CostUSD, want)
	}
}

func TestChatStreamInjectsIncludeUsage(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	secret, _ := e.createKey(`{}`)
	code, out := e.do(e.gw.URL, "POST", "/v1/chat/completions", secret, `{"model":"small","stream":true,"messages":[]}`)
	if code != 200 || !strings.Contains(out, `"Hel"`) || !strings.Contains(out, "[DONE]") {
		t.Fatalf("got %d %s", code, out)
	}
	so, _ := (*e.upstream)[0]["stream_options"].(map[string]any)
	if so["include_usage"] != true {
		t.Fatalf("include_usage not injected: %v", (*e.upstream)[0])
	}
	r := e.usage()[0]
	if !r.Stream || r.PromptTokens != 10 || r.CompletionTokens != 2 || r.TotalTokens != 12 || r.Estimated {
		t.Fatalf("stream usage wrong: %+v", r)
	}
}

func TestEstimateWhenUsageMissing(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	secret, _ := e.createKey(`{}`)
	if code, _ := e.do(e.gw.URL, "POST", "/v1/chat/completions", secret, `{"model":"no-usage"}`); code != 200 {
		t.Fatalf("got %d", code)
	}
	r := e.usage()[0]
	if !r.Estimated || r.CompletionTokens == 0 || r.PromptTokens == 0 {
		t.Fatalf("expected estimate: %+v", r)
	}
}

func TestUpstreamErrorPassesThroughAndIsRecorded(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	secret, _ := e.createKey(`{}`)
	code, out := e.do(e.gw.URL, "POST", "/v1/chat/completions", secret, `{"model":"broken"}`)
	if code != 429 || !strings.Contains(out, "rate limited") {
		t.Fatalf("got %d %s", code, out)
	}
	r := e.usage()[0]
	if r.StatusCode != 429 || !strings.Contains(r.Error, "rate limited") {
		t.Fatalf("error not recorded: %+v", r)
	}
}

func TestModelRoutingAndAllowList(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	secret, _ := e.createKey(`{"allowed_models":["small","fake/vendor/*"]}`)

	if code, _ := e.do(e.gw.URL, "POST", "/v1/chat/completions", secret, `{"model":"nope"}`); code != 404 {
		t.Fatalf("unknown model: got %d", code)
	}
	if code, _ := e.do(e.gw.URL, "POST", "/v1/chat/completions", secret, `{"model":"no-usage"}`); code != 403 {
		t.Fatalf("not allowed: got %d", code)
	}
	if code, _ := e.do(e.gw.URL, "POST", "/v1/chat/completions", secret, `{"model":"fake/vendor/big"}`); code != 200 {
		t.Fatalf("passthrough: got %d", code)
	}
	if got := (*e.upstream)[0]["model"]; got != "vendor/big" {
		t.Fatalf("passthrough upstream model = %v", got)
	}
	_, out := e.do(e.gw.URL, "GET", "/v1/models", secret, "")
	if !strings.Contains(out, `"small"`) || strings.Contains(out, `"no-usage"`) {
		t.Fatalf("models not filtered: %s", out)
	}
}

func TestEmbeddingsAndSummary(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	secret, _ := e.createKey(`{"tenant_id":"acme"}`)
	for range 3 {
		if code, _ := e.do(e.gw.URL, "POST", "/v1/embeddings", secret, `{"model":"small","input":"x"}`); code != 200 {
			t.Fatalf("got %d", code)
		}
	}
	e.usage()
	code, out := e.do(e.admin.URL, "GET", "/admin/v1/usage/summary?group_by=tenant", masterKey, "")
	if code != 200 {
		t.Fatalf("summary: %d %s", code, out)
	}
	var s struct{ Data []store.UsageSummary }
	_ = json.Unmarshal([]byte(out), &s)
	if len(s.Data) != 1 || s.Data[0].Group != "acme" || s.Data[0].Requests != 3 || s.Data[0].TotalTokens != 15 {
		t.Fatalf("summary wrong: %s", out)
	}
}
