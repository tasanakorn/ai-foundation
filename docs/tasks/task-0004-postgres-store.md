# task-0004: PostgreSQL store (only store; SQLite removed)

- Status: done
- Spec: [spec-0001](../specs/spec-0001-gateway-core-architecture.md) §4.2, D12, D15
- Backlog: [backlog-0002](../backlogs/backlog-0002-postgres-store.md)
- Change: [change-0005](../changes/change-0005-postgres-required-sqlite-removed.md)
- Done when: the gateway runs on PostgreSQL only, all tests pass on a real Postgres without Docker, and two gateway nodes share keys, usage and admin sessions through one database.

## 1. Scope

1. **Decision changed during the task:** the user chose PostgreSQL as the only store (D15). SQLite (`store/sqlite`, `modernc.org/sqlite`) is removed, so there is no second dialect and no shared two-driver suite.
2. `gateway/internal/store/postgres`: `store.Store` on `pgx/v5` (`pgxpool`, pure Go).
   1. Types: `timestamptz`, `text[]` (allowed models), `jsonb` (metadata), `boolean`, `bigint`, `double precision`.
   2. Usage insert: one `pgx.Batch` per flush, `ON CONFLICT (id) DO NOTHING` (a retried batch is stored once).
   3. `group_by=day` uses the UTC day.
   4. Migrations: append-only list, one transaction, `pg_advisory_xact_lock` so nodes starting together apply each one once. A schema newer than the binary is refused.
3. **Config:** `database: { url: ${DATABASE_URL}, max_conns: 10, min_conns: 0 }`. `url` is required. Old `driver: sqlite` / `dsn` give a clear error.
4. **Health:** `/healthz` also pings the database (`503` "database unreachable", no connection details on the public port).
5. **Tests:** `gateway/internal/pgtest`.
   1. Default: an embedded PostgreSQL 18 per test binary (`fergusstrange/embedded-postgres`, test-only). Binaries are downloaded once into `gateway/.cache/` (gitignored).
   2. `AIF_TEST_PG_URL` uses another server instead (for example the compose Postgres).
   3. Every test gets its own new database, dropped at the end, so tests run in parallel.
6. **Dev loop:** `make dev` already starts Postgres (`deploy/local/compose.yaml`); `aif.dev.yaml` points at it.

## 2. Code

| Path                                | What                                                     |
| ----------------------------------- | -------------------------------------------------------- |
| `gateway/internal/store/postgres/`  | Store, migrations, tests                                 |
| `gateway/internal/pgtest/`          | Test Postgres: embedded or `AIF_TEST_PG_URL` (test-only) |
| `gateway/internal/config/config.go` | `database.url`, pool sizes, removed-key error            |
| `gateway/internal/app/app.go`       | DB ping in health                                        |
| `gateway/cmd/aif/main.go`           | Opens Postgres before NATS                               |

## 3. Verification

1. `make -C gateway vet test` passes with `-race`, on embedded Postgres (no Docker).
   1. First run downloads and unpacks Postgres (~50 s under `-race`); later runs: whole suite ~15 s.
   2. Also passes with `AIF_TEST_PG_URL` = the compose Postgres 18. No test databases are left behind.
2. Store tests: keys round trip and filters, usage dedup, filters, paging, summaries (UTC day, all `group_by`), admin users and sessions, 8 concurrent `Open` → one migration run, newer schema refused.
3. Live (compose Postgres + NATS, fresh database):

   | Check                                      | Result                                                      |
   | ------------------------------------------ | ----------------------------------------------------------- |
   | Old SQLite config                          | exits: "SQLite is no longer supported; set database.url"    |
   | Start on empty database                    | migrations applied, bootstrap admin created                 |
   | Chat request through a fake upstream       | `200`; row in `usage_records`; summary by day correct       |
   | `docker compose stop postgres`, then start | `/healthz` 503 "database unreachable", 200 again after ~1 s |

## 4. Later

1. Time-partitioned `usage_records` (monthly) for large volumes.
2. Usage rollup table (per hour) for fast dashboards.
3. Retention job for old usage rows.
