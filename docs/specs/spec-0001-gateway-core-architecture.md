# spec-0001: Gateway core architecture

- Status: accepted
- Owner: project owner
- Related: [product](../products/ai-foundation.md), [task-0001](../tasks/task-0001-gateway-core-mvp.md), [task-0002](../tasks/task-0002-admin-web-ui.md), [task-0003](../tasks/task-0003-nats-events-and-hooks.md), [task-0004](../tasks/task-0004-postgres-store.md), [task-0005](../tasks/task-0005-admin-accounts.md), [task-0025](../tasks/task-0025-oidc-admin-login.md) ([change-0013](../changes/change-0013-oidc-admin-login.md))

## 1. Summary

1. One Go binary, `aif`, with two HTTP listeners:
   1. **Public API** (`:8080`) — OpenAI-compatible `/v1/*`. Auth by gateway API key.
   2. **Admin API** (`127.0.0.1:8081` by default) — `/admin/v1/*`. Auth by an OIDC login or the one local account (session cookie), or the optional master key. Can also serve the web UI.
2. Core does four things only: **route**, **authenticate**, **meter**, **administer**.
3. Everything else is an extension that talks to core over network contracts.
4. Upstream targets, in priority order:
   1. OpenRouter — supported, live-tested.
   2. llama.cpp (`llama-server`) — supported, live-tested.
   3. vLLM — same OpenAI-compatible path; not live-tested yet.
5. Backing services:
   1. **NATS** (with JetStream) is **required** (D13, D14). The gateway does not start without it.
   2. **PostgreSQL** is **required** (D15). It is the only store.

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
 │  admin (/admin/v1) ──► auth, Store, keys, router              │◄── web UI / portals / billing
 │  keys.Revoke ──► bus control event ◄──► other nodes           │
 └──────────────────────────────────────────────────────────────┘
                     │
                         PostgreSQL (required)
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
| `store`    | `Store` interface; `postgres` impl (pgx)                  | Partitioned usage, rollups          |
| `auth`     | Admin accounts, argon2id passwords, sessions, roles, OIDC | SAML, roles from IdP groups         |
| `admin`    | Admin REST, sign-in, playground, static UI hosting        | More resources (tenants, budgets)   |

## 4. Data model

### 4.1 Ownership labels

1. Every key carries `tenant_id` and `app_id`. They are free-form strings, not foreign keys.
   1. Why: the builder on top owns the tenant/app registry. Core stays schema-light.
   2. Later: an optional registry ([backlog-0003](../backlogs/backlog-0003-tenant-app-registry.md)) can add tables without changing keys.
2. `end_user` is set per request:
   1. Header `X-AIF-End-User` (wins), else the OpenAI `user` body field.
   2. Lets one app key serve many end users and still meter each one.
3. `metadata` (string map) on keys holds anything else (plan, region, owner email).

### 4.2 Tables (PostgreSQL)

1. `api_keys`: `id, name, prefix, hash (unique), tenant_id, app_id, allowed_models (text[]), metadata (jsonb), created_at, expires_at, revoked_at`.
2. `usage_records`: `id (= request id), created_at, key_id, tenant_id, app_id, end_user, endpoint, model, provider, upstream_model, stream, status_code, prompt_tokens, completion_tokens, total_tokens, cached_tokens, reasoning_tokens, cost_usd, estimated, latency_ms, error`.
   1. Owner labels are copied into each row. Reports need no joins, and history stays correct if a key changes.
3. `admin_users`: `id, email (unique, lowercase), name, role (admin | viewer), password_hash (argon2id; used only for the local account, D42), created_at, updated_at, last_login_at, disabled_at, link_allowed_until`.
4. `admin_sessions`: `hash (SHA-256 of the cookie value), user_id (cascade delete), created_at, expires_at, user_agent, ip`.
5. `admin_identities`: `id, user_id (cascade delete), provider (config name), issuer, subject, email, created_at, last_login_at`. Unique on `(issuer, subject)`. Links an OIDC login to an account (section 6.3).
6. `admin_oidc_flows`: `id_hash, provider, state, nonce, pkce_verifier, link_user_id, expires_at`. One row per login in progress; used once.
7. `schema_migrations`: append-only numbered migrations, applied at startup under a Postgres advisory lock (several nodes may start at once). A database newer than the binary is refused.
8. Times are `timestamptz`, returned in UTC.

### 4.3 Keys

