package gateway_test

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ai-foundation/gateway/internal/catalog"
	"ai-foundation/gateway/internal/config"
	"ai-foundation/gateway/internal/provider"
	"ai-foundation/gateway/internal/store"
)

// A catalog reload swaps the snapshot while a request is in flight (spec-0006 section 5.2):
// that request finishes on the provider, upstream name and price it started with, and
// the next request uses the new catalog. Both catalogs come from PostgreSQL.
func TestCatalogSwapDuringRequest(t *testing.T) {
	t.Parallel()
	e := newEnv(t) // revision 1: seeded from the test config
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
		Providers: []store.CatalogProvider{{Name: "fake", Type: "openai", BaseURL: e.upURL + "/v1", Accounts: acct("env:AIF_TEST_UPSTREAM_KEY")}},
		Models: []store.CatalogModel{{Name: "small", Targets: []store.CatalogTarget{
			{Provider: "fake", Upstream: "new-up", Price: &store.Price{Input: 10, Output: 10, CachedInput: 10}}}}},
	})
	if e.app.Catalog.Revision() != 3 {
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

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func addModel(name string) func(*store.Catalog) error {
	return func(c *store.Catalog) error {
		c.Models = append(c.Models, store.CatalogModel{Name: name, Targets: []store.CatalogTarget{{Provider: "fake", Upstream: "vendor/" + name}}})
		return nil
	}
}

// A change committed on one node reaches the others through catalog_changed (spec-0006 section 5.3).
func TestCatalogEventReachesPeer(t *testing.T) {
	t.Parallel()
	a := newEnv(t)
	b := a.peer()
	ctx := context.Background()
	c, err := catalog.Update(ctx, a.st, 1, "admin", "add model", addModel("added"))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := a.app.Catalogs.Committed(ctx, c.Revision); err != nil {
		t.Fatal(err)
	}
	if a.app.Catalog.Revision() != 2 {
		t.Fatalf("writer serves revision %d", a.app.Catalog.Revision())
	}
	eventually(t, "the peer on revision 2", func() bool { return b.app.Catalog.Revision() == 2 })
	t.Logf("peer reloaded %v after the commit", time.Since(start).Round(time.Millisecond))

	secret, _ := b.createKey(`{}`)
	if code, out := b.do(b.gw.URL, "POST", "/v1/chat/completions", secret, `{"model":"added","messages":[]}`); code != 200 {
		t.Fatalf("new model on the peer: %d %s", code, out)
	}
}

// Without an event (lost, or a writer that did not publish), the revision poll catches up.
func TestCatalogPollWithoutEvent(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(c *config.Config) { c.Catalog.PollInterval = 100 * time.Millisecond })
	if _, err := catalog.Update(context.Background(), e.st, 1, "admin", "no event", addModel("quiet")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "revision 2 by poll", func() bool { return e.app.Catalog.Revision() == 2 })
}

// A reload that fails keeps the last good snapshot and counts the error (spec-0006 section 5.5).
func TestCatalogFailedReloadKeepsLastGood(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	// Valid catalog, but the key reference does not resolve on this node.
	c, err := catalog.Update(ctx, e.st, 1, "admin", "bad key", func(c *store.Catalog) error {
		c.Providers[0].Accounts[0].KeyRef = "env:AIF_TEST_KEY_NOT_SET"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.app.Catalogs.Committed(ctx, c.Revision); err == nil {
		t.Fatal("reload with an unresolved key reported success")
	}
	if e.app.Catalog.Revision() != 1 {
		t.Fatalf("serving revision %d after a failed reload", e.app.Catalog.Revision())
	}
	secret, _ := e.createKey(`{}`)
	if code, out := e.do(e.gw.URL, "POST", "/v1/chat/completions", secret, `{"model":"small","messages":[]}`); code != 200 {
		t.Fatalf("last good catalog not served: %d %s", code, out)
	}

	// Fixed: the next reload succeeds.
	c, err = catalog.Update(ctx, e.st, c.Revision, "admin", "fix key", func(c *store.Catalog) error {
		c.Providers[0].Accounts[0].KeyRef = "env:AIF_TEST_UPSTREAM_KEY"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.app.Catalogs.Committed(ctx, c.Revision); err != nil || e.app.Catalog.Revision() != 3 {
		t.Fatalf("reload after the fix: rev %d, %v", e.app.Catalog.Revision(), err)
	}
	var buf strings.Builder
	e.app.Metrics.Registry().Write(&buf)
	// error is 1 or 2: this node may also retry on its own catalog_changed event.
	for _, line := range []string{`aif_catalog_reloads_total{result="error"} `, `aif_catalog_reloads_total{result="ok"} 1` + "\n", "aif_catalog_revision 3\n"} {
		if !strings.Contains(buf.String(), line) {
			t.Errorf("metrics miss %q", line)
		}
	}
}
