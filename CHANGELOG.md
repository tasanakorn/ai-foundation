# Changelog

All notable changes. Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Versions: [Semantic Versioning](https://semver.org/); `0.x` while the API may still change.

There is no prebuilt image or binary: build from the tag (README section 6).

## [Unreleased]

## [0.4.0] - 2026-11-27

Shared provider account state across gateway nodes (change-0012). Upgrade notes: no database migration. Nodes need NATS JetStream for the shared state (the first node creates the bucket); without it a node logs a warning and keeps its own state as in 0.3.0. During a rolling upgrade, 0.3.0 nodes neither send nor read the shared state. Going back to 0.3.0 needs no data change; the bucket can be left or deleted.

### Changed

1. Provider account state is shared by all gateway nodes (change-0012, spec-0004 D40). A rest that one node sees (rate limit, outage, bad key, no credits) makes every node skip that account, and a node that starts or reconnects loads the current rests. It uses the NATS JetStream KV bucket `AIF_ACCOUNT_STATE` (`nats.state.bucket`), made by the first node. Without JetStream, each node keeps its own state as before. The Providers page shows one state for all nodes.

## [0.3.0] - 2026-11-27

Credit per provider account (task-0010, change-0008). Upgrade notes: migration v8 runs at startup and only adds columns, a table and an index. An older binary refuses a v8 database, so going back to 0.2.0 needs the manual SQL in change-0008 section 4. One client-visible change: an out-of-credits error on the OpenAI format is now `402 provider_credit_exhausted` (Added 3).

### Added

1. Credit per provider account (task-0010): `credit` with `type` `monthly` (plan credit per cycle), `payg` (budget per cycle) or `prepaid` (a balance), plus `usd`, `cycle_day` and `timezone` for the cycle types. It goes on an account, or on the provider as the default of its accounts (migration v8). `GET /admin/v1/providers` and the Providers page show the spend against it, with a warning at 80 percent.
2. Prepaid ledger, in the database only: `GET` and `POST /admin/v1/credit-entries`, `aif credit add | list`, and a top-up dialog on the Providers page. Entries are append-only; a negative amount corrects the balance.
3. OpenAI-format requests get `402` with code `provider_credit_exhausted` when the last account tried is out of credits, instead of the provider's own error. The Anthropic API still passes the provider's error through unchanged.
4. The usage API filters by `provider`.

## [0.2.0] - 2026-11-27

Catalog in the database, several accounts per provider, model targets (specs 0004-0006). Upgrade notes: see Changed; migrations v4-v7 run at startup and only add tables and columns.

### Added

1. Catalog in the database (spec-0006, tasks 0015-0016): migration v4 adds the catalog tables, a revision and an audit table. Nodes serve an immutable catalog snapshot swapped atomically; a request keeps the snapshot it started with.
2. `catalog.mode`: `database` (default; the file seeds an empty database once) or `file` (the file is the source of truth, applied on change and on SIGHUP without a restart).
3. Hot reload across nodes (task-0017): new control event `aif.control.v1.catalog_changed` `{type, revision, node, at}`, a revision poll (`catalog.poll_interval`, 30s), and metrics `aif_catalog_revision`, `aif_catalog_reloads_total`, `aif_catalog_last_reload_seconds`.
4. Encrypted provider keys (task-0018): `catalog.secret_keys` (`AIF_CATALOG_SECRET_KEY`) and migration v5. Keys are stored as references or AES-256-GCM values, never returned.
5. `aif catalog export | import | set-key | history` (task-0019): backup and apply the database catalog, store an encrypted provider key, list revisions.
6. Several accounts per provider (spec-0004, task-0020): `accounts`, `policy: priority | round_robin` (weighted), `sticky`, failover before the first byte with per-account cooldowns. Usage records and NATS usage events get `account` and `attempts` (migration v6). New metrics `aif_upstream_attempts_total` and `aif_account_cooldown`, new `GET /admin/v1/providers` and a Providers page.
7. Model aliases with several targets (spec-0005, task-0021): `targets` with per-target provider, model name, price, weight and formats; `policy`, `sticky` and `max_attempts` on the alias; fallback across providers (Anthropic-type targets only for `/anthropic`); cost from the answering target (migration v7).

### Changed

1. Provider keys in the config file must be references: `${NAME}`, `env:NAME` or `file:/path`. A key value written in the file is refused at startup.
2. In database mode (the default), edits to `providers`, `models` and `passthrough` in the file have no effect after the first start. Use `catalog.mode: file` to keep managing them in the file.

## [0.1.0] - 2026-11-27

First tagged release.

### Added

1. One OpenAI-compatible API (`/v1`, alias `/openai/v1`) in front of OpenAI-compatible providers, OpenRouter and Anthropic (`type: anthropic`, `openai_format: compat`).
2. Anthropic Messages API under `/anthropic` (or a dedicated port) for Claude Code and Anthropic SDK apps, with prompt-cache metering (spec-0002).
3. API keys with tenant, app and end-user labels; token and cost metering per request, streams included.
4. Built-in admin accounts (admin / viewer), admin API, web UI and chat Playground.
5. NATS extension points: usage events (JetStream), pre-request hook (allow / deny / modify), key revocation across nodes.
6. PostgreSQL store with migrations at startup.
7. Production readiness (spec-0003):
   1. `server.trusted_proxies` and `server.client_ip_header`: the admin sign-in throttle and logs use the real client IP behind a proxy.
   2. Prometheus metrics on their own listener (`metrics.listen`, default off).
   3. The public dev secrets are refused at startup unless `dev_mode: true`.

[0.4.0]: https://github.com/tasanakorn/ai-foundation/releases/tag/v0.4.0
[0.3.0]: https://github.com/tasanakorn/ai-foundation/releases/tag/v0.3.0
[0.2.0]: https://github.com/tasanakorn/ai-foundation/releases/tag/v0.2.0
[0.1.0]: https://github.com/tasanakorn/ai-foundation/releases/tag/v0.1.0