1. Format: `sk-aif-` + 32 URL-safe random chars (192 bits).
2. Only the SHA-256 hash is stored. The secret is returned once on create.
3. `prefix` (first 12 chars) is stored for display.
4. `allowed_models`: empty = all. Exact names or a trailing `*` prefix (`openrouter/anthropic/*`).
5. Revoke is soft (`revoked_at`). The local cache is cleared at once, and a `key_revoked` control event clears it on every other node (measured: 3 ms). A node that was disconnected clears its whole cache on reconnect.

## 5. Public API (`/v1`)

| Method | Path                   | Notes                                       |
| ------ | ---------------------- | ------------------------------------------- |
| GET    | `/v1/models`           | Configured aliases the key may use          |
| POST   | `/v1/chat/completions` | Stream and non-stream                       |
| POST   | `/v1/completions`      | Legacy completions                          |
| POST   | `/v1/embeddings`       |                                             |
| GET    | `/healthz`             | No auth. `503` while NATS or the DB is down |

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

| Method | Path                                            | Notes                                                                                                                                    |
| ------ | ----------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------- |
| GET    | `/admin/v1/auth/methods`                        | No auth. `{password, oidc: [{name, title}], auto_redirect}`: the sign-in methods that are on (section 6.4)                               |
| POST   | `/admin/v1/auth/login`                          | No auth. `{email, password}` → `{user, role, method}` + session cookie. The local account only; `403` when the password is off (D43)     |
| GET    | `/admin/v1/auth/oidc/{name}/start`              | No auth. Redirects to the IdP. `?link=1` (signed in): link the login to your account instead (section 6.3)                               |
| GET    | `/admin/v1/auth/oidc/{name}/callback`           | No auth. The IdP returns here; sets the session cookie, then moves the browser to the UI                                                 |
| GET    | `/admin/v1/auth/identities`                     | Any role. Your own OIDC links                                                                                                            |
| DELETE | `/admin/v1/auth/identities/{id}`                | Any role. Removes one of your own links                                                                                                  |
| POST   | `/admin/v1/auth/logout`                         | Ends the session, clears the cookie                                                                                                      |
| GET    | `/admin/v1/auth/me`                             | Any role. `{user, role, method}`; `user` is null for the master key                                                                      |
| POST   | `/admin/v1/auth/password`                       | `403`: the local account's password is set in config (D42)                                                                               |
| GET    | `/admin/v1/users`                               | Admin accounts                                                                                                                           |
| POST   | `/admin/v1/users`                               | `{email, name, role}`: an OIDC account. `password` is refused (`400`)                                                                    |
| GET    | `/admin/v1/users/{id}`                          |                                                                                                                                          |
| PATCH  | `/admin/v1/users/{id}`                          | Any of `{name, role, disabled}`. Disable ends its sessions. `password` is refused; the local account only takes `name`                   |
| DELETE | `/admin/v1/users/{id}`                          | Hard delete; its sessions and OIDC links go with it                                                                                      |
| GET    | `/admin/v1/users/{id}/identities`               | Its OIDC links                                                                                                                           |
| DELETE | `/admin/v1/users/{id}/identities/{identity_id}` | Removes one link                                                                                                                         |
| POST   | `/admin/v1/users/{id}/identities/allow`         | Allows a one-time link by verified email for 24h (section 6.3). Not for the local account                                                |
| GET    | `/admin/v1/keys`                                | `tenant_id`, `app_id`, `include_revoked`, `limit`, `offset`                                                                              |
| POST   | `/admin/v1/keys`                                | `{name, tenant_id, app_id, allowed_models, metadata, expires_at}`                                                                        |
| GET    | `/admin/v1/keys/{id}`                           |                                                                                                                                          |
| DELETE | `/admin/v1/keys/{id}`                           | Revoke (soft)                                                                                                                            |
| GET    | `/admin/v1/models`                              | Aliases, providers, passthrough flag. No secrets.                                                                                        |
| GET    | `/admin/v1/usage`                               | Raw records. Filters: `from, to, id, key_id, tenant_id, app_id, end_user, model`                                                         |
| *      | `/admin/v1/playground/…`                        | Any role. Runs `/v1/{chat/completions, completions, embeddings, models}` in-process with the API key in `X-AIF-Key` (§6.2)               |
| GET    | `/admin/v1/usage/summary`                       | Same filters + `group_by` = `none, day, model, provider, key, tenant, app, end_user`                                                     |

