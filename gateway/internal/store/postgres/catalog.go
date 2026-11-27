package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"ai-foundation/gateway/internal/store"
)

// querier is what both the pool and a transaction offer.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *Store) GetCatalog(ctx context.Context) (*store.Catalog, error) {
	// Repeatable read: the five selects see one committed state, never half a change.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	return readCatalog(ctx, tx, false)
}

func (s *Store) CatalogRevision(ctx context.Context) (int64, error) {
	var rev int64
	err := s.pool.QueryRow(ctx, `SELECT revision FROM catalog_meta WHERE id = 1`).Scan(&rev)
	return rev, mapErr(err)
}

func (s *Store) UpdateCatalog(ctx context.Context, ifRevision int64, actor, summary string, fn func(*store.Catalog) error) (*store.Catalog, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	c, err := readCatalog(ctx, tx, true) // FOR UPDATE on the meta row: writers queue here
	if err != nil {
		return nil, err
	}
	if ifRevision != store.AnyRevision && ifRevision != c.Revision {
		return nil, fmt.Errorf("%w: based on %d, current is %d", store.ErrStale, ifRevision, c.Revision)
	}
	if err := fn(c); err != nil {
		return nil, err
	}
	c.Revision++
	c.UpdatedAt, c.UpdatedBy = time.Now().UTC(), actor
	if err := writeCatalog(ctx, tx, c); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO catalog_changes (revision, created_at, actor, summary) VALUES ($1, $2, $3, $4)`,
		c.Revision, c.UpdatedAt, actor, summary); err != nil {
		return nil, fmt.Errorf("catalog change row: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (s *Store) ListCatalogChanges(ctx context.Context, limit int) ([]store.CatalogChange, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `SELECT revision, created_at, actor, summary FROM catalog_changes ORDER BY revision DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []store.CatalogChange{}
	for rows.Next() {
		var ch store.CatalogChange
		if err := rows.Scan(&ch.Revision, &ch.CreatedAt, &ch.Actor, &ch.Summary); err != nil {
			return nil, err
		}
		ch.CreatedAt = ch.CreatedAt.UTC()
		out = append(out, ch)
	}
	return out, rows.Err()
}

func readCatalog(ctx context.Context, q querier, lock bool) (*store.Catalog, error) {
	c := &store.Catalog{Providers: []store.CatalogProvider{}, Models: []store.CatalogModel{}}
	sql := `SELECT revision, passthrough, updated_at, updated_by FROM catalog_meta WHERE id = 1`
	if lock {
		sql += ` FOR UPDATE`
	}
	if err := q.QueryRow(ctx, sql).Scan(&c.Revision, &c.Passthrough, &c.UpdatedAt, &c.UpdatedBy); err != nil {
		return nil, fmt.Errorf("catalog meta: %w", err)
	}
	c.UpdatedAt = c.UpdatedAt.UTC()

	rows, err := q.Query(ctx, `SELECT name, type, base_url, headers, workspace_id, openai_format FROM catalog_providers ORDER BY position`)
	if err != nil {
		return nil, err
	}
	byName := map[string]int{}
	for rows.Next() {
		var p store.CatalogProvider
		var headers []byte
		if err := rows.Scan(&p.Name, &p.Type, &p.BaseURL, &headers, &p.WorkspaceID, &p.OpenAIFormat); err != nil {
			rows.Close()
			return nil, err
		}
		if err := json.Unmarshal(headers, &p.Headers); err != nil {
			rows.Close()
			return nil, fmt.Errorf("provider %q headers: %w", p.Name, err)
		}
		p.Accounts = []store.CatalogAccount{}
		byName[p.Name] = len(c.Providers)
		c.Providers = append(c.Providers, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = q.Query(ctx, `SELECT provider, name, key_ref FROM catalog_accounts ORDER BY provider, position`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var prov string
		var a store.CatalogAccount
		if err := rows.Scan(&prov, &a.Name, &a.KeyRef); err != nil {
			rows.Close()
			return nil, err
		}
		if i, ok := byName[prov]; ok {
			c.Providers[i].Accounts = append(c.Providers[i].Accounts, a)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = q.Query(ctx, `SELECT name, formats FROM catalog_models ORDER BY position`)
	if err != nil {
		return nil, err
	}
	modelIdx := map[string]int{}
	for rows.Next() {
		var m store.CatalogModel
		if err := rows.Scan(&m.Name, &m.Formats); err != nil {
			rows.Close()
			return nil, err
		}
		m.Targets = []store.CatalogTarget{}
		modelIdx[m.Name] = len(c.Models)
		c.Models = append(c.Models, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = q.Query(ctx, `SELECT model, provider, upstream, price FROM catalog_targets ORDER BY model, position`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var model string
		var t store.CatalogTarget
		var price []byte
		if err := rows.Scan(&model, &t.Provider, &t.Upstream, &price); err != nil {
			return nil, err
		}
		if price != nil {
			t.Price = &store.Price{}
			if err := json.Unmarshal(price, t.Price); err != nil {
				return nil, fmt.Errorf("model %q price: %w", model, err)
			}
		}
		if i, ok := modelIdx[model]; ok {
			c.Models[i].Targets = append(c.Models[i].Targets, t)
		}
	}
	return c, rows.Err()
}

// writeCatalog replaces every catalog row. The catalog is small (tens of rows), so a
// full rewrite keeps one simple code path for every kind of change.
func writeCatalog(ctx context.Context, tx pgx.Tx, c *store.Catalog) error {
	for _, t := range []string{"catalog_targets", "catalog_models", "catalog_accounts", "catalog_providers"} {
		if _, err := tx.Exec(ctx, `DELETE FROM `+t); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE catalog_meta SET revision = $1, passthrough = $2, updated_at = $3, updated_by = $4 WHERE id = 1`,
		c.Revision, c.Passthrough, c.UpdatedAt, c.UpdatedBy); err != nil {
		return err
	}
	b := &pgx.Batch{}
	for i, p := range c.Providers {
		headers, _ := json.Marshal(nonNilMap(p.Headers))
		b.Queue(`INSERT INTO catalog_providers (name, position, type, base_url, headers, workspace_id, openai_format) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			p.Name, i, p.Type, p.BaseURL, headers, p.WorkspaceID, p.OpenAIFormat)
		for j, a := range p.Accounts {
			b.Queue(`INSERT INTO catalog_accounts (provider, name, position, key_ref) VALUES ($1, $2, $3, $4)`, p.Name, a.Name, j, a.KeyRef)
		}
	}
	for i, m := range c.Models {
		b.Queue(`INSERT INTO catalog_models (name, position, formats) VALUES ($1, $2, $3)`, m.Name, i, nonNil(m.Formats))
		for j, t := range m.Targets {
			var price []byte
			if t.Price != nil {
				price, _ = json.Marshal(t.Price)
			}
			b.Queue(`INSERT INTO catalog_targets (model, position, provider, upstream, price) VALUES ($1, $2, $3, $4, $5)`,
				m.Name, j, t.Provider, t.Upstream, price)
		}
	}
	if err := tx.SendBatch(ctx, b).Close(); err != nil {
		return fmt.Errorf("write catalog: %w", mapErr(err))
	}
	return nil
}
