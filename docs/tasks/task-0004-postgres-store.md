# task-0004: PostgreSQL store

- Status: todo
- Spec: [spec-0001](../specs/spec-0001-gateway-core-architecture.md) D5, D12
- Backlog: [backlog-0002](../backlogs/backlog-0002-postgres-store.md)
- Done when: `database.driver: postgres` passes the same store test suite as SQLite, and two gateway nodes share keys and usage through one database.

## 1. Scope

1. `gateway/internal/store/postgres` implementing `store.Store` with `pgx` (pure Go).
2. Same append-only migrations pattern, Postgres dialect (`timestamptz`, `jsonb`, `text[]` where useful).
3. Shared store test suite: one set of tests run against SQLite and Postgres (Postgres when `AIF_TEST_PG_URL` is set).
4. Config: `database: { driver: postgres, dsn: ${DATABASE_URL} }`. Pool size settings.
5. Dev loop: Postgres 18 is already in `deploy/local/compose.yaml` (`make services-up`, `DATABASE_URL` in `.env`). Tests use `AIF_TEST_PG_URL=$DATABASE_URL`.

## 2. Later

1. Time-partitioned `usage_records` (monthly) for large volumes.
2. Usage rollup table (per hour) for fast dashboards.
