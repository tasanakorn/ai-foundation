package catalog_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"ai-foundation/gateway/internal/catalog"
	"ai-foundation/gateway/internal/pgtest"
	"ai-foundation/gateway/internal/provider"
	"ai-foundation/gateway/internal/store"
	"ai-foundation/gateway/internal/store/postgres"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func one(name, typ, keyRef string) store.CatalogProvider {
	p := store.CatalogProvider{Name: name, Type: typ, BaseURL: "http://" + name + "/v1",
		Accounts: []store.CatalogAccount{{Name: catalog.DefaultAccount, KeyRef: keyRef}}}
	if typ == "anthropic" {
		p.OpenAIFormat = "compat"
	}
	return p
}

func model(name, prov, upstream string) store.CatalogModel {
	return store.CatalogModel{Name: name, Targets: []store.CatalogTarget{{Provider: prov, Upstream: upstream}}}
}

func valid() *store.Catalog {
	return &store.Catalog{
		Providers: []store.CatalogProvider{one("local", "openai", ""), one("claude", "anthropic", "env:AIF_CATALOG_TEST_KEY")},
		Models:    []store.CatalogModel{model("small", "local", "vendor/small"), model("sonnet", "claude", "")},
	}
}

func TestValidate(t *testing.T) {
	if err := catalog.Validate(valid()); err != nil {
		t.Fatalf("valid catalog: %v", err)
	}
	cases := map[string]struct {
		edit func(*store.Catalog)
		want string
	}{
		"two accounts": {func(c *store.Catalog) {
			c.Providers[0].Accounts = append(c.Providers[0].Accounts, c.Providers[0].Accounts[0])
		}, "account \"default\" defined twice"},
		"no account":       {func(c *store.Catalog) { c.Providers[0].Accounts = nil }, "needs at least one account"},
		"account name":     {func(c *store.Catalog) { c.Providers[0].Accounts[0].Name = "Plan A" }, "account name"},
		"key value":        {func(c *store.Catalog) { c.Providers[0].Accounts[0].KeyRef = "sk-live-123" }, "not a key value"},
		"relative file":    {func(c *store.Catalog) { c.Providers[0].Accounts[0].KeyRef = "file:key.txt" }, "absolute path"},
		"bad env name":     {func(c *store.Catalog) { c.Providers[0].Accounts[0].KeyRef = "env:1X" }, "variable name"},
		"two targets":      {func(c *store.Catalog) { c.Models[0].Targets = append(c.Models[0].Targets, c.Models[0].Targets[0]) }, "defined twice"},
		"unknown provider": {func(c *store.Catalog) { c.Models[0].Targets[0].Provider = "nope" }, `unknown provider "nope"`},
		"duplicate model":  {func(c *store.Catalog) { c.Models[1].Name = "small" }, "defined twice"},
		"bad type":         {func(c *store.Catalog) { c.Providers[0].Type = "grpc" }, "not supported"},
		"anthropic format": {func(c *store.Catalog) { c.Models[0].Formats = []string{"anthropic"} }, "served by none of its targets"},
		"anthropic target": {func(c *store.Catalog) { c.Models[0].Targets[0].Formats = []string{"anthropic"} }, "needs a provider of type anthropic"},
	}
	for name, tc := range cases {
		c := valid()
		tc.edit(c)
		err := catalog.Validate(c)
		var inv *catalog.InvalidError
		if err == nil || !errors.As(err, &inv) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want InvalidError with %q", name, err, tc.want)
		}
	}
}

func TestResolveKey(t *testing.T) {
	t.Setenv("AIF_CATALOG_TEST_KEY", "from-env")
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	os.WriteFile(path, []byte("from-file\n"), 0o600)
	for ref, want := range map[string]string{"": "", "env:AIF_CATALOG_TEST_KEY": "from-env", "file:" + path: "from-file"} {
		if got, err := catalog.ResolveKey(ref); err != nil || got != want {
			t.Errorf("%q: got %q, %v", ref, got, err)
		}
	}
	// A rotated file is read again.
	os.WriteFile(path, []byte("rotated"), 0o600)
	if got, _ := catalog.ResolveKey("file:" + path); got != "rotated" {
		t.Errorf("rotated file: got %q", got)
	}
	for _, ref := range []string{"env:AIF_CATALOG_TEST_UNSET", "file:" + filepath.Join(dir, "missing")} {
		_, err := catalog.ResolveKey(ref)
		if err == nil || !strings.Contains(err.Error(), ref) {
			t.Errorf("%q: got %v", ref, err)
		}
	}
}

