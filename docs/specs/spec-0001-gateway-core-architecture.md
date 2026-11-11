# spec-0001: Gateway core architecture

- Status: accepted
- Owner: project owner
- Related: [product](../products/ai-foundation.md), [task-0001](../tasks/task-0001-gateway-core-mvp.md), [task-0002](../tasks/task-0002-admin-web-ui.md), [task-0003](../tasks/task-0003-nats-events-and-hooks.md), [task-0004](../tasks/task-0004-postgres-store.md)

## 1. Summary

1. One Go binary, `aif`, with two HTTP listeners:
   1. **Public API** (`:8080`) — OpenAI-compatible `/v1/*`. Auth by gateway API key.
   2. **Admin API** (`127.0.0.1:8081`) — `/admin/v1/*`. Auth by master key. Can also serve the web UI.
2. Core does four things only: **route**, **authenticate**, **meter**, **administer**.
3. Everything else is an extension that talks to core over network contracts.
4. Upstream targets, in priority order:
   1. OpenRouter — supported, live-tested.
   2. llama.cpp (`llama-server`) — supported, live-tested.
   3. vLLM — same OpenAI-compatible path; not live-tested yet.
5. Backing services:
   1. **NATS** (with JetStream) is **required** (D13, D14). The gateway does not start without it.
   2. Store: SQLite embedded by default; **PostgreSQL** for production (D12, task-0004).

## 1A. Repository layout (monorepo)

| Folder        | Part                                          | Language / tool                |
| ------------- | --------------------------------------------- | ------------------------------ |
| `gateway/`    | Gateway binary `aif`                          | Go module                      |
| `web/`        | Admin UI                                      | Nuxt 4 + Nuxt UI               |
| `scripts/`    | Dev loop (`dev.py`)                           | Python (stdlib)                |
| `deploy/`     | `local/compose.yaml`: Postgres + NATS for dev | Docker Compose                 |
| `extensions/` | Example extensions, one folder each           | Any (`example-budget`: Python) |
| `docs/`       | Product, specs, tasks                         | Markdown                       |

1. Each part is self-contained in its own folder (own `go.mod` / `package.json`). The repo root holds no Go or JS files.
2. Future extensions get their own top-level folder each (for example `extensions/<name>/`).
3. `make dev` starts Postgres + NATS in Docker, then runs the gateway (rebuild on save), the Nuxt dev server (HMR, proxies `/admin/v1`), and optionally `llama-server`.
4. App processes run natively for a fast loop; only backing services run in containers. Local config comes from the repo-root `.env` (template: `.env.example`).

## 2. Architecture

```
            OpenAI SDK / app / agent
                     │  Bearer sk-aif-…   (+ X-AIF-End-User)
                     ▼
 ┌──────────────────────────── aif ─────────────────────────────┐
 │  gateway (/v1)                                               │
 │    auth ──► keys.Service ──(cache 30s)──► Store               │
 │    route ─► router                                           │
 │    hook ──► hook.Guard ──► bus (request/reply) ───────────────┼──► NATS ◄──► extensions
 │    proxy ─► provider.Provider ──► upstream HTTP ──────────────┼──► OpenRouter / OpenAI-compatible
 │    meter ─► metering.Recorder ──(async batch)──► Sinks        │
 │                                         ├─ StoreSink (DB)     │
 │                                         └─ bus.UsageSink ─────┼──► NATS JetStream AIF_USAGE
 │  admin (/admin/v1, localhost) ──► Store, keys, router         │◄── web UI / portals / billing
 │  keys.Revoke ──► bus control event ◄──► other nodes           │
 └──────────────────────────────────────────────────────────────┘
                     │
          SQLite (embedded default)  ·  PostgreSQL (task-0004)
```

## 3. Components

Packages live under `gateway/internal/`.

