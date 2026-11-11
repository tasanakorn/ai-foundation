# ai-foundation

A small, fast LLM gateway. Think "minimal LiteLLM / OpenRouter core" in one Go binary.

1. One OpenAI-compatible API in front of OpenRouter and local LLMs (llama.cpp, vLLM).
2. API keys with owner labels (tenant, app, end user).
3. Token and cost metering per request, including streams.
4. Local admin API and web UI.
5. Everything else plugs in from outside over NATS (required): usage events, a pre-request hook (allow / deny / modify), and key revocation across nodes.

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

| Service               | URL                            | Notes                                         |
| --------------------- | ------------------------------ | --------------------------------------------- |
| Public API            | http://127.0.0.1:8080/v1       | Rebuilds and restarts on `.go` / `.yaml` save |
| Admin API             | http://127.0.0.1:8081/admin/v1 | Master key `dev-master-key-0000`              |
| Admin UI              | http://localhost:3000          | Nuxt HMR, proxies `/admin/v1`                 |
| llama-server          | http://127.0.0.1:8090          | Only if `LLAMA_MODEL` or `LLAMA_HF` is set    |
| PostgreSQL 18         | `127.0.0.1:5432`               | Started by `make dev` or `make services-up`   |
| NATS 2.15 (JetStream) | `nats://127.0.0.1:4222`        | Monitor at http://127.0.0.1:8222              |

1. Service commands: `make services-up`, `services-down` (keeps data), `services-reset` (deletes data), `services-ps`, `services-logs`, `psql`.
2. llama.cpp runs natively, not in Docker: on macOS, Docker cannot use the Metal GPU.
3. The gateway needs NATS and exits at startup without it. Postgres is not used yet ([task-0004](docs/tasks/task-0004-postgres-store.md)).
4. Running NATS yourself: `python scripts/dev.py --no-services` with `NATS_URL` set.

Dev models (`gateway/aif.dev.yaml`): `mini` → OpenRouter `openai/gpt-4o-mini`, `local` → llama.cpp. Passthrough is on, so `openrouter/<any model>` works too.

```sh
# create a key in the UI (API keys → New key), then:
curl -s 127.0.0.1:8080/v1/chat/completions -H "Authorization: Bearer sk-aif-..." \
  -H "X-AIF-End-User: alice" \
  -d '{"model":"local","messages":[{"role":"user","content":"hi"}]}'
```

## 3. Extensions (NATS)

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

## 4. Test and build

```sh
make test     # go vet + go test -race, web typecheck
make build    # web/.output/public + gateway/bin/aif
```

## 5. Run in production

1. Copy `gateway/aif.example.yaml` to `aif.yaml` and edit it.
2. Run NATS with JetStream (`nats-server --jetstream`) and set `nats.url`.
3. Set `admin.ui_dir` to the built `web/.output/public` to serve the UI on the admin port.
4. `aif serve -config aif.yaml`. The admin port listens on localhost only by default. `/healthz` is 503 while NATS is down.