func TestBuildAndOffer(t *testing.T) {
	t.Setenv("AIF_CATALOG_TEST_KEY", "k")
	c := valid()
	c.Revision = 3
	s, err := catalog.Build(c, catalog.Env{Client: provider.NewHTTPClient()})
	if err != nil {
		t.Fatal(err)
	}
	r, ok := s.Router.Resolve("small")
	if !ok || r.Upstream != "vendor/small" || r.Provider.Name() != "local" {
		t.Fatalf("small: %+v %v", r, ok)
	}
	if r, ok := s.Router.Resolve("sonnet"); !ok || r.Upstream != "sonnet" || !r.Serves("anthropic") {
		t.Fatalf("sonnet gets the defaults (upstream = name, anthropic format): %+v", r)
	}

	h := catalog.NewHolder(&catalog.Snapshot{Router: s.Router})
	if !h.Offer(s) || h.Revision() != 3 {
		t.Fatal("newer snapshot not taken")
	}
	if h.Offer(&catalog.Snapshot{Revision: 3}) || h.Offer(&catalog.Snapshot{Revision: 2}) {
		t.Fatal("same or older revision replaced the snapshot")
	}

	// A key reference that does not resolve fails the build (the caller keeps its snapshot).
	os.Unsetenv("AIF_CATALOG_TEST_KEY")
	if _, err := catalog.Build(c, catalog.Env{Client: provider.NewHTTPClient()}); err == nil || !strings.Contains(err.Error(), "env:AIF_CATALOG_TEST_KEY") {
		t.Fatalf("unresolved key: got %v", err)
	}
}

// Readers never block and never see a torn snapshot while another goroutine swaps.
func TestHolderConcurrent(t *testing.T) {
	t.Setenv("AIF_CATALOG_TEST_KEY", "k")
	h := catalog.NewHolder(&catalog.Snapshot{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for rev := int64(1); rev <= 200; rev++ {
			c := valid()
			c.Revision = rev
			s, err := catalog.Build(c, catalog.Env{Client: provider.NewHTTPClient()})
			if err != nil {
				t.Error(err)
				return
			}
			h.Offer(s)
		}
	})
	for range 4 {
		wg.Go(func() {
			last := int64(0)
			for range 2000 {
				s := h.Snapshot()
				if s.Revision < last {
					t.Errorf("revision went back: %d after %d", s.Revision, last)
					return
				}
				last = s.Revision
				if s.Router != nil {
					if _, ok := s.Router.Resolve("small"); !ok {
						t.Error("snapshot without its models")
						return
					}
				}
			}
		})
	}
	wg.Wait()
	if h.Revision() != 200 {
		t.Fatalf("final revision %d", h.Revision())
	}
}

func TestUpdateValidatesInTransaction(t *testing.T) {
	t.Setenv("AIF_CATALOG_TEST_KEY", "k")
	st, err := postgres.Open(context.Background(), pgtest.URL(t), postgres.Options{MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	if _, err := catalog.Replace(ctx, st, 0, "seed", "from file", valid()); err != nil {
		t.Fatal(err)
	}
	// An edit that breaks the catalog is refused and never committed.
	_, err = catalog.Update(ctx, st, 1, "admin", "remove local", func(c *store.Catalog) error {
		c.Providers = c.Providers[1:] // model "small" still points at "local"
		return nil
	})
	var inv *catalog.InvalidError
	if !errors.As(err, &inv) {
		t.Fatalf("broken edit: got %v", err)
	}
	s, err := catalog.Load(ctx, st, catalog.Env{Client: provider.NewHTTPClient()})
	if err != nil {
		t.Fatal(err)
	}
	if s.Revision != 1 || len(s.Router.Models()) != 2 {
		t.Fatalf("loaded rev %d with %d models", s.Revision, len(s.Router.Models()))
	}
	if _, err := catalog.Replace(ctx, st, 0, "admin", "", valid()); !errors.Is(err, store.ErrStale) {
		t.Fatalf("stale replace: got %v", err)
	}
}
