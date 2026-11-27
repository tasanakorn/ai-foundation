package gateway_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"ai-foundation/gateway/internal/store"
)

func (b *browser) playground(path, key, body string, hdr ...string) (*http.Response, string) {
	b.e.t.Helper()
	method := "POST"
	if body == "" {
		method = "GET"
	}
	req, _ := http.NewRequest(method, b.e.admin.URL+"/admin/v1/playground/"+path, strings.NewReader(body))
	req.Header.Set("Origin", b.origin)
	if key != "" {
		req.Header.Set("X-AIF-Key", key)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := b.c.Do(req)
	if err != nil {
		b.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, string(out)
}

func TestPlayground(t *testing.T) {
	t.Parallel()
	idp := newFakeIDP(t)
	e := newEnv(t, withIDP(idp.provider("sso")))
	secret, keyID := e.createKey(`{"name":"pg","tenant_id":"acme","allowed_models":["small"]}`)

	anon := e.browser()
	if resp, _ := anon.playground("chat/completions", secret, `{"model":"small"}`); resp.StatusCode != 401 {
		t.Fatalf("not signed in: %d", resp.StatusCode)
	}

	b := e.browser()
	b.mustLogin(bootEmail, bootPassword)
	if resp, out := b.playground("chat/completions", "", `{"model":"small"}`); resp.StatusCode != 400 || !strings.Contains(out, "missing_api_key") {
		t.Fatalf("no key: %d %s", resp.StatusCode, out)
	}
	if resp, _ := b.playground("chat/completions", "sk-aif-wrong", `{"model":"small"}`); resp.StatusCode != 401 {
		t.Fatalf("bad key: %d", resp.StatusCode)
	}
	if resp, out := b.playground("chat/completions", secret, `{"model":"broken"}`); resp.StatusCode != 403 {
		t.Fatalf("model not allowed for key: %d %s", resp.StatusCode, out)
	}
	resp, out := b.playground("models", secret, "")
	if resp.StatusCode != 200 || !strings.Contains(out, `"small"`) || strings.Contains(out, `"broken"`) {
		t.Fatalf("models for key: %d %s", resp.StatusCode, out)
	}

	resp, out = b.playground("chat/completions", secret, `{"model":"small","messages":[{"role":"user","content":"hi"}]}`, "X-AIF-End-User", "tester")
	reqID := resp.Header.Get("X-Request-Id")
	if resp.StatusCode != 200 || !strings.Contains(out, `"hi"`) || reqID == "" {
		t.Fatalf("chat: %d %s", resp.StatusCode, out)
	}
	resp, out = b.playground("chat/completions", secret, `{"model":"small","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") ||
		!strings.Contains(out, `"Hel"`) || !strings.Contains(out, `"total_tokens":12`) || !strings.Contains(out, "[DONE]") {
		t.Fatalf("stream: %d %s", resp.StatusCode, out)
	}
	// The admin cookie and the X-AIF-Key header never reach the upstream.
	for _, body := range *e.upstream {
		if _, ok := body["aif_session"]; ok {
			t.Fatal("session leaked")
		}
	}

	// Metered like any client request, and findable by request id.
	// Rejected before routing (bad key, model not allowed) and /models are not metered.
	if recs := e.usage(); len(recs) != 2 {
		t.Fatalf("usage records = %d, want 2 (chat + stream)", len(recs))
	}
	one, err := e.st.ListUsage(context.Background(), store.UsageFilter{ID: reqID})
	if err != nil || len(one) != 1 || one[0].KeyID != keyID || one[0].EndUser != "tester" || one[0].TotalTokens != 1500 {
		t.Fatalf("usage by id: %+v %v", one, err)
	}
	if code, body, _ := b.do("GET", "/admin/v1/usage?id="+reqID, ""); code != 200 || strings.Count(body, `"id"`) != 1 {
		t.Fatalf("admin usage?id: %d %s", code, body)
	}

	// Viewers may use it too (it changes no admin state), but not cross-origin.
	e.createOIDCUser("v@example.com", "viewer")
	idp.as(idpUser{Sub: "v", Email: "v@example.com", Verified: true})
	v := e.browser()
	v.oidcLogin("sso")
	if resp, _ := v.playground("chat/completions", secret, `{"model":"small","messages":[]}`); resp.StatusCode != 200 {
		t.Fatalf("viewer: %d", resp.StatusCode)
	}
	v.origin = "https://evil.example"
	if resp, _ := v.playground("chat/completions", secret, `{"model":"small","messages":[]}`); resp.StatusCode != 403 {
		t.Fatalf("cross-origin: %d", resp.StatusCode)
	}
}