1. Listens on `127.0.0.1:8081` by default. Not enforced: set `admin.listen` (or `AIF_ADMIN_LISTEN`) to e.g. `0.0.0.0:8081` for public access. The gateway logs a warning when it is not loopback. Use TLS in front.
2. If `admin.ui_dir` is set, the built web UI is served at `/` with SPA fallback (same origin, no CORS).

### 6.2 Playground

1. Lets a person test the gateway as a client from the admin UI, without CORS on the public port.
2. The admin route rewrites the path to `/v1/…`, sets `Authorization: Bearer <X-AIF-Key>`, strips the admin cookie and other credentials, and calls the public handler in-process.
3. So the request takes the full client path: key auth, allow-list, hook, upstream, streaming, metering. It is metered to that API key like any request.
4. It needs a signed-in admin (any role) and a real API key; the CSRF check applies.

### 6.1 Admin sign-in (D16, D42)

1. **Accounts.** Accounts live in `admin_users`. Two kinds (D42):
   1. **The local account (break-glass)**: the one account whose email is `admin.bootstrap.email`. Only it can sign in with a password, and only while no OIDC provider is set (D43), so a fresh install works without any outside identity system.
   2. **OIDC accounts**: every other account. No password; sign-in only through a linked IdP (section 6.3).
   3. Roles: `admin` (everything) and `viewer` (GET only, plus its own links and sign-out).
   4. Guards: you cannot disable or delete your own account, and the last active admin cannot be demoted, disabled or deleted.
2. **The local account.**
   1. Config: `admin.bootstrap.email` (env `AIF_ADMIN_EMAIL`) and one of `admin.bootstrap.password` (`AIF_ADMIN_PASSWORD`, plain, for local dev) or `admin.bootstrap.password_hash` (`AIF_ADMIN_PASSWORD_HASH`, an argon2id PHC string, for production). Both set is a config error.
   2. Read on every start. The account is created if missing, and its role is set to `admin`. If the stored hash does not match config (plain: the password does not verify; hash: the strings differ), the stored hash is replaced and the account's sessions end. Safe when several nodes start at once.
   3. Passwords: argon2id (19 MiB, 2 passes; parameters stored in each hash). Min 12 characters for a plain password.
   4. The API and UI cannot change its role, disable it, delete it, set its password or link it to an IdP. User objects show it with `local: true`.
   5. Password hashes of other accounts (from before D42) are ignored, not deleted, and their count is logged at startup.
   6. Its password is off when `admin.oidc.providers` has any provider (D43), or when `admin.password_login: false`: `POST /admin/v1/auth/login` returns `403` and `methods` says `password: false`. The account is still synced from config, so removing `admin.oidc` and restarting turns its password on again (the way back in when the IdP is down). Without `admin.bootstrap.email` there is no local account.
   7. Every password sign-in logs a warning and publishes `aif.audit.v1.breakglass` (section 8.1). The UI shows a warning in the sidebar while signed in with it.
3. **CLI.** `aif admin create-user -email E [-role viewer]` (an OIDC account, no password), `list-users`, and `hash-password` (reads a password from a hidden prompt or stdin, prints the argon2id hash for `AIF_ADMIN_PASSWORD_HASH`). These need only the config and Postgres, not NATS. `reset-password` is gone: the password is set in config.
4. **Sessions.**
   1. Cookie `aif_session`: random 256-bit token, `HttpOnly`, `SameSite=Strict`, `Path=/`. `Secure` by `admin.cookie_secure` (`auto` = HTTPS or `X-Forwarded-Proto: https`).
   2. Only the SHA-256 of the token is stored, so a database leak does not leak sessions. Sessions live in Postgres, so they work on every node.
   3. Lifetime `admin.session_ttl` (default 12h; `admin.oidc.session_ttl` for OIDC sign-ins). Expired rows are deleted on each new sign-in.
5. **CSRF.** Writes with a session cookie must have `Origin` equal to the request host (or `Sec-Fetch-Site: same-origin` when there is no `Origin`). Login and logout are checked too.
6. **Brute force.** Per node: 10 failed sign-ins per email and 100 per client IP in 15 min, then `429`. Unknown emails take the same time as wrong passwords. The client IP is the TCP peer, or the client IP header when the peer is in `server.trusted_proxies` ([spec-0003](spec-0003-production-readiness.md) D21).
7. **Master key** (optional). `admin.master_key` as a bearer token acts with the `admin` role, for scripts and emergency access. A wrong bearer token never falls back to the cookie. Empty = off; leave it empty in production unless scripts need it. Each use logs a warning; `aif.audit.v1.breakglass` is published at most once per node per minute.
8. **Outside logins.** OIDC login maps a verified identity to an `AdminUser` and calls `auth.Service.IssueSession` (section 6.3, D41).

