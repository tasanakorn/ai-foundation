# ai-foundation

A small, fast LLM gateway. Think "minimal LiteLLM / OpenRouter core" in one Go binary.

1. One OpenAI-compatible API in front of OpenRouter, Anthropic (Claude API) and local LLMs (llama.cpp, vLLM).
2. API keys with owner labels (tenant, app, end user).
3. Token and cost metering per request, including streams.
4. Admin sign-in through any OIDC identity provider (2FA, passkeys and Google login come from the IdP), or one local account with a password when no IdP is set; admin / viewer roles, admin API and web UI.
5. Everything else plugs in from outside over NATS (required): usage events, a pre-request hook (allow / deny / modify), and key revocation across nodes.
6. Backing services: PostgreSQL and NATS (JetStream), both required.

Design: [spec-0001](docs/specs/spec-0001-gateway-core-architecture.md). Product goals: [ai-foundation](docs/products/ai-foundation.md).

## 1. Repository layout

| Folder        | Part                           | Stack                |
| ------------- | ------------------------------ | -------------------- |
| `gateway/`    | Gateway binary `aif`           | Go                   |
| `web/`        | Admin UI                       | Nuxt 4, Nuxt UI 4    |
| `scripts/`    | Dev loop                       | Python (stdlib only) |
| `deploy/`     | Local backing services         | Docker Compose       |
| `extensions/` | Example extensions (NATS only) | Python               |
| `docs/`       | Product, specs, tasks, backlog | Markdown             |

Each part builds on its own from its folder. The root `Makefile` only delegates.

## 2. Dev loop

Needs Go, Node + pnpm, Python 3, Docker (Postgres + NATS). Optional: `llama-server` (llama.cpp), `uv` (example extension).

```sh
cp .env.example .env     # then fill in OPENROUTER_API_KEY and LLAMA_MODEL or LLAMA_HF
make dev                 # Postgres + NATS (Docker), then gateway + web (+ llama-server)
```

`.env` is gitignored and read by both `scripts/dev.py` and Docker Compose. Variables set in your shell win over it.

| Service               | URL                             | Notes                                               |
| --------------------- | ------------------------------- | --------------------------------------------------- |
| Public API            | http://127.0.0.1:8080/v1        | Rebuilds and restarts on `.go` / `.yaml` save       |
| Anthropic API         | http://127.0.0.1:8080/anthropic | Claude Code and Anthropic SDK apps (section 3)      |
| Admin API             | http://127.0.0.1:8081/admin/v1  | Master key `dev-master-key-0000` (scripts)          |
| Admin UI              | http://localhost:3000           | Sign in: `admin@example.com` / `dev-admin-password` |
| llama-server          | http://127.0.0.1:8090           | Only if `LLAMA_MODEL` or `LLAMA_HF` is set          |
| PostgreSQL 18         | `127.0.0.1:5432`                | Started by `make dev` or `make services-up`         |
| NATS 2.15 (JetStream) | `nats://127.0.0.1:4222`         | Monitor at http://127.0.0.1:8222                    |

1. Service commands: `make services-up`, `services-down` (keeps data), `services-reset` (deletes data), `services-ps`, `services-logs`, `psql`.
2. llama.cpp runs natively, not in Docker: on macOS, Docker cannot use the Metal GPU.
3. The gateway needs PostgreSQL and NATS and exits at startup without them.
4. Running them yourself: `python scripts/dev.py --no-services` with `DATABASE_URL` and `NATS_URL` set.
5. **Playground** (admin UI → Playground): paste an API key, pick a model, and chat with streaming. Each message takes the real `/v1` path and shows tokens, time to first token, cost and the request ID.
6. The dev admin account is the local account (`AIF_ADMIN_EMAIL`, `AIF_ADMIN_PASSWORD`), read on every start. It is the only account with a password; other accounts sign in through an OIDC identity provider (`admin.oidc`). Manage accounts on the Admins page or with `aif admin create-user | list-users`.

Dev models (`gateway/aif.dev.yaml`): `mini` → OpenRouter `openai/gpt-4o-mini`, `local` → llama.cpp, `claude-opus-5-5`, `claude-sonnet-5-5` and `claude-haiku-5-5` → Claude API (needs `ANTHROPIC_API_KEY`). Passthrough is on, so `openrouter/<any model>` works too.

```sh
# create a key in the UI (API keys → New key), then:
curl -s 127.0.0.1:8080/v1/chat/completions -H "Authorization: Bearer sk-aif-..." \
  -H "X-AIF-End-User: alice" \
  -d '{"model":"local","messages":[{"role":"user","content":"hi"}]}'
```

## 3. Connect apps and coding agents

One gateway key (`sk-aif-...`) works for both API formats. Details: [spec-0002](docs/specs/spec-0002-anthropic-api-and-credits.md).

| Client                           | Base URL                      | Key setting                     |
| -------------------------------- | ----------------------------- | ------------------------------- |
| OpenAI SDKs and tools            | `https://<gateway>/v1`        | `OPENAI_API_KEY` / `api_key`    |
| Claude Code                      | `https://<gateway>/anthropic` | `ANTHROPIC_AUTH_TOKEN`          |
| Anthropic SDKs, Claude Agent SDK | `https://<gateway>/anthropic` | `ANTHROPIC_API_KEY` / `api_key` |

