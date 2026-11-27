// Package postgres is the Store, on PostgreSQL through pgx (pure Go).
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"ai-foundation/gateway/internal/store"
)

type Store struct {
	pool *pgxpool.Pool
}

var _ store.Store = (*Store)(nil)

// Options tune the connection pool. Zero values keep pgx defaults.
type Options struct {
	MaxConns int32
	MinConns int32
}

// Open connects, checks the server, and runs migrations.
// url is a postgres:// URL or a key=value DSN (see pgx docs).
func Open(ctx context.Context, url string, o Options) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("database.url: %w", err)
	}
	if o.MaxConns > 0 {
		cfg.MaxConns = o.MaxConns
	}
	if o.MinConns > 0 {
		cfg.MinConns = o.MinConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	s := &Store{pool: pool}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	s.pool.Close()
	return nil
}

// Ping checks the database connection (used by health checks).
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// migrations are append-only. Never edit a released entry; add a new one.
var migrations = []string{
	// v1: keys and usage
	`CREATE TABLE api_keys (
		id             text PRIMARY KEY,
		name           text NOT NULL DEFAULT '',
		prefix         text NOT NULL,
		hash           text NOT NULL UNIQUE,
		tenant_id      text NOT NULL DEFAULT '',
		app_id         text NOT NULL DEFAULT '',
		allowed_models text[] NOT NULL DEFAULT '{}',
		metadata       jsonb NOT NULL DEFAULT '{}',
		created_at     timestamptz NOT NULL,
		expires_at     timestamptz,
		revoked_at     timestamptz
	);
	CREATE INDEX idx_api_keys_owner ON api_keys (tenant_id, app_id);

	CREATE TABLE usage_records (
		id                text PRIMARY KEY,
		created_at        timestamptz NOT NULL,
		key_id            text NOT NULL DEFAULT '',
		tenant_id         text NOT NULL DEFAULT '',
		app_id            text NOT NULL DEFAULT '',
		end_user          text NOT NULL DEFAULT '',
		endpoint          text NOT NULL DEFAULT '',
		model             text NOT NULL DEFAULT '',
		provider          text NOT NULL DEFAULT '',
		upstream_model    text NOT NULL DEFAULT '',
		stream            boolean NOT NULL DEFAULT false,
		status_code       integer NOT NULL DEFAULT 0,
		prompt_tokens     bigint NOT NULL DEFAULT 0,
		completion_tokens bigint NOT NULL DEFAULT 0,
		total_tokens      bigint NOT NULL DEFAULT 0,
		cached_tokens     bigint NOT NULL DEFAULT 0,
		reasoning_tokens  bigint NOT NULL DEFAULT 0,
		cost_usd          double precision NOT NULL DEFAULT 0,
		estimated         boolean NOT NULL DEFAULT false,
		latency_ms        bigint NOT NULL DEFAULT 0,
		error             text NOT NULL DEFAULT ''
	);
	CREATE INDEX idx_usage_created ON usage_records (created_at);
	CREATE INDEX idx_usage_key ON usage_records (key_id, created_at);
	CREATE INDEX idx_usage_owner ON usage_records (tenant_id, app_id, created_at);`,

	// v2: built-in admin accounts and browser sessions
	`CREATE TABLE admin_users (
		id            text PRIMARY KEY,
		email         text NOT NULL UNIQUE,
		name          text NOT NULL DEFAULT '',
		role          text NOT NULL,
		password_hash text NOT NULL DEFAULT '',
		created_at    timestamptz NOT NULL,
		updated_at    timestamptz NOT NULL,
		last_login_at timestamptz,
		disabled_at   timestamptz
	);

	CREATE TABLE admin_sessions (
		hash       text PRIMARY KEY,
		user_id    text NOT NULL REFERENCES admin_users (id) ON DELETE CASCADE,
		created_at timestamptz NOT NULL,
		expires_at timestamptz NOT NULL,
		user_agent text NOT NULL DEFAULT '',
		ip         text NOT NULL DEFAULT ''
	);
	CREATE INDEX idx_admin_sessions_user ON admin_sessions (user_id);
	CREATE INDEX idx_admin_sessions_expires ON admin_sessions (expires_at);`,

	// v3: Anthropic API (spec-0002): cache writes, client API format, coding session
	`ALTER TABLE usage_records
		ADD COLUMN cache_write_tokens bigint NOT NULL DEFAULT 0,
		ADD COLUMN format             text   NOT NULL DEFAULT 'openai',
		ADD COLUMN session_id         text   NOT NULL DEFAULT '';`,

	// v4: the catalog in the database (spec-0006). One meta row holds the revision.
	`CREATE TABLE catalog_meta (
		id          integer PRIMARY KEY CHECK (id = 1),
		revision    bigint NOT NULL,
		passthrough boolean NOT NULL DEFAULT false,
		updated_at  timestamptz NOT NULL,
		updated_by  text NOT NULL DEFAULT ''
	);
	INSERT INTO catalog_meta (id, revision, updated_at) VALUES (1, 0, now());

	CREATE TABLE catalog_providers (
		name          text PRIMARY KEY,
		position      integer NOT NULL,
		type          text NOT NULL,
		base_url      text NOT NULL DEFAULT '',
		headers       jsonb NOT NULL DEFAULT '{}',
		workspace_id  text NOT NULL DEFAULT '',
		openai_format text NOT NULL DEFAULT ''
	);

	CREATE TABLE catalog_accounts (
		provider text NOT NULL REFERENCES catalog_providers (name) ON DELETE CASCADE,
		name     text NOT NULL,
		position integer NOT NULL,
		key_ref  text NOT NULL DEFAULT '',
		PRIMARY KEY (provider, name)
	);

	CREATE TABLE catalog_models (
		name     text PRIMARY KEY,
		position integer NOT NULL,
		formats  text[] NOT NULL DEFAULT '{}'
	);

	CREATE TABLE catalog_targets (
		model    text NOT NULL REFERENCES catalog_models (name) ON DELETE CASCADE,
		position integer NOT NULL,
		provider text NOT NULL REFERENCES catalog_providers (name),
		upstream text NOT NULL,
		price    jsonb,
		PRIMARY KEY (model, position)
	);

	CREATE TABLE catalog_changes (
		revision   bigint PRIMARY KEY,
		created_at timestamptz NOT NULL,
		actor      text NOT NULL,
		summary    text NOT NULL DEFAULT ''
	);`,

	// v5: encrypted provider keys (spec-0006 section 4.4). key_ref or key_cipher, not both.
	`ALTER TABLE catalog_accounts
		ADD COLUMN key_cipher bytea,
		ADD COLUMN key_id     text NOT NULL DEFAULT '',
		ADD COLUMN key_hint   text NOT NULL DEFAULT '';`,

	// v6: several accounts per provider (spec-0004): policy, per-account settings,
	// and the account and attempts of each request.
	`ALTER TABLE catalog_providers
		ADD COLUMN policy       text    NOT NULL DEFAULT '',
		ADD COLUMN sticky       text    NOT NULL DEFAULT '',
		ADD COLUMN max_attempts integer NOT NULL DEFAULT 0,
		ADD COLUMN cooldown_ms  bigint  NOT NULL DEFAULT 0;
	ALTER TABLE catalog_accounts
		ADD COLUMN weight       integer,
		ADD COLUMN base_url     text  NOT NULL DEFAULT '',
		ADD COLUMN headers      jsonb NOT NULL DEFAULT '{}',
		ADD COLUMN workspace_id text  NOT NULL DEFAULT '';
	ALTER TABLE usage_records
		ADD COLUMN account  text    NOT NULL DEFAULT '',
		ADD COLUMN attempts integer NOT NULL DEFAULT 1;`,

	// v7: model aliases with several targets (spec-0005).
	`ALTER TABLE catalog_models
		ADD COLUMN policy       text    NOT NULL DEFAULT '',
		ADD COLUMN sticky       text    NOT NULL DEFAULT '',
		ADD COLUMN max_attempts integer NOT NULL DEFAULT 0;
	ALTER TABLE catalog_targets
		ADD COLUMN weight  integer,
		ADD COLUMN formats text[] NOT NULL DEFAULT '{}';`,

	// v8: credit per account (task-0010): its type and cycle in the catalog, the
	// prepaid ledger, and an index for spend per account.
	`ALTER TABLE catalog_accounts
		ADD COLUMN credit_type      text             NOT NULL DEFAULT '',
		ADD COLUMN credit_usd       double precision NOT NULL DEFAULT 0,
		ADD COLUMN credit_cycle_day integer          NOT NULL DEFAULT 0,
		ADD COLUMN credit_timezone  text             NOT NULL DEFAULT '';
	CREATE TABLE credit_ledger (
		id         text PRIMARY KEY,
		provider   text NOT NULL,
		account    text NOT NULL,
		usd        double precision NOT NULL,
		note       text NOT NULL DEFAULT '',
		at         timestamptz NOT NULL,
		created_at timestamptz NOT NULL,
		actor      text NOT NULL
	);
	CREATE INDEX idx_credit_ledger_account ON credit_ledger (provider, account, at);
	CREATE INDEX idx_usage_account ON usage_records (provider, account, created_at);`,
}