### 6.3 Outside login: OIDC (D41)

1. **Role.** The gateway is an OIDC client only. 2FA, passkeys and "login with Google" are features of the identity provider (IdP). The gateway works with any OIDC IdP and bundles none.
2. **Config** (all optional; without `admin.oidc` nothing changes):
   ```yaml
   admin:
     public_url: https://admin.example.com # the IdP redirects back here; never taken from the Host header
     password_login: true                  # false: the local password is off even without OIDC (section 6.1 item 2.6)
     oidc:
       auto_redirect: sso                  # default empty; send the sign-in page straight to this provider (section 6.4)
       session_ttl: 1h                     # sessions from an OIDC login; default admin.session_ttl
       providers:
         - name: sso                       # in URLs: /admin/v1/auth/oidc/sso/...
           title: Company SSO              # button text
           issuer: https://auth.example.com
           client_id: aif-admin
           client_secret: ${AIF_OIDC_SSO_SECRET}
           scopes: [openid, email, profile] # default
           link_by_email: false            # default; see item 4.4
           allowed_groups: [aif-admins]    # default empty = no check; see item 4.5
           groups_claim: groups            # default
   ```
   1. Each provider's discovery document (`<issuer>/.well-known/openid-configuration`) is read at startup and cached. A provider that cannot be reached is logged; the next `start` for it tries again, at most every 30 seconds, and answers `503` with a short error page while it still fails. Its button stays on the sign-in page.
   2. The redirect URL to register at the IdP is `<public_url>/admin/v1/auth/oidc/<name>/callback`.
3. **Flow.**
   1. Start: the gateway makes a random flow id, state, nonce and PKCE verifier (S256). It stores them in `admin_oidc_flows` (only the SHA-256 of the flow id; 10 minute life; expired rows deleted on each new flow) and sets the cookie `aif_oidc` = flow id.
   2. `aif_oidc` is `HttpOnly`, `Secure` as `aif_session`, `SameSite=Lax`, `Path=/admin/v1/auth/oidc`. Lax is needed: the IdP sends the browser back from another site, and a `Strict` cookie is not sent on that redirect.
   3. Callback: the gateway looks up the flow by the cookie, checks `state`, exchanges the code with the PKCE verifier, and verifies the ID token (signature, `iss`, `aud`, `exp`, `nonce`). The flow row is deleted first, so it is used once. Any failure ends at the sign-in page with a short error and is logged.
   4. Claims: `email` and `email_verified` come from the ID token, or from the userinfo endpoint when the token lacks them.
   5. Then it finds the account (item 4) and issues a normal session (`aif_session`). It answers with a small same-origin page that moves the browser to the UI, so the next request carries the `SameSite=Strict` session cookie.
   6. The flow state is in Postgres, so start and callback may land on different nodes, and no shared secret is needed.
4. **Accounts.** No sign-up through the IdP. A login is accepted only for an active account linked to it:
   1. A link is `(issuer, subject)` in `admin_identities`. Logins match by the link, never by email alone.
   2. An admin allows a one-time link for an account (`.../identities/allow`, sets `link_allowed_until` = now + 24h). The next OIDC login whose email matches that account, with `email_verified: true`, adds the link and clears the allowance. This is how a new account gets its first login.
   3. A signed-in user links another IdP with `start?link=1`; the flow row holds the user id, and the callback adds the link and keeps the current session.
   4. `link_by_email: true` on a provider allows that one-time link for every active account, without the admin step. Off by default: some IdPs allow open sign-up or set `email_verified` loosely. Use it only for an IdP whose users you control.
   5. `allowed_groups`: when set, a login or a link is accepted only if the `groups_claim` claim (a list of strings, from the ID token or userinfo) has one of the groups; otherwise `403` at the sign-in page, logged. Use it when the IdP also has users who sign up by themselves (end users of other apps). Add `groups` (or what the IdP needs) to `scopes`.
   6. The role is the gateway's (`admin` or `viewer`). Claims from the IdP do not change it. A disabled account cannot sign in, linked or not.
   7. The local account cannot be linked or matched; it stays separate from the IdP.
   8. The guards of section 6.1 still hold. The way back in when no admin can sign in through the IdP is the master key, or removing `admin.oidc` and restarting, which turns the local account's password on again (D43).
