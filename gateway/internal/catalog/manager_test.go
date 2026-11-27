package catalog_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ai-foundation/gateway/internal/catalog"
	"ai-foundation/gateway/internal/config"
	"ai-foundation/gateway/internal/pgtest"
	"ai-foundation/gateway/internal/provider"
	"ai-foundation/gateway/internal/store"
	"ai-foundation/gateway/internal/store/postgres"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func fileYAML(mode, models string) string {
	return `
database: { url: "postgres://unused" }
catalog: { mode: ` + mode + `, watch_interval: 100ms }
providers:
  - { name: local, base_url: http://local/v1 }
  - { name: keyed, base_url: http://keyed/v1, api_key: ${AIF_CATALOG_TEST_KEY} }
models:
` + models
}

func parse(t *testing.T, raw string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func openStore(t *testing.T, url string) store.Store {
	t.Helper()
	st, err := postgres.Open(context.Background(), url, postgres.Options{MaxConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func start(t *testing.T, st store.Store, cfg *config.Config) *catalog.Manager {
	t.Helper()
	file, err := catalog.FromFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m, err := catalog.Start(context.Background(), catalog.Options{Store: st, Client: provider.NewHTTPClient(), Log: quiet,
		Mode: cfg.Catalog.Mode, File: file, Path: cfg.Path, WatchInterval: cfg.Catalog.WatchInterval})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestFromFileKeyReferences(t *testing.T) {
	t.Setenv("AIF_CATALOG_TEST_KEY", "sk-test")
	cfg := parse(t, `
database: { url: "postgres://unused" }
providers:
  - { name: a, base_url: http://a/v1, api_key: ${AIF_CATALOG_TEST_KEY} }
  - { name: b, base_url: http://b/v1, api_key: "${AIF_CATALOG_TEST_UNSET:-}" }
  - { name: c, base_url: http://c/v1, api_key: env:OTHER_KEY }
  - { name: d, base_url: http://d/v1, api_key: file:/run/secrets/d }
  - { name: e, base_url: http://e/v1 }
`)
	c, err := catalog.FromFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"a": "env:AIF_CATALOG_TEST_KEY", "b": "", "c": "env:OTHER_KEY", "d": "file:/run/secrets/d", "e": ""}
	for _, p := range c.Providers {
		if got := p.Accounts[0].KeyRef; got != want[p.Name] || p.Accounts[0].Name != catalog.DefaultAccount {
			t.Errorf("%s: key ref %q, want %q", p.Name, got, want[p.Name])
		}
	}
	for _, raw := range []string{"api_key: sk-written-in-file", `api_key: "${AIF_CATALOG_TEST_UNSET:-sk-default}"`} {
		cfg := parse(t, "database: { url: \"postgres://unused\" }\nproviders:\n  - { name: x, base_url: http://x/v1, "+raw+" }\n")
		if _, err := catalog.FromFile(cfg); err == nil || !strings.Contains(err.Error(), "never stored in the database") {
			t.Errorf("%s: got %v", raw, err)
		}
	}
}

// Several nodes start at once on an empty database: exactly one seeds it. Later
// starts keep the database catalog, even when the file changed.
func TestSeedOnce(t *testing.T) {
	t.Setenv("AIF_CATALOG_TEST_KEY", "sk-test")
	url := pgtest.URL(t)
	cfg := parse(t, fileYAML("database", "  - { name: small, provider: local }\n"))
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if m := start(t, openStore(t, url), cfg); m.Holder().Revision() != 1 {
				t.Errorf("node serves revision %d", m.Holder().Revision())
			}
		})
	}
	wg.Wait()
	st := openStore(t, url)
	if ch, _ := st.ListCatalogChanges(context.Background(), 10); len(ch) != 1 || ch[0].Actor != catalog.ActorSeed {
		t.Fatalf("changes: %+v", ch)
	}

	changed := parse(t, fileYAML("database", "  - { name: other, provider: local }\n"))
	m := start(t, st, changed)
	if _, ok := m.Holder().Router().Resolve("small"); !ok || m.Holder().Revision() != 1 {
		t.Fatal("a later start replaced the database catalog with the file's")
	}
}

func TestFileModeSync(t *testing.T) {
	t.Setenv("AIF_CATALOG_TEST_KEY", "sk-test")
	st := openStore(t, pgtest.URL(t))
	one := parse(t, fileYAML("file", "  - { name: small, provider: local }\n"))
	if m := start(t, st, one); m.Holder().Revision() != 1 {
		t.Fatalf("first sync: revision %d", m.Holder().Revision())
	}
	// A restart (or another node) with the same file writes nothing.
	if m := start(t, st, one); m.Holder().Revision() != 1 {
		t.Fatalf("same file added a revision: %d", m.Holder().Revision())
	}
	// A changed file wins over the database.
	two := parse(t, fileYAML("file", "  - { name: big, provider: keyed }\n"))
	m := start(t, st, two)
	if _, ok := m.Holder().Router().Resolve("big"); !ok || m.Holder().Revision() != 2 {
		t.Fatalf("changed file: revision %d", m.Holder().Revision())
	}
	if ch, _ := st.ListCatalogChanges(context.Background(), 10); ch[0].Actor != catalog.ActorFile {
		t.Fatalf("change actor: %+v", ch[0])
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestFileWatch(t *testing.T) {
	t.Setenv("AIF_CATALOG_TEST_KEY", "sk-test")
	st := openStore(t, pgtest.URL(t))
	path := filepath.Join(t.TempDir(), "aif.yaml")
	write := func(s string) {
		t.Helper()
		// Write a new file and rename it, like a Kubernetes ConfigMap update.
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, path); err != nil {
			t.Fatal(err)
		}
	}
	write(fileYAML("file", "  - { name: small, provider: local }\n"))
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	m := start(t, st, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	has := func(model string) func() bool {
		return func() bool { _, ok := m.Holder().Router().Resolve(model); return ok }
	}

	write(fileYAML("file", "  - { name: small, provider: local }\n  - { name: added, provider: keyed }\n"))
	waitFor(t, "the added model", has("added"))
	rev := m.Holder().Revision()

	// Invalid files are ignored: a broken model link, then a key value in the file.
	write(fileYAML("file", "  - { name: broken, provider: missing }\n"))
	time.Sleep(400 * time.Millisecond)
	write(strings.Replace(fileYAML("file", "  - { name: small, provider: local }\n"), "${AIF_CATALOG_TEST_KEY}", "sk-in-file", 1))
	time.Sleep(400 * time.Millisecond)
	if !has("added")() || m.Holder().Revision() != rev {
		t.Fatalf("an invalid file changed the catalog: revision %d, want %d", m.Holder().Revision(), rev)
	}

	// SIGHUP applies the file even when the file did not change: here the database
	// drifted (an outside write), and only the trigger restores the file's catalog.
	write(fileYAML("file", "  - { name: small, provider: local }\n  - { name: added, provider: keyed }\n"))
	time.Sleep(400 * time.Millisecond) // same content as the database: nothing written
	if m.Holder().Revision() != rev {
		t.Fatalf("an unchanged catalog added a revision: %d", m.Holder().Revision())
	}
	if _, err := catalog.Replace(ctx, st, store.AnyRevision, "admin", "drift", &store.Catalog{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	if m.Holder().Revision() != rev {
		t.Fatal("the watch reacted although the file did not change")
	}
	m.Trigger()
	waitFor(t, "the file's catalog after SIGHUP", func() bool { return m.Holder().Revision() == rev+2 && has("added")() })
}

// In database mode a trigger (SIGHUP) reloads from the database.
func TestDatabaseModeTrigger(t *testing.T) {
	t.Setenv("AIF_CATALOG_TEST_KEY", "sk-test")
	st := openStore(t, pgtest.URL(t))
	m := start(t, st, parse(t, fileYAML("database", "  - { name: small, provider: local }\n")))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	if _, err := catalog.Update(ctx, st, 1, "admin", "add", func(c *store.Catalog) error {
		c.Models = append(c.Models, store.CatalogModel{Name: "new", Targets: []store.CatalogTarget{{Provider: "local", Upstream: "new"}}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	m.Trigger()
	waitFor(t, "revision 2", func() bool { return m.Holder().Revision() == 2 })
}
