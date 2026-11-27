package gateway_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-foundation/gateway/internal/catalog"
	"ai-foundation/gateway/internal/store"
)

// Model aliases with several targets (spec-0005): fallback across providers, the
// answering target's price, the per-format filter, and no fallback on client errors.
func TestModelTargets(t *testing.T) {
	t.Parallel()
	f := newFakeAnthropic(t)
	e := newEnv(t, withAnthropic(f))
	ctx := context.Background()
	status := http.StatusServiceUnavailable
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, `{"error":{"message":"down"}}`)
	}))
	defer down.Close()

	price := func(in float64) *store.Price { return &store.Price{Input: in, Output: in, CachedInput: in} }
	c, err := catalog.Update(ctx, e.st, store.AnyRevision, "test", "targets", func(c *store.Catalog) error {
		for _, name := range []string{"down", "downc"} {
			c.Providers = append(c.Providers, store.CatalogProvider{Name: name, Type: "openai", BaseURL: down.URL + "/v1",
				Accounts: []store.CatalogAccount{{Name: "default"}}})
		}
		c.Models = append(c.Models,
			store.CatalogModel{Name: "fb", Targets: []store.CatalogTarget{
				{Provider: "down", Upstream: "first-up", Price: price(100)},
				{Provider: "fake", Upstream: "second-up", Price: price(1)},
			}},
			store.CatalogModel{Name: "mix", Targets: []store.CatalogTarget{
				{Provider: "fake", Upstream: "mix-openai"},
				{Provider: "claude", Upstream: "mix-claude"},
			}},
			store.CatalogModel{Name: "capped", MaxAttempts: 1, Targets: []store.CatalogTarget{
				{Provider: "downc", Upstream: "c1"}, {Provider: "fake", Upstream: "c2"},
			}},
		)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.app.Catalogs.Committed(ctx, c.Revision); err != nil {
		t.Fatal(err)
	}
	secret, _ := e.createKey(`{}`)
	chat := func(model string) (int, store.UsageRecord) {
		t.Helper()
		code, _ := e.do(e.gw.URL, "POST", "/v1/chat/completions", secret, `{"model":"`+model+`","messages":[]}`)
		return code, e.usage()[0]
	}

	// 1. The first target is down: the second answers, with its own model name and price.
	code, r := chat("fb")
	if code != 200 || r.Model != "fb" || r.Provider != "fake" || r.UpstreamModel != "second-up" || r.Attempts != 2 {
		t.Fatalf("fallback: %d %+v", code, r)
	}
	if got := (*e.upstream)[len(*e.upstream)-1]["model"]; got != "second-up" {
		t.Fatalf("upstream got model %v", got)
	}
	if r.CostUSD != 0.0015 { // 1500 tokens at $1 per 1M, not the first target's $100
		t.Fatalf("cost %v uses the wrong target's price", r.CostUSD)
	}
	// While "down" rests, the alias skips it: one call.
	if _, r := chat("fb"); r.Attempts != 1 || r.Provider != "fake" {
		t.Fatalf("resting target not skipped: %+v", r)
	}

	// 2. /anthropic uses only Anthropic-type targets; /v1 takes the first (priority).
	if _, r := chat("mix"); r.Provider != "fake" || r.UpstreamModel != "mix-openai" {
		t.Fatalf("mix on /v1: %+v", r)
	}
	resp, out := e.anthDo(e.gw.URL, "POST", "/anthropic/v1/messages", secret, `{"model":"mix","max_tokens":5,"messages":[]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("mix on /anthropic: %d %s", resp.StatusCode, out)
	}
	if r := e.usage()[0]; r.Provider != "claude" || r.UpstreamModel != "mix-claude" || r.Format != "anthropic" {
		t.Fatalf("mix on /anthropic: %+v", r)
	}
	if body := f.last(t).body; !strings.Contains(body, `"model":"mix-claude"`) {
		t.Fatalf("Anthropic target got %s", body)
	}

	// 3. max_attempts on the model caps calls across targets and accounts.
	// "downc" is healthy and fails with a retryable 503; the cap stops the fallback.
	if code, r := chat("capped"); code != 503 || r.Provider != "downc" || r.Attempts != 1 {
		t.Fatalf("capped: %d %+v", code, r)
	}

	// 4. A client error (404) is the same on every target: no fallback.
	status = http.StatusNotFound
	c, err = catalog.Update(ctx, e.st, store.AnyRevision, "test", "fresh", func(c *store.Catalog) error {
		c.Providers = append(c.Providers, store.CatalogProvider{Name: "down2", Type: "openai", BaseURL: down.URL + "/v1",
			Accounts: []store.CatalogAccount{{Name: "default"}}})
		c.Models = append(c.Models, store.CatalogModel{Name: "nf", Targets: []store.CatalogTarget{
			{Provider: "down2", Upstream: "x"}, {Provider: "fake", Upstream: "y"},
		}})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	e.app.Catalogs.Committed(ctx, c.Revision)
	if code, r := chat("nf"); code != 404 || r.Provider != "down2" || r.Attempts != 1 {
		t.Fatalf("client error fell back: %d %+v", code, r)
	}
}
