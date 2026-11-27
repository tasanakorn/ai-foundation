# change-0005: PostgreSQL required, SQLite removed

- Status: applied
- Date: 2026-11-11
- Related: [spec-0001](../specs/spec-0001-gateway-core-architecture.md) §4.2, D5, D12, D15; [task-0004](../tasks/task-0004-postgres-store.md)

## 1. What changed

1. **PostgreSQL is the only store.** `store/sqlite` and the `modernc.org/sqlite` dependency are gone. New `store/postgres` on `pgx/v5`.
2. Config: `database.url` (required, `DATABASE_URL`), `database.max_conns` (10), `database.min_conns`. `database.driver` and `database.dsn` are removed; old configs fail with a clear message.
3. Startup opens Postgres and runs migrations (advisory lock) before connecting to NATS.
4. `/healthz` answers `503` "database unreachable" when Postgres is down.
5. Tests run on a real Postgres: embedded per test binary (downloaded once into `gateway/.cache/`), or `AIF_TEST_PG_URL`. Tests run in parallel, each on its own database.
6. `make dev` sets no `AIF_DB` any more; the dev config uses the compose Postgres.

## 2. Why

1. The user chose PostgreSQL as default and asked to drop SQLite (D15).
2. One code path and one SQL dialect, like D14 for NATS. NATS already made aif a multi-process setup, so SQLite's "zero setup" no longer held.
3. Multi-node setups share keys, usage and admin sessions by default. The SQLite file-lock workarounds are gone.

## 3. Impact

1. **Breaking:** a gateway needs PostgreSQL. Existing SQLite data is not migrated (no production installs existed). To keep old data, export it with `sqlite3` and import it by hand.
2. Times are now `timestamptz` (microseconds) instead of Unix milliseconds. The JSON API is unchanged.
3. Binary size is about the same (18.7 MB).

## 4. Roll back

1. Revert this change and change-0006 together (the admin tables exist only in the Postgres store).
