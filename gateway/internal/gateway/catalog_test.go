package gateway_test

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai-foundation/gateway/internal/catalog"
	"ai-foundation/gateway/internal/provider"
	"ai-foundation/gateway/internal/store"
)

// A catalog reload swaps the snapshot while a request is in flight (spec-0006 section 5.2):
// that request finishes on the provider, upstream name and price it started with, and
// the next request uses the new catalog. Both catalogs come from PostgreSQL.
func TestCatalogSwapDuringRequest(t *testing.T) {
	t.Setenv("AIF_SWAP_TEST_KEY", "upstream-secret") // key reference of the new catalog
	e := newEnv(t)
	secret, _ := e.createKey(`{"name":"swap"}`)
	ctx := context.Background()

	arrived, release := make(chan struct{}), make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		<-release
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"old"}}],"usage":{"prompt_tokens":1000,"completion_tokens":500,"total_tokens":1500}}`)
	}))
	defer slow.Close()

	load := func(c *store.Catalog) {
		t.Helper()
		if _, err := catalog.Replace(ctx, e.st, store.AnyRevision, "test", "", c); err != nil {
			t.Fatal(err)
		}
		s, err := catalog.Load(ctx, e.st, provider.NewHTTPClient())
		if err != nil {
			t.Fatal(err)
		}
		if !e.app.Catalog.Offer(s) {
			t.Fatalf("revision %d not taken over %d", s.Revision, e.app.Catalog.Revision())
		}
	}
	acct := func(ref string) []store.CatalogAccount { return []store.CatalogAccount{{Name: "default", KeyRef: ref}} }
	load(&store.Catalog{
		Providers: []store.CatalogProvider{{Name: "slow", Type: "openai", BaseURL: slow.URL + "/v1", Accounts: acct("")}},
		Models: []store.CatalogModel{{Name: "small", Targets: []store.CatalogTarget{
			{Provider: "slow", Upstream: "old-up", Price: &store.Price{Input: 1, Output: 2}}}}},
	})

	body := `{"model":"small","messages":[{"role":"user","content":"hi"}]}`
	first := make(chan int)
	go func() {
		code, _ := e.do(e.gw.URL, "POST", "/v1/chat/completions", secret, body)
		first <- code
	}()
	<-arrived

	load(&store.Catalog{
		Providers: []store.CatalogProvider{{Name: "fake", Type: "openai", BaseURL: e.upURL + "/v1", Accounts: acct("env:AIF_SWAP_TEST_KEY")}},
		Models: []store.CatalogModel{{Name: "small", Targets: []store.CatalogTarget{
			{Provider: "fake", Upstream: "new-up", Price: &store.Price{Input: 10, Output: 10, CachedInput: 10}}}}},
	})
	if e.app.Catalog.Revision() != 2 {
		t.Fatalf("serving revision %d", e.app.Catalog.Revision())
	}
	if code, out := e.do(e.gw.URL, "POST", "/v1/chat/completions", secret, body); code != 200 {
		t.Fatalf("request on the new catalog: %d %s", code, out)
	}
	close(release)
	if code := <-first; code != 200 {
		t.Fatalf("request in flight during the swap: %d", code)
	}

	byUpstream := map[string]store.UsageRecord{}
	for _, r := range e.usage() {
		byUpstream[r.UpstreamModel] = r
	}
	old, cur := byUpstream["old-up"], byUpstream["new-up"]
	if old.Provider != "slow" || math.Abs(old.CostUSD-0.002) > 1e-12 { // (1000*1 + 500*2) / 1e6
		t.Fatalf("in-flight request lost its snapshot: %+v", old)
	}
	if cur.Provider != "fake" || math.Abs(cur.CostUSD-0.015) > 1e-12 { // (1500*10) / 1e6
		t.Fatalf("new request not on the new catalog: %+v", cur)
	}
	if code, out := e.do(e.admin.URL, "GET", "/admin/v1/models", masterKey, ""); code != 200 || !strings.Contains(out, `"new-up"`) || strings.Contains(out, `"vendor/small-v1"`) {
		t.Fatalf("admin models after swap: %d %s", code, out)
	}
}