| Package    | Role                                                      | Seam for later                      |
| ---------- | --------------------------------------------------------- | ----------------------------------- |
| `app`      | Wires one node (used by `cmd/aif` and e2e tests)          | —                                   |
| `config`   | YAML + `${ENV}` expansion + validation                    | Hot reload, more provider types     |
| `gateway`  | `/v1` handlers, body rewrite, SSE pipe, hook call         | Post-response hook                  |
| `hook`     | Hook wire types, `Hook` interface, `Guard` policy         | HTTP / gRPC transports              |
| `bus`      | NATS: connect, usage sink, control events, hook transport | More events (key created, config)   |
| `router`   | Model alias / passthrough resolution                      | Fallbacks, load balancing, weights  |
| `provider` | `Provider` interface; `OpenAI` adapter                    | Native adapters (Anthropic, Gemini) |
| `keys`     | Key gen, SHA-256 hash, auth cache, allow-list             | Budgets, rate limits, scopes        |
| `metering` | Usage parse, cost, `Recorder`, `Sink` interface           | Webhook / queue / gRPC sinks        |
| `store`    | `Store` interface; `sqlite` impl                          | Postgres impl                       |
| `admin`    | Admin REST + static UI hosting                            | More resources (tenants, budgets)   |

## 4. Data model

### 4.1 Ownership labels

1. Every key carries `tenant_id` and `app_id`. They are free-form strings, not foreign keys.
   1. Why: the builder on top owns the tenant/app registry. Core stays schema-light.
   2. Later: an optional registry ([backlog-0003](../backlogs/backlog-0003-tenant-app-registry.md)) can add tables without changing keys.
2. `end_user` is set per request:
   1. Header `X-AIF-End-User` (wins), else the OpenAI `user` body field.
   2. Lets one app key serve many end users and still meter each one.
3. `metadata` (string map) on keys holds anything else (plan, region, owner email).

### 4.2 Tables (SQLite)

1. `api_keys`: `id, name, prefix, hash (unique), tenant_id, app_id, allowed_models (json), metadata (json), created_at, expires_at, revoked_at`.
2. `usage_records`: `id (= request id), created_at, key_id, tenant_id, app_id, end_user, endpoint, model, provider, upstream_model, stream, status_code, prompt_tokens, completion_tokens, total_tokens, cached_tokens, reasoning_tokens, cost_usd, estimated, latency_ms, error`.
   1. Owner labels are copied into each row. Reports need no joins, and history stays correct if a key changes.
3. `schema_migrations`: append-only numbered migrations.
4. Times are stored as Unix milliseconds (UTC).

### 4.3 Keys

1. Format: `sk-aif-` + 32 URL-safe random chars (192 bits).
2. Only the SHA-256 hash is stored. The secret is returned once on create.
3. `prefix` (first 12 chars) is stored for display.
4. `allowed_models`: empty = all. Exact names or a trailing `*` prefix (`openrouter/anthropic/*`).
5. Revoke is soft (`revoked_at`). The local cache is cleared at once, and a `key_revoked` control event clears it on every other node (measured: 3 ms). A node that was disconnected clears its whole cache on reconnect.

## 5. Public API (`/v1`)

| Method | Path                   | Notes                                     |
| ------ | ---------------------- | ----------------------------------------- |
| GET    | `/v1/models`           | Configured aliases the key may use        |
| POST   | `/v1/chat/completions` | Stream and non-stream                     |
| POST   | `/v1/completions`      | Legacy completions                        |
| POST   | `/v1/embeddings`       |                                           |
| GET    | `/healthz`             | No auth. `503` while NATS is disconnected |

1. Auth: `Authorization: Bearer <key>` or `x-api-key: <key>`.
2. The body is decoded at the top level only. Unknown fields pass through untouched. Only `model` (and `stream_options` for streams) is changed.
3. Every response has `X-Request-Id`. It equals the usage record ID.
4. Errors use the OpenAI error shape, so SDKs parse them.
5. Upstream errors pass through with their status and body.

### 5.1 Model resolution

1. Exact alias from config (`gpt-4o-mini` → provider `openai`, upstream `gpt-4o-mini`).
2. If `passthrough: true`: `<provider>/<upstream model>` (`openrouter/anthropic/claude-sonnet-4.5`).
3. Else `404 model_not_found`.

## 6. Admin API (`/admin/v1`)

