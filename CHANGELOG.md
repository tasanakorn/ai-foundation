# Changelog

All notable changes. Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Versions: [Semantic Versioning](https://semver.org/); `0.x` while the API may still change.

There is no prebuilt image or binary: build from the tag (README section 6).

## [Unreleased]

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

[0.1.0]: https://github.com/tasanakorn/ai-foundation/releases/tag/v0.1.0