5. **Sign-out.** `POST /admin/v1/auth/logout` ends the gateway session only. The IdP session stays.

### 6.4 Sign-in page and auto redirect

1. `admin.oidc.auto_redirect` names one of the providers (else a config error). `methods` returns it as `auto_redirect` (empty when off).
2. When it is set and there is no session, the sign-in page sends the browser to `/admin/v1/auth/oidc/<name>/start` at once.
3. No loop: the page does not redirect when it shows an error (`/login?error=...`) or after sign-out (`/login?signed_out=1`). It shows a button instead. Every failed `start` or `callback` lands on `/login?error=...`.
4. Deep link: a page opened without a session goes to `/login?next=<path>`. The UI keeps `next` (local paths only) in session storage across the IdP round trip and opens it after sign-in.
5. The redirect is done by the UI; the API has no redirect of its own, so scripts and the master key are not affected.

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

| Contract                              | Direction     | Status | Use                                                 |
| ------------------------------------- | ------------- | ------ | --------------------------------------------------- |
| Public `/v1` API                      | client → core | done   | Apps, agents, SDKs                                  |
| Admin REST                            | ext → core    | done   | Portals, tenant managers, billing pull (master key) |
| Usage events (NATS)                   | core → ext    | done   | Billing, analytics, alerts                          |
| Pre-request hook (NATS request/reply) | core → ext    | done   | Budgets, guardrails, custom auth, request rewrite   |
| Control events (NATS)                 | core ↔ core   | done   | Instant key revocation across nodes                 |
| HTTP webhook / gRPC                   | core → ext    | later  | Same payloads, for setups without NATS              |

1. Inside the binary, each seam is a Go interface: `store.Store`, `provider.Provider`, `metering.Sink`, and `hook.Hook`. A NATS, HTTP or gRPC extension is just one more implementation.
2. Hook and event payloads reuse the JSON shapes of `store.Key` and `store.UsageRecord`. Subjects carry a version (`aif.usage.v1.…`), so payloads can evolve without breaking subscribers.
3. Why NATS first: one connection gives pub/sub (events), request/reply with queue groups (hooks that scale out), and JetStream (durable usage stream, replay). Clients exist for every major language.
4. Packages stay under `internal/` until the Go API is worth freezing. Moving them to `pkg/` later is a rename, not a rewrite.

### 8.1 NATS contracts (v1)

Subjects are set in config (`nats.*`). Defaults:

| Subject                          | Kind                         | Payload                                                      |
| -------------------------------- | ---------------------------- | ------------------------------------------------------------ |
| `aif.usage.v1.<tenant>.<app>`    | JetStream stream `AIF_USAGE` | One `store.UsageRecord` (JSON) per request                   |
| `aif.control.v1.key_revoked`     | Core pub/sub                 | `{type, id, node, at}`                                       |
| `aif.control.v1.catalog_changed` | Core pub/sub                 | `{type, revision, node, at}` (spec-0006)                     |
| `aif.hook.pre_request.v1`        | Request/reply                | `hook.Request` -> `hook.Decision`                            |
| `aif.audit.v1.breakglass`        | Core pub/sub                 | `{type, method, user_id, email, ip, node, at}` (section 6.1) |

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
3. **Control events**: every node subscribes. `key_revoked` drops that key from the auth cache. `catalog_changed` makes each node reload the catalog when the revision is newer than the one it serves (spec-0006).

### 8.2 Failure behavior

1. Startup: PostgreSQL or NATS unreachable → the gateway exits with an error. With JetStream on, the stream must be creatable or present.
2. NATS lost while running: the client reconnects forever (1 s wait). Meanwhile:
   1. `/healthz` answers `503`, so load balancers route away.
   2. Usage still goes to the store. The NATS sink retries each batch (0.1 s, 0.4 s, 1.6 s), then drops it and counts the drop.
   3. Hooks fail by policy (`fail_open`).
   4. On reconnect, the auth cache is cleared (revocations may have been missed).
3. PostgreSQL lost while running: the pool reconnects on its own. Meanwhile `/healthz` answers `503`, keys already in the auth cache (30 s) still work, the store sink drops failed batches (counted; the NATS stream still has them), and the admin API answers `500`.