// migrationLock is a constant key for pg_advisory_xact_lock. Several nodes may start
// at once; the lock makes them apply each migration exactly once.
const migrationLock = 0x61696600 // "aif\0"

func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLock); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version integer PRIMARY KEY, applied_at timestamptz NOT NULL)`); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	var current int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if current > len(migrations) {
		return fmt.Errorf("migrate: database schema v%d is newer than this binary (v%d); upgrade aif", current, len(migrations))
	}
	for v := current + 1; v <= len(migrations); v++ {
		if _, err := tx.Exec(ctx, migrations[v-1]); err != nil {
			return fmt.Errorf("migrate v%d: %w", v, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES ($1, now())`, v); err != nil {
			return fmt.Errorf("migrate v%d: %w", v, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// ---- keys ----

const keyColumns = `id, name, prefix, hash, tenant_id, app_id, allowed_models, metadata, created_at, expires_at, revoked_at`

func (s *Store) CreateKey(ctx context.Context, k *store.Key) error {
	meta, _ := json.Marshal(nonNilMap(k.Metadata))
	_, err := s.pool.Exec(ctx,
		`INSERT INTO api_keys (`+keyColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		k.ID, k.Name, k.Prefix, k.Hash, k.TenantID, k.AppID, nonNil(k.AllowedModels), meta,
		k.CreatedAt, k.ExpiresAt, k.RevokedAt)
	return mapErr(err)
}

func (s *Store) GetKey(ctx context.Context, id string) (*store.Key, error) {
	return scanKey(s.pool.QueryRow(ctx, `SELECT `+keyColumns+` FROM api_keys WHERE id = $1`, id))
}

func (s *Store) GetKeyByHash(ctx context.Context, hash string) (*store.Key, error) {
	return scanKey(s.pool.QueryRow(ctx, `SELECT `+keyColumns+` FROM api_keys WHERE hash = $1`, hash))
}

func (s *Store) ListKeys(ctx context.Context, f store.KeyFilter) ([]store.Key, error) {
	var w where
	if f.TenantID != "" {
		w.add("tenant_id = ?", f.TenantID)
	}
	if f.AppID != "" {
		w.add("app_id = ?", f.AppID)
	}
	if !f.IncludeRevoked {
		w.add("revoked_at IS NULL")
	}
	q := `SELECT ` + keyColumns + ` FROM api_keys` + w.sql() + ` ORDER BY created_at DESC, id DESC` + w.page(f.Limit, f.Offset)
	rows, err := s.pool.Query(ctx, q, w.args...)
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
	tag, err := s.pool.Exec(ctx, `UPDATE api_keys SET revoked_at = COALESCE(revoked_at, $1) WHERE id = $2`, at, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func scanKey(row pgx.Row) (*store.Key, error) {
	var k store.Key
	var meta []byte
	err := row.Scan(&k.ID, &k.Name, &k.Prefix, &k.Hash, &k.TenantID, &k.AppID, &k.AllowedModels, &meta,
		&k.CreatedAt, &k.ExpiresAt, &k.RevokedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	_ = json.Unmarshal(meta, &k.Metadata)
	k.AllowedModels = nonNil(k.AllowedModels)
	k.Metadata = nonNilMap(k.Metadata)
	k.CreatedAt = k.CreatedAt.UTC()
	k.ExpiresAt = utcPtr(k.ExpiresAt)
	k.RevokedAt = utcPtr(k.RevokedAt)
	return &k, nil
}

// ---- usage ----

const usageColumns = `id, created_at, key_id, tenant_id, app_id, end_user, endpoint, model, provider, upstream_model,
	stream, status_code, prompt_tokens, completion_tokens, total_tokens, cached_tokens, reasoning_tokens,
	cost_usd, estimated, latency_ms, error, cache_write_tokens, format, session_id, account, attempts`

// InsertUsage writes all records in one round trip. Records with a known ID are skipped,
// so a retried batch never counts a request twice.
func (s *Store) InsertUsage(ctx context.Context, recs []store.UsageRecord) error {
	if len(recs) == 0 {
		return nil
	}
	const q = `INSERT INTO usage_records (` + usageColumns + `)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26)
		ON CONFLICT (id) DO NOTHING`
	b := &pgx.Batch{}
	for _, r := range recs {
		b.Queue(q, r.ID, r.CreatedAt, r.KeyID, r.TenantID, r.AppID, r.EndUser,
			r.Endpoint, r.Model, r.Provider, r.UpstreamModel, r.Stream, r.StatusCode,
			r.PromptTokens, r.CompletionTokens, r.TotalTokens, r.CachedTokens, r.ReasoningTokens,
			r.CostUSD, r.Estimated, r.LatencyMS, r.Error, r.CacheWriteTokens, r.Format, r.SessionID, r.Account, r.Attempts)
	}
	// A batch outside a transaction runs as one implicit transaction: all or nothing.
	return s.pool.SendBatch(ctx, b).Close()
}

func usageWhere(f store.UsageFilter) *where {
	w := &where{}
	if !f.From.IsZero() {
		w.add("created_at >= ?", f.From)
	}
	if !f.To.IsZero() {
		w.add("created_at < ?", f.To)
	}
	if f.ID != "" {
		w.add("id = ?", f.ID)
	}
	if f.KeyID != "" {
		w.add("key_id = ?", f.KeyID)
	}
	if f.TenantID != "" {
		w.add("tenant_id = ?", f.TenantID)
	}
	if f.AppID != "" {
		w.add("app_id = ?", f.AppID)
	}
	if f.EndUser != "" {
		w.add("end_user = ?", f.EndUser)
	}
	if f.Model != "" {
		w.add("model = ?", f.Model)
	}
	if f.Provider != "" {
		w.add("provider = ?", f.Provider)
	}
	if f.Account != "" {
		w.add("account = ?", f.Account)
	}
	return w
}

func (s *Store) ListUsage(ctx context.Context, f store.UsageFilter) ([]store.UsageRecord, error) {
	w := usageWhere(f)
	q := `SELECT ` + usageColumns + ` FROM usage_records` + w.sql() + ` ORDER BY created_at DESC, id DESC` + w.page(f.Limit, f.Offset)
	rows, err := s.pool.Query(ctx, q, w.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []store.UsageRecord{}
	for rows.Next() {
		var r store.UsageRecord
		if err := rows.Scan(&r.ID, &r.CreatedAt, &r.KeyID, &r.TenantID, &r.AppID, &r.EndUser, &r.Endpoint, &r.Model,
			&r.Provider, &r.UpstreamModel, &r.Stream, &r.StatusCode, &r.PromptTokens, &r.CompletionTokens,
			&r.TotalTokens, &r.CachedTokens, &r.ReasoningTokens, &r.CostUSD, &r.Estimated, &r.LatencyMS, &r.Error,
			&r.CacheWriteTokens, &r.Format, &r.SessionID, &r.Account, &r.Attempts); err != nil {
			return nil, err
		}
		r.CreatedAt = r.CreatedAt.UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

var groupExpr = map[string]string{
	"none":     "''",
	"day":      "to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD')",
	"model":    "model",
	"provider": "provider",
	"account":  "provider || '/' || account",
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
	w := usageWhere(f)
	q := `SELECT ` + expr + ` AS g, COUNT(*),
		COUNT(*) FILTER (WHERE status_code >= 400),
		COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0), COALESCE(SUM(total_tokens), 0),
		COALESCE(SUM(cost_usd), 0)
		FROM usage_records` + w.sql() + ` GROUP BY g ORDER BY g`
	rows, err := s.pool.Query(ctx, q, w.args...)
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

// ---- admin users ----

const userColumns = `id, email, name, role, password_hash, created_at, updated_at, last_login_at, disabled_at`

func (s *Store) CreateAdminUser(ctx context.Context, u *store.AdminUser) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO admin_users (`+userColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		u.ID, u.Email, u.Name, u.Role, u.PasswordHash, u.CreatedAt, u.UpdatedAt, u.LastLoginAt, u.DisabledAt)
	return mapErr(err)
}

func (s *Store) GetAdminUser(ctx context.Context, id string) (*store.AdminUser, error) {
	return scanUser(s.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM admin_users WHERE id = $1`, id))
}

func (s *Store) GetAdminUserByEmail(ctx context.Context, email string) (*store.AdminUser, error) {
	return scanUser(s.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM admin_users WHERE email = $1`, email))
}

func (s *Store) ListAdminUsers(ctx context.Context) ([]store.AdminUser, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+userColumns+` FROM admin_users ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []store.AdminUser{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

func (s *Store) UpdateAdminUser(ctx context.Context, u *store.AdminUser) error {
	tag, err := s.pool.Exec(ctx, `UPDATE admin_users SET name = $2, role = $3, password_hash = $4, disabled_at = $5, updated_at = $6 WHERE id = $1`,
		u.ID, u.Name, u.Role, u.PasswordHash, u.DisabledAt, u.UpdatedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) DeleteAdminUser(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM admin_users WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) CountActiveAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM admin_users WHERE role = $1 AND disabled_at IS NULL`, store.RoleAdmin).Scan(&n)
	return n, err
}

func (s *Store) SetAdminLastLogin(ctx context.Context, id string, at time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE admin_users SET last_login_at = $2 WHERE id = $1`, id, at)
	return err
}

func scanUser(row pgx.Row) (*store.AdminUser, error) {
	var u store.AdminUser
	if err := row.Scan(&u.ID, &u.Email, &u.Name, &u.Role, &u.PasswordHash, &u.CreatedAt, &u.UpdatedAt, &u.LastLoginAt, &u.DisabledAt); err != nil {
		return nil, mapErr(err)
	}
	u.CreatedAt, u.UpdatedAt = u.CreatedAt.UTC(), u.UpdatedAt.UTC()
	u.LastLoginAt, u.DisabledAt = utcPtr(u.LastLoginAt), utcPtr(u.DisabledAt)
	return &u, nil
}

// ---- admin sessions ----

func (s *Store) CreateAdminSession(ctx context.Context, a *store.AdminSession) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO admin_sessions (hash, user_id, created_at, expires_at, user_agent, ip) VALUES ($1, $2, $3, $4, $5, $6)`,
		a.Hash, a.UserID, a.CreatedAt, a.ExpiresAt, a.UserAgent, a.IP)
	return mapErr(err)
}

func (s *Store) GetAdminSession(ctx context.Context, hash string) (*store.AdminSession, error) {
	var a store.AdminSession
	err := s.pool.QueryRow(ctx, `SELECT hash, user_id, created_at, expires_at, user_agent, ip FROM admin_sessions WHERE hash = $1 AND expires_at > now()`, hash).
		Scan(&a.Hash, &a.UserID, &a.CreatedAt, &a.ExpiresAt, &a.UserAgent, &a.IP)
	if err != nil {
		return nil, mapErr(err)
	}
	a.CreatedAt, a.ExpiresAt = a.CreatedAt.UTC(), a.ExpiresAt.UTC()
	return &a, nil
}

func (s *Store) DeleteAdminSession(ctx context.Context, hash string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM admin_sessions WHERE hash = $1`, hash)
	return err
}

func (s *Store) DeleteAdminSessions(ctx context.Context, userID, exceptHash string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM admin_sessions WHERE user_id = $1 AND hash <> $2`, userID, exceptHash)
	return err
}

func (s *Store) DeleteExpiredAdminSessions(ctx context.Context, now time.Time) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM admin_sessions WHERE expires_at <= $1`, now)
	return err
}

// ---- helpers ----

// where builds a WHERE clause. Conditions use "?"; they become $1, $2, ... in order.
type where struct {
	conds []string
	args  []any
}

func (w *where) add(cond string, args ...any) {
	for _, a := range args {
		w.args = append(w.args, a)
		cond = strings.Replace(cond, "?", "$"+strconv.Itoa(len(w.args)), 1)
	}
	w.conds = append(w.conds, cond)
}

func (w *where) sql() string {
	if len(w.conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(w.conds, " AND ")
}

// page appends LIMIT/OFFSET as parameters. Call it after the last add.
func (w *where) page(limit, offset int) string {
	switch {
	case limit <= 0:
		limit = 100
	case limit > 1000:
		limit = 1000
	}
	w.args = append(w.args, limit, max(offset, 0))
	n := len(w.args)
	return " LIMIT $" + strconv.Itoa(n-1) + " OFFSET $" + strconv.Itoa(n)
}

func mapErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ErrNotFound
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" { // unique_violation
		return store.ErrConflict
	}
	return err
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
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
