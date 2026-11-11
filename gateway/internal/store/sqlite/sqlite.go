// Package sqlite is the default Store. Pure Go (no CGO), single file, WAL mode.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"ai-foundation/gateway/internal/store"
)

type Store struct {
	db *sql.DB
}

var _ store.Store = (*Store)(nil)

// Open opens (or creates) the database and runs migrations.
// dsn is a file path ("aif.db"), ":memory:", or a full "file:" URI.
func Open(dsn string) (*Store, error) {
	if !strings.HasPrefix(dsn, "file:") {
		dsn = "file:" + dsn
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	// busy_timeout first: switching to WAL needs a lock another process may hold.
	dsn += sep + "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite allows one writer at a time. A small pool keeps memory low and avoids lock churn.
	db.SetMaxOpenConns(4)
	s := &Store{db: db}
	// A brand-new file switches to WAL on first use. That switch can fail with SQLITE_BUSY
	// at once (busy_timeout does not cover it) when another process opens the file at the
	// same moment, so retry for a short while.
	deadline := time.Now().Add(10 * time.Second)
	for {
		err = s.migrate(context.Background())
		if err == nil || !strings.Contains(err.Error(), "SQLITE_BUSY") || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// migrations are append-only. Never edit a released entry; add a new one.
var migrations = []string{
	`CREATE TABLE api_keys (
		id             TEXT PRIMARY KEY,
		name           TEXT NOT NULL DEFAULT '',
		prefix         TEXT NOT NULL,
		hash           TEXT NOT NULL UNIQUE,
		tenant_id      TEXT NOT NULL DEFAULT '',
		app_id         TEXT NOT NULL DEFAULT '',
		allowed_models TEXT NOT NULL DEFAULT '[]',
		metadata       TEXT NOT NULL DEFAULT '{}',
		created_at     INTEGER NOT NULL,
		expires_at     INTEGER,
		revoked_at     INTEGER
	);
	CREATE INDEX idx_api_keys_owner ON api_keys(tenant_id, app_id);

	CREATE TABLE usage_records (
		id                TEXT PRIMARY KEY,
		created_at        INTEGER NOT NULL,
		key_id            TEXT NOT NULL DEFAULT '',
		tenant_id         TEXT NOT NULL DEFAULT '',
		app_id            TEXT NOT NULL DEFAULT '',
		end_user          TEXT NOT NULL DEFAULT '',
		endpoint          TEXT NOT NULL DEFAULT '',
		model             TEXT NOT NULL DEFAULT '',
		provider          TEXT NOT NULL DEFAULT '',
		upstream_model    TEXT NOT NULL DEFAULT '',
		stream            INTEGER NOT NULL DEFAULT 0,
		status_code       INTEGER NOT NULL DEFAULT 0,
		prompt_tokens     INTEGER NOT NULL DEFAULT 0,
		completion_tokens INTEGER NOT NULL DEFAULT 0,
		total_tokens      INTEGER NOT NULL DEFAULT 0,
		cached_tokens     INTEGER NOT NULL DEFAULT 0,
		reasoning_tokens  INTEGER NOT NULL DEFAULT 0,
		cost_usd          REAL NOT NULL DEFAULT 0,
		estimated         INTEGER NOT NULL DEFAULT 0,
		latency_ms        INTEGER NOT NULL DEFAULT 0,
		error             TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX idx_usage_created ON usage_records(created_at);
	CREATE INDEX idx_usage_key ON usage_records(key_id, created_at);
	CREATE INDEX idx_usage_owner ON usage_records(tenant_id, app_id, created_at);`,
}

// migrate applies new migrations in one write transaction. BEGIN IMMEDIATE takes the
// write lock first (waiting up to busy_timeout), so several processes starting at
// once on the same file apply each migration exactly once.
func (s *Store) migrate(ctx context.Context) (err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	defer func() {
		if err != nil {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	var current int
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	for v := current + 1; v <= len(migrations); v++ {
		if _, err := conn.ExecContext(ctx, migrations[v-1]); err != nil {
			return fmt.Errorf("migrate v%d: %w", v, err)
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)`, v, time.Now().UnixMilli()); err != nil {
			return fmt.Errorf("migrate v%d: %w", v, err)
		}
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// ---- keys ----

const keyColumns = `id, name, prefix, hash, tenant_id, app_id, allowed_models, metadata, created_at, expires_at, revoked_at`

func (s *Store) CreateKey(ctx context.Context, k *store.Key) error {
	models, _ := json.Marshal(nonNil(k.AllowedModels))
	meta, _ := json.Marshal(nonNilMap(k.Metadata))
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO api_keys (`+keyColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		k.ID, k.Name, k.Prefix, k.Hash, k.TenantID, k.AppID, string(models), string(meta),
		k.CreatedAt.UnixMilli(), millisPtr(k.ExpiresAt), millisPtr(k.RevokedAt))
	return err
}

func (s *Store) GetKey(ctx context.Context, id string) (*store.Key, error) {
	return scanKey(s.db.QueryRowContext(ctx, `SELECT `+keyColumns+` FROM api_keys WHERE id = ?`, id))
}

func (s *Store) GetKeyByHash(ctx context.Context, hash string) (*store.Key, error) {
	return scanKey(s.db.QueryRowContext(ctx, `SELECT `+keyColumns+` FROM api_keys WHERE hash = ?`, hash))
}

func (s *Store) ListKeys(ctx context.Context, f store.KeyFilter) ([]store.Key, error) {
	var where []string
	var args []any
	if f.TenantID != "" {
		where, args = append(where, "tenant_id = ?"), append(args, f.TenantID)
	}
	if f.AppID != "" {
		where, args = append(where, "app_id = ?"), append(args, f.AppID)
	}
	if !f.IncludeRevoked {
		where = append(where, "revoked_at IS NULL")
	}
	q := `SELECT ` + keyColumns + ` FROM api_keys` + joinWhere(where) + ` ORDER BY created_at DESC LIMIT ? OFFSET ?`
	args = append(args, limit(f.Limit), f.Offset)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []store.Key{}
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *k)
	}
	return out, rows.Err()
}

func (s *Store) RevokeKey(ctx context.Context, id string, at time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE api_keys SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ?`, at.UnixMilli(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

type scanner interface{ Scan(dest ...any) error }

func scanKey(row scanner) (*store.Key, error) {
	var k store.Key
	var models, meta string
	var created int64
	var expires, revoked sql.NullInt64
	err := row.Scan(&k.ID, &k.Name, &k.Prefix, &k.Hash, &k.TenantID, &k.AppID, &models, &meta, &created, &expires, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(models), &k.AllowedModels)
	_ = json.Unmarshal([]byte(meta), &k.Metadata)
	k.AllowedModels = nonNil(k.AllowedModels)
	k.Metadata = nonNilMap(k.Metadata)
	k.CreatedAt = time.UnixMilli(created).UTC()
	k.ExpiresAt = timePtr(expires)
	k.RevokedAt = timePtr(revoked)
	return &k, nil
}

// ---- usage ----

const usageColumns = `id, created_at, key_id, tenant_id, app_id, end_user, endpoint, model, provider, upstream_model,
	stream, status_code, prompt_tokens, completion_tokens, total_tokens, cached_tokens, reasoning_tokens,
	cost_usd, estimated, latency_ms, error`

func (s *Store) InsertUsage(ctx context.Context, recs []store.UsageRecord) error {
	if len(recs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO usage_records (`+usageColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, r := range recs {
		if _, err := stmt.ExecContext(ctx, r.ID, r.CreatedAt.UnixMilli(), r.KeyID, r.TenantID, r.AppID, r.EndUser,
			r.Endpoint, r.Model, r.Provider, r.UpstreamModel, r.Stream, r.StatusCode,
			r.PromptTokens, r.CompletionTokens, r.TotalTokens, r.CachedTokens, r.ReasoningTokens,
			r.CostUSD, r.Estimated, r.LatencyMS, r.Error); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func usageWhere(f store.UsageFilter) (string, []any) {
	var where []string
	var args []any
	add := func(cond string, v any) { where, args = append(where, cond), append(args, v) }
	if !f.From.IsZero() {
		add("created_at >= ?", f.From.UnixMilli())
	}
	if !f.To.IsZero() {
		add("created_at < ?", f.To.UnixMilli())
	}
	if f.KeyID != "" {
		add("key_id = ?", f.KeyID)
	}
	if f.TenantID != "" {
		add("tenant_id = ?", f.TenantID)
	}
	if f.AppID != "" {
		add("app_id = ?", f.AppID)
	}
	if f.EndUser != "" {
		add("end_user = ?", f.EndUser)
	}
	if f.Model != "" {
		add("model = ?", f.Model)
	}
	return joinWhere(where), args
}

func (s *Store) ListUsage(ctx context.Context, f store.UsageFilter) ([]store.UsageRecord, error) {
	where, args := usageWhere(f)
	args = append(args, limit(f.Limit), f.Offset)
	rows, err := s.db.QueryContext(ctx, `SELECT `+usageColumns+` FROM usage_records`+where+` ORDER BY created_at DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []store.UsageRecord{}
	for rows.Next() {
		var r store.UsageRecord
		var created int64
		if err := rows.Scan(&r.ID, &created, &r.KeyID, &r.TenantID, &r.AppID, &r.EndUser, &r.Endpoint, &r.Model,
			&r.Provider, &r.UpstreamModel, &r.Stream, &r.StatusCode, &r.PromptTokens, &r.CompletionTokens,
			&r.TotalTokens, &r.CachedTokens, &r.ReasoningTokens, &r.CostUSD, &r.Estimated, &r.LatencyMS, &r.Error); err != nil {
			return nil, err
		}
		r.CreatedAt = time.UnixMilli(created).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

var groupExpr = map[string]string{
	"none":     "''",
	"day":      "strftime('%Y-%m-%d', created_at / 1000, 'unixepoch')",
	"model":    "model",
	"provider": "provider",
	"key":      "key_id",
	"tenant":   "tenant_id",
	"app":      "app_id",
	"end_user": "end_user",
}

func (s *Store) SummarizeUsage(ctx context.Context, f store.UsageFilter, groupBy string) ([]store.UsageSummary, error) {
	expr, ok := groupExpr[groupBy]
	if !ok {
		return nil, fmt.Errorf("unknown group_by %q", groupBy)
	}
	where, args := usageWhere(f)
	q := `SELECT ` + expr + ` AS g, COUNT(*),
		SUM(CASE WHEN status_code >= 400 THEN 1 ELSE 0 END),
		COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0), COALESCE(SUM(total_tokens), 0),
		COALESCE(SUM(cost_usd), 0)
		FROM usage_records` + where + ` GROUP BY g ORDER BY g`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []store.UsageSummary{}
	for rows.Next() {
		var r store.UsageSummary
		if err := rows.Scan(&r.Group, &r.Requests, &r.Errors, &r.PromptTokens, &r.CompletionTokens, &r.TotalTokens, &r.CostUSD); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- helpers ----

func joinWhere(where []string) string {
	if len(where) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(where, " AND ")
}

func limit(n int) int {
	switch {
	case n <= 0:
		return 100
	case n > 1000:
		return 1000
	}
	return n
}

func millisPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}

func timePtr(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.UnixMilli(v.Int64).UTC()
	return &t
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
