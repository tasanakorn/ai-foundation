package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"ai-foundation/gateway/internal/pgtest"
	"ai-foundation/gateway/internal/store"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), pgtest.URL(t), Options{MaxConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// Several gateway nodes may start at once on the same database.
func TestConcurrentOpenMigratesOnce(t *testing.T) {
	t.Parallel()
	url := pgtest.URL(t)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			s, err := Open(context.Background(), url, Options{MaxConns: 2})
			if err != nil {
				errs <- err
				return
			}
			s.Close()
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	s, err := Open(context.Background(), url, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n, maxV int
	if err := s.pool.QueryRow(context.Background(), `SELECT COUNT(*), MAX(version) FROM schema_migrations`).Scan(&n, &maxV); err != nil {
		t.Fatal(err)
	}
	if n != len(migrations) || maxV != len(migrations) {
		t.Fatalf("schema_migrations has %d rows, max %d; want %d", n, maxV, len(migrations))
	}
}

func TestOpenRejectsNewerSchema(t *testing.T) {
	t.Parallel()
	url := pgtest.URL(t)
	s, err := Open(context.Background(), url, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(context.Background(), `INSERT INTO schema_migrations VALUES (999, now())`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(context.Background(), url, Options{}); err == nil {
		t.Fatal("want error for a schema newer than the binary")
	}
}

func TestKeys(t *testing.T) {
	t.Parallel()
	s, ctx := open(t), context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	exp := now.Add(time.Hour)
	k := &store.Key{ID: "key_1", Name: "a", Prefix: "aif_ab", Hash: "h1", TenantID: "acme", AppID: "chat",
		AllowedModels: []string{"small", "openrouter/*"}, Metadata: map[string]string{"plan": "pro"}, CreatedAt: now, ExpiresAt: &exp}
	if err := s.CreateKey(ctx, k); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateKey(ctx, &store.Key{ID: "key_2", Prefix: "p", Hash: "h1", CreatedAt: now}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate hash: got %v, want ErrConflict", err)
	}
	if err := s.CreateKey(ctx, &store.Key{ID: "key_2", Prefix: "p", Hash: "h2", TenantID: "beta", CreatedAt: now.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetKeyByHash(ctx, "h1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "key_1" || got.TenantID != "acme" || len(got.AllowedModels) != 2 || got.Metadata["plan"] != "pro" ||
		!got.CreatedAt.Equal(now) || got.ExpiresAt == nil || !got.ExpiresAt.Equal(exp) || got.RevokedAt != nil {
		t.Fatalf("round trip: %+v", got)
	}
	if got.CreatedAt.Location() != time.UTC {
		t.Errorf("created_at not UTC: %v", got.CreatedAt.Location())
	}
	k2, _ := s.GetKey(ctx, "key_2")
	if k2.AllowedModels == nil || k2.Metadata == nil {
		t.Errorf("empty lists must be non-nil: %+v", k2)
	}
	if _, err := s.GetKey(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing key: %v", err)
	}

	list, _ := s.ListKeys(ctx, store.KeyFilter{})
	if len(list) != 2 || list[0].ID != "key_2" {
		t.Fatalf("list newest first: %+v", list)
	}
	list, _ = s.ListKeys(ctx, store.KeyFilter{TenantID: "acme", AppID: "chat"})
	if len(list) != 1 || list[0].ID != "key_1" {
		t.Fatalf("filter: %+v", list)
	}
	list, _ = s.ListKeys(ctx, store.KeyFilter{Limit: 1, Offset: 1})
	if len(list) != 1 || list[0].ID != "key_1" {
		t.Fatalf("paging: %+v", list)
	}

	if err := s.RevokeKey(ctx, "key_1", now); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeKey(ctx, "key_1", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetKey(ctx, "key_1")
	if got.RevokedAt == nil || !got.RevokedAt.Equal(now) {
		t.Fatalf("revoke keeps the first time: %v", got.RevokedAt)
	}
	if err := s.RevokeKey(ctx, "nope", now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoke missing: %v", err)
	}
	list, _ = s.ListKeys(ctx, store.KeyFilter{})
	all, _ := s.ListKeys(ctx, store.KeyFilter{IncludeRevoked: true})
	if len(list) != 1 || len(all) != 2 {
		t.Fatalf("revoked filter: %d active, %d all", len(list), len(all))
	}
}

func TestUsage(t *testing.T) {
	t.Parallel()
	s, ctx := open(t), context.Background()
	day1 := time.Date(2026, 11, 10, 23, 30, 0, 0, time.UTC)
	day2 := day1.Add(time.Hour) // 2026-11-11 00:30 UTC
	recs := []store.UsageRecord{
		{ID: "r1", CreatedAt: day1, KeyID: "k1", TenantID: "acme", AppID: "chat", EndUser: "u1", Model: "small", Provider: "fake",
			Stream: true, StatusCode: 200, PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, CachedTokens: 2, CostUSD: 0.5, Estimated: true, LatencyMS: 12},
		{ID: "r2", CreatedAt: day2, KeyID: "k1", TenantID: "acme", AppID: "chat", Model: "big", StatusCode: 429, Error: "rate limited"},
		{ID: "r3", CreatedAt: day2.Add(time.Minute), KeyID: "k2", TenantID: "beta", Model: "small", StatusCode: 200, TotalTokens: 100, CostUSD: 1.25},
	}
	if err := s.InsertUsage(ctx, recs); err != nil {
		t.Fatal(err)
	}
	// A retried batch (same IDs) must not count twice.
	if err := s.InsertUsage(ctx, recs[:2]); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertUsage(ctx, nil); err != nil {
		t.Fatal(err)
	}

	all, _ := s.ListUsage(ctx, store.UsageFilter{})
	if len(all) != 3 || all[0].ID != "r3" {
		t.Fatalf("list newest first: %+v", all)
	}
	r1 := all[2]
	if !r1.CreatedAt.Equal(day1) || !r1.Stream || !r1.Estimated || r1.CachedTokens != 2 || r1.CostUSD != 0.5 || r1.EndUser != "u1" || r1.LatencyMS != 12 {
		t.Fatalf("round trip: %+v", r1)
	}
	for _, c := range []struct {
		f    store.UsageFilter
		want int
	}{
		{store.UsageFilter{TenantID: "acme"}, 2},
		{store.UsageFilter{TenantID: "acme", AppID: "chat", KeyID: "k1"}, 2},
		{store.UsageFilter{EndUser: "u1"}, 1},
		{store.UsageFilter{ID: "r2"}, 1},
		{store.UsageFilter{Model: "small"}, 2},
		{store.UsageFilter{From: day2}, 2},
		{store.UsageFilter{To: day2}, 1},
		{store.UsageFilter{From: day2, To: day2.Add(time.Minute)}, 1},
		{store.UsageFilter{Limit: 2}, 2},
		{store.UsageFilter{Offset: 2}, 1},
	} {
		got, err := s.ListUsage(ctx, c.f)
		if err != nil || len(got) != c.want {
			t.Errorf("ListUsage(%+v) = %d rows, %v; want %d", c.f, len(got), err, c.want)
		}
	}

	sum, err := s.SummarizeUsage(ctx, store.UsageFilter{}, "none")
	if err != nil || len(sum) != 1 {
		t.Fatalf("summary none: %+v %v", sum, err)
	}
	if g := sum[0]; g.Requests != 3 || g.Errors != 1 || g.TotalTokens != 115 || g.PromptTokens != 10 || g.CostUSD != 1.75 {
		t.Fatalf("summary none: %+v", g)
	}
	byDay, _ := s.SummarizeUsage(ctx, store.UsageFilter{}, "day")
	if len(byDay) != 2 || byDay[0].Group != "2026-11-10" || byDay[1].Group != "2026-11-11" || byDay[1].Requests != 2 {
		t.Fatalf("summary by UTC day: %+v", byDay)
	}
	for _, g := range store.GroupBy {
		if _, err := s.SummarizeUsage(ctx, store.UsageFilter{TenantID: "acme"}, g); err != nil {
			t.Errorf("group_by %s: %v", g, err)
		}
	}
	byTenant, _ := s.SummarizeUsage(ctx, store.UsageFilter{}, "tenant")
	if len(byTenant) != 2 || byTenant[0].Group != "acme" || byTenant[0].Requests != 2 {
		t.Fatalf("summary by tenant: %+v", byTenant)
	}
	empty, err := s.SummarizeUsage(ctx, store.UsageFilter{TenantID: "nobody"}, "none")
	if err != nil || len(empty) != 0 { // no matching rows = no groups
		t.Fatalf("empty summary: %+v %v", empty, err)
	}
	if _, err := s.SummarizeUsage(ctx, store.UsageFilter{}, "bogus"); err == nil {
		t.Fatal("unknown group_by must fail")
	}
}

func TestAdminUsersAndSessions(t *testing.T) {
	t.Parallel()
	s, ctx := open(t), context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	u := &store.AdminUser{ID: "usr_1", Email: "a@example.com", Name: "A", Role: store.RoleAdmin, PasswordHash: "x", CreatedAt: now, UpdatedAt: now}
	if err := s.CreateAdminUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	dup := *u
	dup.ID = "usr_2"
	if err := s.CreateAdminUser(ctx, &dup); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate email: %v", err)
	}
	if err := s.CreateAdminUser(ctx, &store.AdminUser{ID: "usr_2", Email: "v@example.com", Role: store.RoleViewer, CreatedAt: now.Add(time.Second), UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAdminUserByEmail(ctx, "a@example.com")
	if err != nil || got.ID != "usr_1" || got.PasswordHash != "x" || !got.CreatedAt.Equal(now) {
		t.Fatalf("by email: %+v %v", got, err)
	}
	if _, err := s.GetAdminUser(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing user: %v", err)
	}
	if n, _ := s.CountActiveAdmins(ctx); n != 1 {
		t.Fatalf("active admins = %d, want 1", n)
	}

	got.Name, got.Role, got.DisabledAt, got.UpdatedAt = "B", store.RoleViewer, &now, now.Add(time.Minute)
	if err := s.UpdateAdminUser(ctx, got); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAdminLastLogin(ctx, "usr_1", now); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetAdminUser(ctx, "usr_1")
	if got.Name != "B" || got.Role != store.RoleViewer || got.DisabledAt == nil || got.LastLoginAt == nil {
		t.Fatalf("update: %+v", got)
	}
	if n, _ := s.CountActiveAdmins(ctx); n != 0 {
		t.Fatalf("active admins = %d, want 0", n)
	}
	if err := s.UpdateAdminUser(ctx, &store.AdminUser{ID: "nope"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("update missing: %v", err)
	}
	users, _ := s.ListAdminUsers(ctx)
	if len(users) != 2 || users[0].ID != "usr_1" {
		t.Fatalf("list oldest first: %+v", users)
	}

	for _, h := range []string{"s1", "s2"} {
		if err := s.CreateAdminSession(ctx, &store.AdminSession{Hash: h, UserID: "usr_1", CreatedAt: now, ExpiresAt: now.Add(time.Hour), IP: "127.0.0.1"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateAdminSession(ctx, &store.AdminSession{Hash: "old", UserID: "usr_2", CreatedAt: now, ExpiresAt: now.Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	if a, err := s.GetAdminSession(ctx, "s1"); err != nil || a.UserID != "usr_1" || a.IP != "127.0.0.1" {
		t.Fatalf("get session: %+v %v", a, err)
	}
	if _, err := s.GetAdminSession(ctx, "old"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired session must be not found: %v", err)
	}
	if err := s.DeleteAdminSessions(ctx, "usr_1", "s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAdminSession(ctx, "s2"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("other session must be gone: %v", err)
	}
	if _, err := s.GetAdminSession(ctx, "s1"); err != nil {
		t.Fatalf("kept session: %v", err)
	}
	if err := s.DeleteExpiredAdminSessions(ctx, now); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM admin_sessions`).Scan(&n)
	if n != 1 {
		t.Fatalf("sessions left = %d, want 1", n)
	}
	// Deleting a user deletes its sessions.
	if err := s.DeleteAdminUser(ctx, "usr_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAdminSession(ctx, "s1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("session of deleted user: %v", err)
	}
	if err := s.DeleteAdminUser(ctx, "usr_1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete missing: %v", err)
	}
}

// rollbackV9 is the manual roll back of migration v9 in change-0013 section 5. Keep the two
// the same.
const rollbackV9 = `
BEGIN;
DROP TABLE IF EXISTS admin_oidc_flows;
DROP TABLE IF EXISTS admin_identities;
ALTER TABLE admin_users DROP COLUMN link_allowed_until;
DELETE FROM schema_migrations WHERE version = 9;
COMMIT;`

func TestRollbackV9(t *testing.T) {
	t.Parallel()
	url := pgtest.URL(t)
	ctx := context.Background()
	s, err := Open(ctx, url, Options{MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	u := &store.AdminUser{ID: "usr_1", Email: "a@example.com", Role: store.RoleAdmin, CreatedAt: now, UpdatedAt: now, LinkAllowedUntil: &now}
	if err := s.CreateAdminUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAdminIdentity(ctx, &store.AdminIdentity{ID: "idn_1", UserID: u.ID, Issuer: "https://i", Subject: "s", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	// Roll backs run newest first: v10, then v9.
	for _, sql := range []string{rollbackV10, rollbackV9} {
		if _, err := s.pool.Exec(ctx, sql); err != nil {
			t.Fatalf("roll back: %v", err)
		}
	}
	var v int
	if err := s.pool.QueryRow(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil || v != 8 {
		t.Fatalf("version after roll back: %d %v", v, err)
	}
	s.Close()
	// The account is still there, and the migration runs again on the next start.
	s, err = Open(ctx, url, Options{MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, err := s.GetAdminUser(ctx, u.ID); err != nil || got.LinkAllowedUntil != nil {
		t.Fatalf("after migrating again: %+v %v", got, err)
	}
}

func TestOIDCLinksAndFlows(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	now := time.Now().UTC()
	u := &store.AdminUser{ID: "usr_1", Email: "a@example.com", Role: store.RoleAdmin, CreatedAt: now, UpdatedAt: now}
	if err := s.CreateAdminUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	// One-time allowance: used once, and only before it ends.
	soon := now.Add(time.Hour)
	if err := s.AllowAdminLink(ctx, u.ID, &soon); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ClaimAdminLink(ctx, u.ID, now.Add(2*time.Hour)); ok || err != nil {
		t.Fatalf("claim after the end: %v %v", ok, err)
	}
	if ok, _ := s.ClaimAdminLink(ctx, u.ID, now); !ok {
		t.Fatal("claim in time")
	}
	if ok, _ := s.ClaimAdminLink(ctx, u.ID, now); ok {
		t.Fatal("claim twice")
	}
	if err := s.AllowAdminLink(ctx, "usr_none", &soon); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("allow for missing user: %v", err)
	}

	id := &store.AdminIdentity{ID: "idn_1", UserID: u.ID, Provider: "sso", Issuer: "https://i", Subject: "s", Email: "a@example.com", CreatedAt: now}
	if err := s.CreateAdminIdentity(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAdminIdentity(ctx, &store.AdminIdentity{ID: "idn_2", UserID: u.ID, Issuer: "https://i", Subject: "s", CreatedAt: now}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("same issuer+subject: %v", err)
	}
	if got, err := s.GetAdminIdentity(ctx, "https://i", "s"); err != nil || got.UserID != u.ID || got.Provider != "sso" {
		t.Fatalf("get: %+v %v", got, err)
	}
	if err := s.DeleteAdminIdentity(ctx, "usr_other", id.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete another user's link: %v", err)
	}
	if err := s.DeleteAdminUser(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if list, err := s.ListAdminIdentities(ctx, u.ID); err != nil || len(list) != 0 {
		t.Fatalf("links after user delete: %v %v", list, err)
	}

	// Flows: taken once, expired ones refused.
	f := &store.OIDCFlow{IDHash: "h1", Provider: "sso", State: "st", Nonce: "n", PKCEVerifier: "v", ExpiresAt: now.Add(time.Minute)}
	if err := s.CreateOIDCFlow(ctx, f); err != nil {
		t.Fatal(err)
	}
	if got, err := s.TakeOIDCFlow(ctx, "h1", now); err != nil || got.State != "st" {
		t.Fatalf("take: %+v %v", got, err)
	}
	if _, err := s.TakeOIDCFlow(ctx, "h1", now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("take twice: %v", err)
	}
	_ = s.CreateOIDCFlow(ctx, &store.OIDCFlow{IDHash: "h2", Provider: "sso", ExpiresAt: now.Add(time.Minute)})
	if _, err := s.TakeOIDCFlow(ctx, "h2", now.Add(2*time.Minute)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("take expired: %v", err)
	}
}

// rollbackV10 is the manual roll back of migration v10 in change-0015 section 5. Keep the two
// in step.
const rollbackV10 = `
BEGIN;
ALTER TABLE catalog_providers DROP COLUMN prompt_cache;
DELETE FROM schema_migrations WHERE version = 10;
COMMIT;`

func TestRollbackV10(t *testing.T) {
	t.Parallel()
	url := pgtest.URL(t)
	ctx := context.Background()
	s, err := Open(ctx, url, Options{MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	cat := &store.Catalog{Providers: []store.CatalogProvider{{Name: "a", Type: "anthropic", OpenAIFormat: "native", PromptCache: "off",
		Headers: map[string]string{}, Accounts: []store.CatalogAccount{{Name: "default", KeyRef: "env:K"}}}}, Models: []store.CatalogModel{}}
	if _, err := s.UpdateCatalog(ctx, 0, "test", "seed", func(c *store.Catalog) error { *c = *cat; return nil }); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetCatalog(ctx); err != nil || got.Providers[0].PromptCache != "off" {
		t.Fatalf("prompt_cache not stored: %+v %v", got, err)
	}
	if _, err := s.pool.Exec(ctx, rollbackV10); err != nil {
		t.Fatalf("roll back: %v", err)
	}
	var v int
	if err := s.pool.QueryRow(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil || v != 9 {
		t.Fatalf("version after roll back: %d %v", v, err)
	}
	s.Close()
	// The provider is still there, and the migration runs again on the next start.
	s, err = Open(ctx, url, Options{MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.GetCatalog(ctx)
	if err != nil || len(got.Providers) != 1 || got.Providers[0].OpenAIFormat != "native" || got.Providers[0].PromptCache != "" {
		t.Fatalf("after migrating again: %+v %v", got, err)
	}
}

