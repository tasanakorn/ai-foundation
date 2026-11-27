package gateway_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-foundation/gateway/internal/catalog"
	"ai-foundation/gateway/internal/store"
)

// Several accounts on one provider (spec-0004): a rate-limited account fails over to
// the next one before the client sees anything; usage, metrics and the providers API
// show the account; the cooldown survives a catalog reload (spec-0006 section 5.6).
func TestAccountsFailover(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"message":"rate limited"}}`)
	}))
	defer limited.Close()

	apply := func(summary string, edit func(*store.Catalog)) {
		t.Helper()
		c, err := catalog.Update(ctx, e.st, store.AnyRevision, "test", summary, func(c *store.Catalog) error {
			edit(c)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.app.Catalogs.Committed(ctx, c.Revision); err != nil {
			t.Fatal(err)
		}
	}
	apply("two accounts", func(c *store.Catalog) {
		c.Providers = append(c.Providers, store.CatalogProvider{Name: "multi", Type: "openai", BaseURL: e.upURL + "/v1",
			Accounts: []store.CatalogAccount{
				{Name: "plan-a", KeyRef: "env:AIF_TEST_UPSTREAM_KEY", BaseURL: limited.URL + "/v1"},
				{Name: "plan-b", KeyRef: "env:AIF_TEST_UPSTREAM_KEY"},
			}})
		c.Models = append(c.Models, store.CatalogModel{Name: "multi-model", Targets: []store.CatalogTarget{{Provider: "multi", Upstream: "vendor/m"}}})
	})

	secret, _ := e.createKey(`{}`)
	body := `{"model":"multi-model","messages":[{"role":"user","content":"hi"}]}`
	if code, out := e.do(e.gw.URL, "POST", "/v1/chat/completions", secret, body); code != 200 {
		t.Fatalf("failover: %d %s", code, out)
	}
	recs := e.usage()
	if r := recs[0]; r.Provider != "multi" || r.Account != "plan-b" || r.Attempts != 2 {
		t.Fatalf("usage record: provider %q account %q attempts %d", r.Provider, r.Account, r.Attempts)
	}
	// The usage API filters by account.
	if code, out := e.do(e.admin.URL, "GET", "/admin/v1/usage?account=plan-b", masterKey, ""); code != 200 || !strings.Contains(out, `"attempts":2`) {
		t.Fatalf("usage by account: %d %s", code, out)
	}

	providers := func() map[string]any {
		t.Helper()
		code, out := e.do(e.admin.URL, "GET", "/admin/v1/providers", masterKey, "")
		if code != 200 || strings.Contains(out, "upstream-secret") {
			t.Fatalf("providers: %d %s", code, out)
		}
		var resp struct {
			Node string
			Data []struct {
				Name     string
				Accounts []map[string]any
			}
		}
		_ = json.Unmarshal([]byte(out), &resp)
		for _, p := range resp.Data {
			if p.Name == "multi" {
				return p.Accounts[0]
			}
		}
		t.Fatalf("no provider multi: %s", out)
		return nil
	}
	if a := providers(); a["name"] != "plan-a" || a["state"] != "cooldown" || !strings.HasPrefix(a["last_error"].(string), "429") {
		t.Fatalf("plan-a: %v", a)
	}

	// A reload (here: a model added) keeps plan-a resting.
	apply("one more model", func(c *store.Catalog) {
		c.Models = append(c.Models, store.CatalogModel{Name: "other", Targets: []store.CatalogTarget{{Provider: "multi", Upstream: "x"}}})
	})
	if a := providers(); a["state"] != "cooldown" {
		t.Fatalf("cooldown lost on reload: %v", a)
	}
	if code, _ := e.do(e.gw.URL, "POST", "/v1/chat/completions", secret, body); code != 200 || e.usage()[0].Attempts != 1 {
		t.Fatal("resting account tried again after the reload")
	}

	var buf strings.Builder
	e.app.Metrics.Registry().Write(&buf)
	for _, line := range []string{
		`aif_upstream_attempts_total{provider="multi",account="plan-a",outcome="rate_limited"} 1`,
		`aif_upstream_attempts_total{provider="multi",account="plan-b",outcome="ok"} 2`,
		`aif_account_cooldown{provider="multi",account="plan-a"} 1`,
		`aif_account_cooldown{provider="multi",account="plan-b"} 0`,
	} {
		if !strings.Contains(buf.String(), line+"\n") {
			t.Errorf("metrics miss %s", line)
		}
	}
}