| Method | Path                      | Notes                                                                                |
| ------ | ------------------------- | ------------------------------------------------------------------------------------ |
| GET    | `/admin/v1/keys`          | `tenant_id`, `app_id`, `include_revoked`, `limit`, `offset`                          |
| POST   | `/admin/v1/keys`          | `{name, tenant_id, app_id, allowed_models, metadata, expires_at}`                    |
| GET    | `/admin/v1/keys/{id}`     |                                                                                      |
| DELETE | `/admin/v1/keys/{id}`     | Revoke (soft)                                                                        |
| GET    | `/admin/v1/models`        | Aliases, providers, passthrough flag. No secrets.                                    |
| GET    | `/admin/v1/usage`         | Raw records. Filters: `from, to, key_id, tenant_id, app_id, end_user, model`         |
| GET    | `/admin/v1/usage/summary` | Same filters + `group_by` = `none, day, model, provider, key, tenant, app, end_user` |

1. Listens on `127.0.0.1` by default ("local admin"). Exposing it is an explicit config change.
2. Master key is required (min 16 chars). Constant-time compare.
3. If `admin.ui_dir` is set, the built web UI is served at `/` with SPA fallback (same origin, no CORS).

## 7. Metering rules

1. Source of truth is the upstream `usage` object.
2. Streams: the gateway sets `stream_options.include_usage = true` and reads the final usage chunk. The chunk is passed to the client as is.
3. Cost:
   1. Provider-reported `usage.cost` (OpenRouter) wins.
   2. Else config price (USD / 1M tokens): `(prompt − cached) × input + cached × cached_input + completion × output`.
4. No usage from upstream → estimate at ~4 bytes per token, `estimated = true`.
5. Records are written async: in-memory queue → batch (200 or 1 s) → every sink. A full queue drops records and counts them; requests never wait on the DB.
6. Status codes: upstream status; `499` client closed; `502` upstream unreachable; `504` upstream timeout.

## 8. Extension model

The stable contracts are **network contracts**, not Go imports. Extensions can be any language.

| Contract                              | Direction     | Status | Use                                               |
| ------------------------------------- | ------------- | ------ | ------------------------------------------------- |
| Public `/v1` API                      | client → core | done   | Apps, agents, SDKs                                |
| Admin REST                            | ext → core    | done   | Portals, tenant managers, billing pull            |
| Usage events (NATS)                   | core → ext    | done   | Billing, analytics, alerts                        |
| Pre-request hook (NATS request/reply) | core → ext    | done   | Budgets, guardrails, custom auth, request rewrite |
| Control events (NATS)                 | core ↔ core   | done   | Instant key revocation across nodes               |
| HTTP webhook / gRPC                   | core → ext    | later  | Same payloads, for setups without NATS            |

1. Inside the binary, each seam is a Go interface: `store.Store`, `provider.Provider`, `metering.Sink`, and `hook.Hook`. A NATS, HTTP or gRPC extension is just one more implementation.
2. Hook and event payloads reuse the JSON shapes of `store.Key` and `store.UsageRecord`. Subjects carry a version (`aif.usage.v1.…`), so payloads can evolve without breaking subscribers.
3. Why NATS first: one connection gives pub/sub (events), request/reply with queue groups (hooks that scale out), and JetStream (durable usage stream, replay). Clients exist for every major language.
4. Packages stay under `internal/` until the Go API is worth freezing. Moving them to `pkg/` later is a rename, not a rewrite.

### 8.1 NATS contracts (v1)

Subjects are set in config (`nats.*`). Defaults:

| Subject                       | Kind                         | Payload                                    |
| ----------------------------- | ---------------------------- | ------------------------------------------ |
| `aif.usage.v1.<tenant>.<app>` | JetStream stream `AIF_USAGE` | One `store.UsageRecord` (JSON) per request |
| `aif.control.v1.key_revoked`  | Core pub/sub                 | `{type, id, node, at}`                     |
| `aif.hook.pre_request.v1`     | Request/reply                | `hook.Request` → `hook.Decision`           |

1. **Usage events**
   1. Subject tokens: empty label → `_`. Bytes outside `[A-Za-z0-9_-]` are `%XX`-escaped (`chat.bot` → `chat%2Ebot`), so tenant and app are always one token each. Subscribers can filter with `aif.usage.v1.acme.>`.
   2. JetStream publish waits for the server ack. `Nats-Msg-Id` = request ID, so a retried batch is stored once (2 min dedup window).
   3. The gateway creates the stream if missing (file storage, `max_age` 30 days). If it exists, the gateway leaves its settings alone.
   4. Every request that passed routing is a record, including hook denials (`status_code` = deny status, `error` = `<code>: <message>`).