1. The Anthropic base URL ends in `/anthropic`, **without** `/v1`: the client adds `/v1/messages`. A request to `/anthropic/v1/v1/...` gets a 404 that says so.
2. `/openai/v1` is an alias of `/v1`.
3. Only aliases on a provider of `type: anthropic` work on `/anthropic`. The body goes upstream unchanged except `model`, so prompt caching works, and cache reads and writes are metered.
4. With a Claude Max, Ultra or Team plan, use an Anthropic key from the Console org linked to the plan: the gateway then spends the plan's monthly API credits.
5. An ingress that cannot route by path can use a dedicated port instead: `AIF_ANTHROPIC_LISTEN=:8082` serves the Anthropic API at the root of that port.

```sh
# Claude Code through the gateway
ANTHROPIC_BASE_URL=http://127.0.0.1:8080/anthropic ANTHROPIC_AUTH_TOKEN=sk-aif-... claude -p "hello"
```

```python
import anthropic
client = anthropic.Anthropic(base_url="http://127.0.0.1:8080/anthropic", api_key="sk-aif-...")
```

## 4. Extensions (NATS)

| Subject                       | Kind                         | Use                                   |
| ----------------------------- | ---------------------------- | ------------------------------------- |
| `aif.usage.v1.<tenant>.<app>` | JetStream stream `AIF_USAGE` | One JSON usage record per request     |
| `aif.hook.pre_request.v1`     | Request/reply, queue group   | Allow, deny or modify before upstream |
| `aif.control.v1.key_revoked`  | Pub/sub                      | All nodes drop the revoked key        |

Payloads and failure rules: [spec-0001 §8.1](docs/specs/spec-0001-gateway-core-architecture.md). Example: a daily token budget per tenant.

```sh
BUDGET_TENANT_LIMITS="acme=5000" uv run extensions/example-budget/budget.py
```

The dev config has the hook on with `fail_open: true`, so requests pass when no extension runs.

## 5. Test and build

```sh
make test     # go vet + go test -race, web typecheck
make build    # web/.output/public + gateway/bin/aif
```

1. Go tests need no Docker: they start an embedded PostgreSQL (downloaded once into `gateway/.cache/`) and an in-process NATS.
2. To test against another Postgres: `AIF_TEST_PG_URL=postgres://aif:aif-dev@127.0.0.1:5432/aif?sslmode=disable make test`. Each test makes and drops its own database.

## 6. Run in production

There is no prebuilt image or binary. Build from a release tag (see [CHANGELOG.md](CHANGELOG.md)):

```sh
git checkout v0.7.0
make build    # web/.output/public + gateway/bin/aif (version from the tag)
```

1. Copy `gateway/aif.example.yaml` to `aif.yaml` and edit it.
2. Run PostgreSQL and set `DATABASE_URL`. Migrations run at startup.
3. Run NATS with JetStream (`nats-server --jetstream`) and set `nats.url`.
4. Admin sign-in ([spec-0001](docs/specs/spec-0001-gateway-core-architecture.md) sections 6.1 and 6.3):
   1. The local account: set `AIF_ADMIN_EMAIL` and `AIF_ADMIN_PASSWORD_HASH` (make it with `aif admin hash-password`). It is the only account with a password, read on every start, and it works only while no OIDC provider is set (D43). Every use is logged and published on `aif.audit.v1.breakglass`.
   2. Everyone else: add an OIDC identity provider in `admin.oidc` with `admin.public_url`, and register `<public_url>/admin/v1/auth/oidc/<name>/callback` at the IdP. Add each person on the Admins page (or `aif admin create-user`); their first sign-in within 24 hours links the account. Use `allowed_groups` when the IdP also has users who sign up by themselves.
   3. Once a provider is set, password sign-in is off. Link at least one other admin to the IdP first: sign in with the local account, add the account, then add the provider and restart. `admin.oidc.auto_redirect: <name>` skips the sign-in page. When the IdP is down, remove `admin.oidc` and restart to use the local account again. Keep `admin.master_key` empty unless scripts need it.
5. Set `admin.ui_dir` to the built `web/.output/public` to serve the UI on the admin port.
6. `aif serve -config aif.yaml`. The admin port listens on `127.0.0.1:8081` by default; set `AIF_ADMIN_LISTEN=0.0.0.0:8081` (or `admin.listen`) for public access, behind TLS (`admin.cookie_secure: auto` marks the cookie Secure on HTTPS). `/healthz` is 503 while NATS or PostgreSQL is down.
7. Behind a reverse proxy (ingress, CDN, load balancer), set `server.trusted_proxies` (`AIF_TRUSTED_PROXIES`) to the proxy addresses and `server.client_ip_header` if it is not `X-Forwarded-For`. Otherwise every admin sign-in looks like it comes from the proxy, and the per-IP sign-in throttle hits everyone at once. Make the proxy the only way in: a client that reaches the gateway directly can set the header too.
8. Metrics: set `metrics.listen` (`AIF_METRICS_LISTEN=:9090`) and scrape `GET /metrics` (Prometheus text format). Keep that port off the public ingress. Families: [spec-0003 section 4](docs/specs/spec-0003-production-readiness.md).
9. Catalog (providers and models): it lives in PostgreSQL. With `catalog.mode: database` (default) the file seeds an empty database once; with `catalog.mode: file` (`AIF_CATALOG_MODE=file`) the file stays the source of truth and is applied on change and on SIGHUP, without a restart. Write provider keys as `${NAME}`, `env:NAME` or `file:/path`; key values in the file are refused. Back it up or apply a reviewed file with `aif catalog export` and `aif catalog import -file catalog.yaml`; store a key encrypted in the database with `aif catalog set-key -provider NAME` (needs `catalog.secret_keys`). Details: [spec-0006](docs/specs/spec-0006-catalog-database-hot-reload.md).
10. The dev secrets from `.env.example` (`dev-master-key-0000`, `dev-admin-password`) are refused at startup unless `dev_mode: true`, which only `gateway/aif.dev.yaml` sets.
