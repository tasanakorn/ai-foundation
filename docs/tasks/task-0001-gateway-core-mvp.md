# task-0001: Gateway core MVP

- Status: done
- Spec: [spec-0001](../specs/spec-0001-gateway-core-architecture.md)
- Done when: an OpenAI SDK can call a model through `aif` with a gateway key, and the request shows up in admin usage with correct tokens and cost.

## 1. Scope

1. Config loader with `${ENV}` expansion and validation.
2. OpenAI-compatible provider adapter (covers OpenRouter and direct APIs).
3. Router: aliases + `<provider>/<model>` passthrough.
4. Keys: create, hash, auth cache, revoke, model allow-list.
5. `/v1/models`, `/v1/chat/completions` (stream + non-stream), `/v1/completions`, `/v1/embeddings`.
6. Metering: usage parse, stream `include_usage`, cost, estimate fallback, async batched sink.
7. SQLite store with migrations.
8. Admin API: keys, models, usage list, usage summary. Optional static UI hosting.

## 2. Verification

1. `go test -race ./...` passes. Tests use a fake upstream and cover: auth, revoke, model rewrite, body passthrough, stream usage, estimate, upstream error pass-through, allow-list, passthrough routing, summary.
2. Smoke run: binary 11 MB (stripped), ~20 MB RSS at idle.
3. Live test, 2026-11-11 (all through the gateway, usage checked in admin API):

   | Target                                 | Chat | Stream | Completions | Embeddings | Usage source           |
   | -------------------------------------- | ---- | ------ | ----------- | ---------- | ---------------------- |
   | OpenRouter (`openai/gpt-4o-mini`)      | ok   | ok     | —           | —          | exact tokens + cost    |
   | llama.cpp b12110 (Qwen2.5-0.5B Q4_K_M) | ok   | ok     | ok          | ok ¹       | exact tokens           |
   | Upstream down                          | 502 recorded                                           |

   1. ¹ llama-server needs `--embeddings --pooling mean` for a chat model.
   2. OpenRouter always sends usage and cost. Its non-stream replies start with keep-alive whitespace; passthrough and parsing handle it.
   3. vLLM not tested live (no GPU host here). It uses the same OpenAI-compatible path and supports `stream_options.include_usage`.
   4. Gateway RSS after the tests: ~18 MB.