2. **Pre-request hook**
   1. Runs after auth, routing and the key allow-list; before upstream. Only for models matching `hooks.pre_request.models`.
   2. Request: `{request_id, key: {id, tenant_id, app_id, metadata}, end_user, endpoint, model, provider, upstream_model, stream, body}`. Bodies over the NATS max payload (1 MiB default) are sent as `body: null, body_omitted: true`.
   3. Reply: `{"action":"allow"}`, `{"action":"deny","status":429,"code":"budget_exceeded","message":"…"}` (status 400–599, default 403), or `{"action":"modify","body":{…}}`.
   4. A modified body is parsed, routed and allow-list checked again. The hook cannot widen a key's model access.
   5. Timeout (default 200 ms), no responder, bad reply → `fail_open: true` allows; `false` answers `503 hook_unavailable`.
   6. Extensions subscribe with a queue group, so copies share the load.
3. **Control events**: every node subscribes. `key_revoked` drops that key from the auth cache.

### 8.2 Failure behavior

1. Startup: NATS unreachable → the gateway exits with an error. With JetStream on, the stream must be creatable or present.
2. NATS lost while running: the client reconnects forever (1 s wait). Meanwhile:
   1. `/healthz` answers `503`, so load balancers route away.
   2. Usage still goes to the store. The NATS sink retries each batch (0.1 s, 0.4 s, 1.6 s), then drops it and counts the drop.
   3. Hooks fail by policy (`fail_open`).
   4. On reconnect, the auth cache is cleared (revocations may have been missed).

## 9. Decisions

| ID  | Decision                                            | Why                                                                                                                                                |
| --- | --------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------- |
| D1  | Go, stdlib `net/http`, no web framework             | Small binary, low memory, Go 1.22+ mux has method + path params                                                                                    |
| D2  | OpenAI wire format on both sides                    | OpenRouter and most servers speak it. Native adapters translate inside `Provider`.                                                                 |
| D3  | Raw-JSON passthrough (top-level decode only)        | New OpenAI fields work without code changes                                                                                                        |
| D4  | Providers/models in YAML; keys/usage in DB          | Config is reviewable and versioned; runtime data changes often                                                                                     |
| D5  | SQLite via `modernc.org/sqlite` (pure Go, WAL)      | Zero setup, no CGO, cross-compiles. `Store` interface keeps Postgres open.                                                                         |
| D6  | Free-form `tenant_id` / `app_id` labels             | Multi-tenant ready without forcing a tenant model on builders                                                                                      |
| D7  | Async batched metering with pluggable sinks         | No DB write on the hot path; webhook/queue sinks slot in                                                                                           |
| D8  | Separate admin listener on localhost                | Safe default; public port never exposes admin                                                                                                      |
| D9  | Extensions over REST/gRPC, not Go plugins           | Any language; crash isolation; independent deploy                                                                                                  |
| D10 | Web UI = static Nuxt build served by admin listener | One origin, no CORS, UI is optional and replaceable                                                                                                |
| D11 | Monorepo, one folder per part                       | Fast dev loop; each part still builds alone and can split out later                                                                                |
| D12 | PostgreSQL as the production store                  | Preferred service; multi-node; SQLite stays the zero-setup default                                                                                 |
| D13 | NATS as the extension and event bus                 | Preferred service; events + request/reply + durable stream in one; any language                                                                    |
| D14 | NATS is required, not optional                      | One code path; multi-node correct by default (revocation, usage stream); extensions can rely on it. Cost: one more process, even for a single node |

## 10. Known limits (MVP)

1. Estimated tokens are rough (bytes / 4). A tokenizer can be added behind the same function.
2. The store sink and the NATS sink are independent. A NATS outage longer than the retries (~2 s per batch) drops those events from the stream; the store still has them. A store outage drops the batch from the store only.
3. Pre-request hooks add one NATS round trip per request (sub-millisecond locally) when on.
4. No built-in rate limits or budgets ([backlog-0001](../backlogs/backlog-0001-budgets-and-rate-limits.md)). `extensions/example-budget` shows a soft daily token budget: it counts a request only after its usage event arrives, so parallel requests can overshoot a little.
5. No `/v1/responses`, audio, images, or Anthropic-native `/v1/messages` yet ([backlog-0004](../backlogs/backlog-0004-native-provider-adapters.md)).