## 9. Decisions

| ID  | Decision                                                | Why                                                                                                                                                                                                                                  |
| --- | ------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| D1  | Go, stdlib `net/http`, no web framework                 | Small binary, low memory, Go 1.22+ mux has method + path params                                                                                                                                                                      |
| D2  | OpenAI wire format on both sides                        | OpenRouter and most servers speak it. Native adapters translate inside `Provider`.                                                                                                                                                   |
| D3  | Raw-JSON passthrough (top-level decode only)            | New OpenAI fields work without code changes                                                                                                                                                                                          |
| D4  | Providers/models in YAML; keys/usage in DB              | Config is reviewable and versioned; runtime data changes often                                                                                                                                                                       |
| D5  | ~~SQLite via `modernc.org/sqlite`~~                     | Replaced by D15 (change-0005)                                                                                                                                                                                                        |
| D6  | Free-form `tenant_id` / `app_id` labels                 | Multi-tenant ready without forcing a tenant model on builders                                                                                                                                                                        |
| D7  | Async batched metering with pluggable sinks             | No DB write on the hot path; webhook/queue sinks slot in                                                                                                                                                                             |
| D8  | Separate admin listener, localhost by default           | Safe default; public port never exposes admin. Switch to public with `admin.listen` / `AIF_ADMIN_LISTEN`                                                                                                                             |
| D9  | Extensions over REST/gRPC, not Go plugins               | Any language; crash isolation; independent deploy                                                                                                                                                                                    |
| D10 | Web UI = static Nuxt build served by admin listener     | One origin, no CORS, UI is optional and replaceable                                                                                                                                                                                  |
| D11 | Monorepo, one folder per part                           | Fast dev loop; each part still builds alone and can split out later                                                                                                                                                                  |
| D12 | PostgreSQL as the production store                      | Preferred service; multi-node. Now the only store (D15)                                                                                                                                                                              |
| D13 | NATS as the extension and event bus                     | Preferred service; events + request/reply + durable stream in one; any language                                                                                                                                                      |
| D14 | NATS is required, not optional                          | One code path; multi-node correct by default (revocation, usage stream); extensions can rely on it. Cost: one more process, even for a single node                                                                                   |
| D15 | PostgreSQL is required; SQLite removed                  | Same reason as D14: one code path, one test suite, multi-node by default. NATS already made aif a multi-process setup, so SQLite's zero setup is gone anyway. pgx is pure Go. Tests use an embedded Postgres, so no Docker is needed |
| D16 | Built-in admin accounts + sessions; master key optional | A gateway must be operable alone, before any identity system exists. Sessions in Postgres work on every node. Outside logins plug in later via `IssueSession`                                                                        |
| D41 | Generic OIDC client for admin login                     | 2FA, passkeys and Google login come from the IdP, so the gateway owns no such code and needs no new service. Not a forward-auth header: a header can be faked when the admin port is reached directly (change-0013)                  |
| D42 | One local password account (break-glass), from config   | Extra local accounts cannot appear by accident, and none of them skips the IdP's 2FA. Every use is logged and published (change-0013). Since D43 it works only without OIDC                                                          |
| D43 | Password sign-in only when no OIDC provider is set      | A password beside the IdP is a way around its 2FA. Way back in when the IdP is down: remove `admin.oidc` and restart, or the master key (change-0014)                                                                                |

## 10. Known limits (MVP)

1. Estimated tokens are rough (bytes / 4). A tokenizer can be added behind the same function.
2. Admin sign-in limits are per node; with N nodes an attacker gets N times the budget.
3. An account disabled at the IdP keeps its gateway session until it expires (`admin.oidc.session_ttl`). There is no back-channel logout yet.
4. The store sink and the NATS sink are independent. A NATS outage longer than the retries (~2 s per batch) drops those events from the stream; the store still has them. A store outage drops the batch from the store only.
5. Pre-request hooks add one NATS round trip per request (sub-millisecond locally) when on.
6. No built-in rate limits or budgets ([backlog-0001](../backlogs/backlog-0001-budgets-and-rate-limits.md)). `extensions/example-budget` shows a soft daily token budget: it counts a request only after its usage event arrives, so parallel requests can overshoot a little.
7. No `/v1/responses`, audio, images, or Anthropic-native `/v1/messages` yet ([backlog-0004](../backlogs/backlog-0004-native-provider-adapters.md)).
