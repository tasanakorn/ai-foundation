package gateway_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-foundation/gateway/internal/admin"
	"ai-foundation/gateway/internal/config"
)

// loginIP signs in with an X-Forwarded-For header and returns the IP stored on the session.
func loginIP(t *testing.T, e *env, xff string) string {
	t.Helper()
	req, _ := http.NewRequest("POST", e.admin.URL+"/admin/v1/auth/login",
		strings.NewReader(`{"email":"`+bootEmail+`","password":"`+bootPassword+`"}`))
	req.Header.Set("Origin", e.admin.URL)
	req.Header.Set("X-Forwarded-For", xff)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for _, c := range resp.Cookies() {
		if c.Name == admin.SessionCookie {
			sum := sha256.Sum256([]byte(c.Value))
			s, err := e.st.GetAdminSession(context.Background(), hex.EncodeToString(sum[:]))
			if err != nil {
				t.Fatal(err)
			}
			return s.IP
		}
	}
	t.Fatalf("login: %d, no session cookie", resp.StatusCode)
	return ""
}

// spec-0003 D21: the header counts only when the TCP peer is a trusted proxy.
func TestAdminClientIPBehindTrustedProxy(t *testing.T) {
	t.Parallel()
	plain := newEnv(t)
	if ip := loginIP(t, plain, "198.51.100.4"); ip != "127.0.0.1" {
		t.Fatalf("no trusted proxies: session IP %s, want the TCP peer", ip)
	}
	proxied := newEnv(t, func(c *config.Config) { c.Server.TrustedProxies = []string{"127.0.0.0/8"} })
	if ip := loginIP(t, proxied, "6.6.6.6, 198.51.100.4"); ip != "198.51.100.4" {
		t.Fatalf("trusted proxy: session IP %s, want 198.51.100.4", ip)
	}
}

// spec-0003 D22: a metered request moves the counters, and the stream still works through the wrapper.
func TestMetrics(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	secret, _ := e.createKey(`{"tenant_id":"acme","app_id":"chat"}`)
	if code, out := e.do(e.gw.URL, "POST", "/v1/chat/completions", secret, `{"model":"small","messages":[]}`); code != 200 {
		t.Fatalf("chat: %d %s", code, out)
	}
	if code, out := e.do(e.gw.URL, "POST", "/v1/chat/completions", secret, `{"model":"small","stream":true,"messages":[]}`); code != 200 || !strings.Contains(out, "[DONE]") {
		t.Fatalf("stream: %d %s", code, out)
	}
	e.do(e.gw.URL, "GET", "/v1/models", "wrong-key", "")

	srv := httptest.NewServer(e.app.Metrics.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	out := string(b)
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Errorf("content type %q", resp.Header.Get("Content-Type"))
	}
	for _, want := range []string{
		`aif_requests_total{format="openai",endpoint="/chat/completions",model="small",status="200"} 2`,
		`aif_http_responses_total{listener="public",code="401"} 1`,
		`aif_http_responses_total{listener="public",code="200"} 2`,
		`aif_request_duration_seconds_count{format="openai",model="small"} 2`,
		`aif_tokens_total{format="openai",model="small",kind="prompt"}`,
		`aif_cost_usd_total{format="openai",model="small"}`,
		"aif_nats_up 1", "aif_database_up 1", "aif_usage_dropped_total 0",
		`aif_build_info{version="dev"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
	if t.Failed() {
		t.Log(out)
	}
}
