# ai-foundation

A small, fast LLM gateway. Think "minimal LiteLLM / OpenRouter core" in one Go binary.

1. One OpenAI-compatible API in front of OpenRouter and local LLMs (llama.cpp, vLLM).
2. API keys with owner labels (tenant, app, end user).
3. Token and cost metering per request, including streams.
4. Local admin API and web UI. Everything else plugs in from outside (NATS events and hooks next).

Design: [spec-0001](docs/specs/spec-0001-gateway-core-architecture.md). Product goals: [ai-foundation](docs/products/ai-foundation.md).

## 1. Repository layout

| Folder      | Part                                  | Stack                 |
| ----------- | ------------------------------------- | --------------------- |
| `gateway/`  | Gateway binary `aif`                  | Go                    |
| `web/`      | Admin UI                              | Nuxt 4, Nuxt UI 4     |
| `scripts/`  | Dev loop                              | Python (stdlib only)  |
| `docs/`     | Product, specs, tasks, backlog        | Markdown              |

Each part builds on its own from its folder. The root `Makefile` only delegates.

## 2. Dev loop

Needs Go, Node + pnpm, Python 3. Optional: `llama-server` (llama.cpp).

```sh
# .env at the repo root (gitignored); the shell environment wins over it
OPENROUTER_API_KEY=sk-or-...
LLAMA_MODEL=/path/to/model.gguf            # or LLAMA_HF=<huggingface repo>
LLAMA_ARGS=--embeddings --pooling mean -c 4096
```

```sh
make dev
```

| Service         | URL                              | Notes                                         |
| --------------- | -------------------------------- | --------------------------------------------- |
| Public API      | http://127.0.0.1:8080/v1         | Rebuilds and restarts on `.go` / `.yaml` save |
| Admin API       | http://127.0.0.1:8081/admin/v1   | Master key `dev-master-key-0000`              |
| Admin UI        | http://localhost:3000            | Nuxt HMR, proxies `/admin/v1`                 |
| llama-server    | http://127.0.0.1:8090            | Only if `LLAMA_MODEL` or `LLAMA_HF` is set    |

Dev models (`gateway/aif.dev.yaml`): `mini` → OpenRouter `openai/gpt-4o-mini`, `local` → llama.cpp. Passthrough is on, so `openrouter/<any model>` works too.

```sh
# create a key in the UI (API keys → New key), then:
curl -s 127.0.0.1:8080/v1/chat/completions -H "Authorization: Bearer sk-aif-..." \
  -H "X-AIF-End-User: alice" \
  -d '{"model":"local","messages":[{"role":"user","content":"hi"}]}'
```

## 3. Test and build

```sh
make test     # go vet + go test -race, web typecheck
make build    # web/.output/public + gateway/bin/aif
```

## 4. Run in production

1. Copy `gateway/aif.example.yaml` to `aif.yaml` and edit it.
2. Set `admin.ui_dir` to the built `web/.output/public` to serve the UI on the admin port.
3. `aif serve -config aif.yaml`. The admin port listens on localhost only by default.
