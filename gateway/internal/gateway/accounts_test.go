package gateway_test

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

// Credit per account (task-0010): the providers API shows the spend of the cycle
// (monthly) and the balance of the ledger (prepaid); top-ups go through the admin
// API; an account out of credits gives OpenAI-format clients provider_credit_exhausted.
func TestAccountCredit(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	broke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		io.WriteString(w, `{"error":{"message":"no credits"}}`)
	}))
	defer broke.Close()

	c, err := catalog.Update(ctx, e.st, store.AnyRevision, "test", "credit", func(c *store.Catalog) error {
		c.Providers = append(c.Providers,
			store.CatalogProvider{Name: "credited", Type: "openai", BaseURL: e.upURL + "/v1", Accounts: []store.CatalogAccount{
				{Name: "plan-a", KeyRef: "env:AIF_TEST_UPSTREAM_KEY", Credit: &store.Credit{Type: "monthly", USD: 100, CycleDay: 1, Timezone: "Asia/Bangkok"}},
				{Name: "plan-b", KeyRef: "env:AIF_TEST_UPSTREAM_KEY"},
			}},
			store.CatalogProvider{Name: "pre", Type: "openai", BaseURL: e.upURL + "/v1", Accounts: []store.CatalogAccount{
				{Name: "wallet", KeyRef: "env:AIF_TEST_UPSTREAM_KEY", Credit: &store.Credit{Type: "prepaid"}},
			}},
			store.CatalogProvider{Name: "broke", Type: "openai", BaseURL: broke.URL + "/v1", Accounts: []store.CatalogAccount{
				{Name: "only", KeyRef: "env:AIF_TEST_UPSTREAM_KEY"},
			}})
		price := &store.Price{Input: 1000, Output: 10000}
		c.Models = append(c.Models,
			store.CatalogModel{Name: "credited-model", Targets: []store.CatalogTarget{{Provider: "credited", Upstream: "vendor/m", Price: price}}},
			store.CatalogModel{Name: "pre-model", Targets: []store.CatalogTarget{{Provider: "pre", Upstream: "vendor/m", Price: price}}},
			store.CatalogModel{Name: "broke-model", Targets: []store.CatalogTarget{{Provider: "broke", Upstream: "vendor/m"}}})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.app.Catalogs.Committed(ctx, c.Revision); err != nil {
		t.Fatal(err)
	}
	secret, _ := e.createKey(`{}`)
	chat := func(model string) (int, string) {
		return e.do(e.gw.URL, "POST", "/v1/chat/completions", secret, `{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)
	}

	type creditJSON struct {
		Type        string
		USD         float64    `json:"usd"`
		Timezone    string     `json:"timezone"`
		CycleStart  *time.Time `json:"cycle_start"`
		CycleEnd    *time.Time `json:"cycle_end"`
		TopupsUSD   *float64   `json:"topups_usd"`
		BalanceUSD  *float64   `json:"balance_usd"`
		Since       *time.Time `json:"since"`
		SpentUSD    float64    `json:"spent_usd"`
		UsedPercent float64    `json:"used_percent"`
		Level       string
	}
	credit := func(prov, account string) *creditJSON {
		t.Helper()
		code, out := e.do(e.admin.URL, "GET", "/admin/v1/providers", masterKey, "")
		if code != 200 {
			t.Fatalf("providers: %d %s", code, out)
		}
		var resp struct {
			Data []struct {
				Name     string
				Accounts []struct {
					Name   string
					Credit *creditJSON
				}
			}
		}
		if err := json.Unmarshal([]byte(out), &resp); err != nil {
			t.Fatal(err)
		}
		for _, p := range resp.Data {
			for _, a := range p.Accounts {
				if p.Name == prov && a.Name == account {
					return a.Credit
				}
			}
		}
		t.Fatalf("no %s/%s: %s", prov, account, out)
		return nil
	}

	// monthly: the spend of the cycle, which starts at midnight in Bangkok.
	if code, out := chat("credited-model"); code != 200 {
		t.Fatalf("credited: %d %s", code, out)
	}
	cost := e.usage()[0].CostUSD
	if cost <= 0 || cost >= 50 {
		t.Fatalf("cost %v: the test needs a cost between 0 and half the credit", cost)
	}
	bkk, _ := time.LoadLocation("Asia/Bangkok")
	now := time.Now().In(bkk)
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, bkk)
	if a := credit("credited", "plan-a"); a == nil || a.Type != "monthly" || a.USD != 100 || a.Timezone != "Asia/Bangkok" ||
		!a.CycleStart.Equal(first) || !a.CycleEnd.Equal(first.AddDate(0, 1, 0)) || math.Abs(a.SpentUSD-cost) > 1e-9 ||
		a.Level != map[bool]string{true: "warning", false: "ok"}[cost >= 80] || a.TopupsUSD != nil {
		t.Fatalf("plan-a credit: %+v (cost %v)", a, cost)
	}
	if b := credit("credited", "plan-b"); b != nil {
		t.Fatalf("plan-b has no credit: %+v", b)
	}

	// prepaid: an empty ledger, then top-ups through the admin API.
	if w := credit("pre", "wallet"); w.Type != "prepaid" || *w.BalanceUSD != 0 || w.Since != nil || w.Level != "exhausted" || w.CycleStart != nil {
		t.Fatalf("empty ledger: %+v", w)
	}
	for name, body := range map[string]string{
		"not prepaid":   `{"provider":"credited","account":"plan-a","usd":5}`,
		"zero":          `{"provider":"pre","account":"wallet","usd":0}`,
		"future":        `{"provider":"pre","account":"wallet","usd":5,"at":"2999-01-01T00:00:00Z"}`,
		"unknown field": `{"provider":"pre","account":"wallet","usd":5,"balance":1}`,
	} {
		if code, out := e.do(e.admin.URL, "POST", "/admin/v1/credit-entries", masterKey, body); code != 400 {
			t.Errorf("%s: %d %s", name, code, out)
		}
	}
	if code, _ := e.do(e.admin.URL, "POST", "/admin/v1/credit-entries", masterKey, `{"provider":"pre","account":"nope","usd":5}`); code != 404 {
		t.Errorf("unknown account: %d", code)
	}
	since := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	if code, out := e.do(e.admin.URL, "POST", "/admin/v1/credit-entries", masterKey,
		`{"provider":"pre","account":"wallet","usd":20,"note":"invoice 7","at":"`+since.Format(time.RFC3339)+`"}`); code != 201 || !strings.Contains(out, `"actor":"master_key"`) {
		t.Fatalf("top-up: %d %s", code, out)
	}
	if code, out := e.do(e.admin.URL, "POST", "/admin/v1/credit-entries", masterKey, `{"provider":"pre","account":"wallet","usd":-2.5,"note":"match the console"}`); code != 201 {
		t.Fatalf("correction: %d %s", code, out)
	}
	if code, out := chat("pre-model"); code != 200 {
		t.Fatalf("prepaid request: %d %s", code, out)
	}
	pcost := e.usage()[0].CostUSD
	w := credit("pre", "wallet")
	if *w.TopupsUSD != 17.5 || math.Abs(*w.BalanceUSD-(17.5-pcost)) > 1e-9 || !w.Since.Equal(since) || math.Abs(w.SpentUSD-pcost) > 1e-9 {
		t.Fatalf("prepaid credit: %+v (cost %v)", w, pcost)
	}
	code, out := e.do(e.admin.URL, "GET", "/admin/v1/credit-entries?provider=pre&account=wallet", masterKey, "")
	if code != 200 || strings.Count(out, `"id":"cre_`) != 2 || !strings.Contains(out, `"note":"invoice 7"`) {
		t.Fatalf("ledger: %d %s", code, out)
	}
	if code, _ := e.do(e.admin.URL, "GET", "/admin/v1/credit-entries?provider=pre", masterKey, ""); code != 400 {
		t.Fatalf("provider without account: %d", code)
	}

	// Out of credits: a clear error on the OpenAI format.
	code, out = chat("broke-model")
	if code != http.StatusPaymentRequired || !strings.Contains(out, `"code":"provider_credit_exhausted"`) || strings.Contains(out, "no credits") {
		t.Fatalf("out of credits: %d %s", code, out)
	}
	if r := e.usage()[0]; r.StatusCode != http.StatusPaymentRequired || r.Account != "only" || !strings.HasPrefix(r.Error, "provider_credit_exhausted") {
		t.Fatalf("usage record: %+v", r)
	}
}

// Account state is one system (spec-0004 section 4.4, D40): a rest seen by one node
// makes the other node skip the account too, and both show the same state.
func TestAccountStateShared(t *testing.T) {
	t.Parallel()
	a := newEnv(t)
	b := a.peer()
	ctx := context.Background()
	var hits atomic.Int32
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"message":"rate limited"}}`)
	}))
	defer limited.Close()

	c, err := catalog.Update(ctx, a.st, store.AnyRevision, "test", "two accounts", func(c *store.Catalog) error {
		c.Providers = append(c.Providers, store.CatalogProvider{Name: "multi", Type: "openai", BaseURL: a.upURL + "/v1",
			Accounts: []store.CatalogAccount{
				{Name: "plan-a", KeyRef: "env:AIF_TEST_UPSTREAM_KEY", BaseURL: limited.URL + "/v1"},
				{Name: "plan-b", KeyRef: "env:AIF_TEST_UPSTREAM_KEY"},
			}})
		c.Models = append(c.Models, store.CatalogModel{Name: "multi-model", Targets: []store.CatalogTarget{{Provider: "multi", Upstream: "vendor/m"}}})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.app.Catalogs.Committed(ctx, c.Revision); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the peer on the new catalog", func() bool { return b.app.Catalog.Revision() == c.Revision })

	secret, _ := a.createKey(`{}`)
	body := `{"model":"multi-model","messages":[{"role":"user","content":"hi"}]}`
	if code, out := a.do(a.gw.URL, "POST", "/v1/chat/completions", secret, body); code != 200 {
		t.Fatalf("node a: %d %s", code, out)
	}
	if hits.Load() != 1 {
		t.Fatalf("plan-a hits after node a: %d", hits.Load())
	}

	planA := func(e *env) map[string]any {
		_, out := e.do(e.admin.URL, "GET", "/admin/v1/providers", masterKey, "")
		var resp struct {
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
		return nil
	}
	eventually(t, "plan-a resting on node b", func() bool { s := planA(b); return s != nil && s["state"] == "cooldown" })
	sa, sb := planA(a), planA(b)
	if sa["cooldown_until"] != sb["cooldown_until"] || sa["last_error"] != sb["last_error"] {
		t.Fatalf("nodes differ:\na %v\nb %v", sa, sb)
	}

	// Node b never saw the 429 itself, but it skips plan-a.
	if code, out := b.do(b.gw.URL, "POST", "/v1/chat/completions", secret, body); code != 200 {
		t.Fatalf("node b: %d %s", code, out)
	}
	if r := b.usage()[0]; r.Account != "plan-b" || r.Attempts != 1 || hits.Load() != 1 {
		t.Fatalf("node b tried plan-a: account %q attempts %d hits %d", r.Account, r.Attempts, hits.Load())
	}

	// A node that starts now loads the rest from the bucket.
	late := a.peer()
	eventually(t, "plan-a resting on a late node", func() bool { s := planA(late); return s != nil && s["state"] == "cooldown" })
}
