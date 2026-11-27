package postgres

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"ai-foundation/gateway/internal/store"
)

func sampleCatalog() *store.Catalog {
	two := 2
	return &store.Catalog{
		Passthrough: true,
		Providers: []store.CatalogProvider{
			{Name: "zeta", Type: "openai", BaseURL: "http://z/v1", Headers: map[string]string{"X-A": "1"},
				Accounts: []store.CatalogAccount{{Name: "b", KeyRef: "env:KEY_B", Credit: &store.Credit{Type: "monthly", USD: 100.5, CycleDay: 5, Timezone: "Asia/Bangkok"}}, {Name: "c", KeyRef: "env:KEY_C", Credit: &store.Credit{Type: "prepaid"}}, {Name: "a", KeyRef: "file:/run/a"}}},
			{Name: "alpha", Type: "anthropic", OpenAIFormat: "compat", WorkspaceID: "wrk", Headers: map[string]string{},
				Accounts: []store.CatalogAccount{{Name: "default"}}},
		},
		Models: []store.CatalogModel{
			{Name: "m2", Formats: []string{"openai"}, Targets: []store.CatalogTarget{
				{Provider: "zeta", Upstream: "u2", Price: &store.Price{Input: 1, Output: 2, CachedInput: 0.5, CacheWrite: 1.25}, Formats: []string{}},
				{Provider: "alpha", Upstream: "u2b", Weight: &two, Formats: []string{"openai"}},
			}},
			{Name: "m1", Formats: []string{}, Policy: "round_robin", Sticky: "session", MaxAttempts: 3, Targets: []store.CatalogTarget{{Provider: "alpha", Upstream: "u1", Formats: []string{}}}},
		},
	}
}

func put(c *store.Catalog) func(*store.Catalog) error {
	return func(cur *store.Catalog) error {
		cur.Passthrough, cur.Providers, cur.Models = c.Passthrough, c.Providers, c.Models
		return nil
	}
}

func TestCatalogRoundTripKeepsOrder(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()

	empty, err := s.GetCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if empty.Revision != 0 || !empty.Empty() || empty.Passthrough {
		t.Fatalf("fresh catalog: %+v", empty)
	}

	want := sampleCatalog()
	got, err := s.UpdateCatalog(ctx, 0, "seed", "first", put(want))
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != 1 || got.UpdatedBy != "seed" {
		t.Fatalf("after update: rev %d by %q", got.Revision, got.UpdatedBy)
	}
	read, err := s.GetCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if read.Revision != 1 || !read.Passthrough {
		t.Fatalf("read meta: %+v", read)
	}
	if !reflect.DeepEqual(read.Providers, want.Providers) {
		t.Fatalf("providers:\n got %+v\nwant %+v", read.Providers, want.Providers)
	}
	if !reflect.DeepEqual(read.Models, want.Models) {
		t.Fatalf("models:\n got %+v\nwant %+v", read.Models, want.Models)
	}
	if rev, err := s.CatalogRevision(ctx); err != nil || rev != 1 {
		t.Fatalf("revision: %d %v", rev, err)
	}

	// A smaller catalog replaces every row.
	if _, err := s.UpdateCatalog(ctx, 1, "admin", "trim", func(c *store.Catalog) error {
		c.Models = c.Models[1:]
		c.Providers = c.Providers[1:]
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	read, _ = s.GetCatalog(ctx)
	if read.Revision != 2 || len(read.Providers) != 1 || read.Providers[0].Name != "alpha" || len(read.Models) != 1 || read.Models[0].Name != "m1" {
		t.Fatalf("after trim: %+v", read)
	}

	ch, err := s.ListCatalogChanges(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ch) != 2 || ch[0].Revision != 2 || ch[0].Actor != "admin" || ch[0].Summary != "trim" || ch[1].Actor != "seed" {
		t.Fatalf("changes: %+v", ch)
	}
}

func TestCatalogStaleAndAbort(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	if _, err := s.UpdateCatalog(ctx, 0, "seed", "", put(sampleCatalog())); err != nil {
		t.Fatal(err)
	}
	// Based on revision 0, but the catalog is at 1 now.
	if _, err := s.UpdateCatalog(ctx, 0, "admin", "", put(&store.Catalog{})); !errors.Is(err, store.ErrStale) {
		t.Fatalf("stale: got %v", err)
	}
	// fn refuses (a failed validation): nothing changes.
	boom := errors.New("invalid")
	if _, err := s.UpdateCatalog(ctx, store.AnyRevision, "admin", "", func(c *store.Catalog) error {
		c.Models = nil
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("abort: got %v", err)
	}
	// A database rule breaks (target on a missing provider): rolled back too.
	if _, err := s.UpdateCatalog(ctx, store.AnyRevision, "admin", "", func(c *store.Catalog) error {
		c.Models[0].Targets[0].Provider = "missing"
		return nil
	}); err == nil {
		t.Fatal("target on a missing provider was written")
	}
	c, _ := s.GetCatalog(ctx)
	if c.Revision != 1 || len(c.Models) != 2 || c.Models[0].Targets[0].Provider != "zeta" {
		t.Fatalf("catalog changed by a failed update: %+v", c)
	}
	if ch, _ := s.ListCatalogChanges(ctx, 10); len(ch) != 1 {
		t.Fatalf("changes after failed updates: %d", len(ch))
	}
}

// Writers on several nodes queue on the meta row: every change lands, revisions do not repeat.
func TestCatalogConcurrentUpdates(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Go(func() {
			_, err := s.UpdateCatalog(ctx, store.AnyRevision, "admin", "", func(c *store.Catalog) error {
				c.Models = append(c.Models, store.CatalogModel{Name: "m" + string(rune('a'+len(c.Models)))})
				return nil
			})
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	c, _ := s.GetCatalog(ctx)
	if c.Revision != n || len(c.Models) != n {
		t.Fatalf("rev %d with %d models, want %d and %d", c.Revision, len(c.Models), n, n)
	}
}

func TestCreditLedger(t *testing.T) {
	t.Parallel()
	st := open(t)
	ctx := context.Background()
	day := func(d int) time.Time { return time.Date(2026, 11, d, 0, 0, 0, 0, time.UTC) }
	for i, e := range []store.CreditEntry{
		{ID: "cre_1", Provider: "p", Account: "a", USD: 50, Note: "invoice 1", At: day(1), CreatedAt: day(1), Actor: "cli"},
		{ID: "cre_2", Provider: "p", Account: "a", USD: -3.25, At: day(20), CreatedAt: day(21), Actor: "usr_x"},
		{ID: "cre_3", Provider: "p", Account: "b", USD: 10, At: day(10), CreatedAt: day(10), Actor: "master_key"},
	} {
		if err := st.AddCreditEntry(ctx, &e); err != nil {
			t.Fatalf("entry %d: %v", i, err)
		}
	}
	got, err := st.ListCreditEntries(ctx, "p", "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "cre_2" || got[0].USD != -3.25 || got[1].Note != "invoice 1" || !got[1].At.Equal(day(1)) {
		t.Fatalf("p/a ledger: %+v", got)
	}
	if all, _ := st.ListCreditEntries(ctx, "", ""); len(all) != 3 || all[0].ID != "cre_2" || all[1].ID != "cre_3" {
		t.Fatalf("all: %+v", all)
	}
	dup := store.CreditEntry{ID: "cre_1", Provider: "p", Account: "a", USD: 1, At: day(2), CreatedAt: day(2), Actor: "cli"}
	if err := st.AddCreditEntry(ctx, &dup); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("same id: %v", err)
	}
}
