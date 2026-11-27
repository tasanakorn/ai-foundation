# task-0008: Anthropic Messages API under /anthropic

- Status: done
- Spec: [spec-0002](../specs/spec-0002-anthropic-api-and-credits.md) sections 3, 4, 6
- Depends on: [task-0007](task-0007-anthropic-provider-type.md)
- Done when: Claude Code and the Anthropic SDK work through `https://<gateway>/anthropic` with an aif key, requests pass through byte for byte (except `model`), and every request is metered with cache tokens.

## 1. Scope

1. Paths
   1. `/anthropic/v1/messages`, `/anthropic/v1/messages/count_tokens`, `/anthropic/v1/models`.
   2. `/openai/v1/...` alias of `/v1/...`.
   3. `server.anthropic.path_prefix` and the optional dedicated listener `server.anthropic.listen`.
   4. Helpful 404 for `/anthropic/v1/v1/...`.
2. Model `formats` with the defaults of spec-0002 section 4; per-format `/models` lists.
3. Passthrough rules of spec-0002 section 6.3: in-place `model` edit, header forward and strip, query string, unbuffered SSE, unchanged upstream errors, Anthropic error envelope for gateway errors.
4. Metering of spec-0002 section 6.4: usage from JSON and SSE, token rule, `CacheWriteTokens`, `cache_write` price, `Format`, session ID, zero-cost `count_tokens`. Store migration for the new columns. Mirror in `web/app/types/admin.ts`.
5. Hook: `formats` config (default `["openai"]`) and the `format` field.
6. README: connect guide for Claude Code and Anthropic SDK apps.
7. Out of scope: batches (spec-0002 open question 11.1), translation to OpenAI-format providers.

## 2. Verification

1. Tests with a fake Anthropic upstream:
   1. Body bytes equal the client's, except the `model` value.
   2. `anthropic-*`, `user-agent`, `x-claude-code-*` forwarded; client `Authorization` and `x-api-key` never reach upstream.
   3. SSE events arrive unchanged and in order, including `ping`, with no buffering.
   4. Upstream 429 and 400 pass through with body and headers (`retry-after`, `x-should-retry`).
   5. Usage record: prompt, cached, cache write, completion tokens and cost, for stream and non-stream.
   6. Model formats: OpenAI-only alias on `/anthropic` gives 404 in the Anthropic shape.
   7. Hook skipped for Anthropic requests unless `formats` lists `anthropic`.
2. Manual: `claude -p` with `ANTHROPIC_BASE_URL=http://localhost:8080/anthropic` against a real key, and the request shows in the admin UI usage page.
3. `make -C gateway vet test` and `cd web && pnpm typecheck`.
4. Done 2026-11-27
   1. Item 1: `gateway/internal/gateway/anthropic_test.go` (fake upstream, all 7 points; the stream test fails if the gateway buffers) and `anthropic_internal_test.go` (in-place `model` edit, header filter, error types). Config tests cover `path_prefix`, `listen`, `formats` and hook `formats`.
   2. Item 2, live with the real Claude API: `claude -p --model claude-haiku-5-5` with `ANTHROPIC_BASE_URL=.../anthropic` and `ANTHROPIC_AUTH_TOKEN=<aif key>` answered correctly. Its first run wrote 30,713 tokens to the prompt cache; the second run read 14,642 of them back, so passthrough keeps caching. `count_tokens`, `/anthropic/v1/models` and the `/v1/v1` hint also checked live. Usage was checked through the admin API, not the UI page.
   3. Item 3: both pass.
   4. Small additions: thinking tokens are metered as reasoning tokens; the client's `anthropic-workspace-id` is dropped (spec-0002 section 6.3.2).
