# change-0015: Native Anthropic adapter for OpenAI-format clients

- Status: applied (released in v0.7.0)
- Date: 2026-11-28
- Related: [task-0009](../tasks/task-0009-anthropic-native-adapter.md); [spec-0002](../specs/spec-0002-anthropic-api-and-credits.md) sections 5, 7, D44 and D45; [backlog-0004](../backlogs/backlog-0004-native-provider-adapters.md) item 1.1

## 1. What changes

1. **New `openai_format: native`** on `anthropic` providers. `POST /v1/chat/completions` (and `/openai/v1/...`) is translated by the gateway to `POST /v1/messages`, and the answer back to the OpenAI chat format, for both normal and stream responses. `compat` stays as it is.
2. **Prompt caching** for OpenAI-format clients: the gateway sets the top-level `cache_control: {type: "ephemeral"}`. New provider setting `prompt_cache: auto | off` (default `auto`, only with `native`).
3. **Cache tokens are metered** on `/v1`: `cached_tokens` and the new `cache_write_tokens` in `usage.prompt_tokens_details`, so the usage record and the cost count cache reads and writes.
4. **Errors** from Anthropic come back in the OpenAI error shape, with the same status and the upstream message. Request fields the gateway cannot carry get a clear 400 from the gateway (for example `n` above 1, `logprobs`, audio input).
5. The example config switches the Anthropic provider to `openai_format: native`.
6. Docs-only fix in the same branch: spec-0003 status `draft` -> `built` (all its tasks were done in v0.3.0-v0.4.0).

## 2. Why

1. Anthropic says its OpenAI compatibility layer is "not considered a long-term or production-ready solution for most use cases".
2. Through it, prompt caching is not supported and `prompt_tokens_details` is always empty, so OpenAI-format apps on Claude pay full input price every turn, and the gateway cannot show cache use.

## 3. Impact

1. Config: `openai_format: native` is now valid; new `prompt_cache` on `anthropic` providers. No new env variable.
2. Data: migration v10 adds `catalog_providers.prompt_cache` (text, default `''`).
3. Public API: `/v1/chat/completions` on a `native` provider answers in OpenAI chat format, as with `compat`. `usage.prompt_tokens_details` gains `cache_write_tokens` (additive; OpenRouter uses the same name). `/v1/completions` and `/v1/embeddings` on a `native` provider get `400 unsupported_endpoint` (Anthropic has neither).
4. Metering: `cache_write_tokens` from any OpenAI-format provider that sends it (also OpenRouter) is now recorded and priced.
5. Spec-0002: section 5 (config), section 7 (rewritten with the field mapping), D44, D45; status `built` when this is applied.
6. No NATS change. No admin API or UI change.
7. Dependencies: none. The adapter uses its own small JSON types (D44), not the Anthropic Go SDK that spec-0002 section 7.2 named.
8. Product doc: no change. It already promises OpenAI-format access to Anthropic.

## 4. Left out

1. Thinking text back to the client. Current models leave it empty unless `thinking.display` is set; a thinking text that comes back is sent as `reasoning_content`, but thinking blocks are never sent upstream again, so reasoning is not carried between turns (same as `compat`).
2. Model rules in the gateway, such as which models refuse `temperature` or a forced `tool_choice` (D45).
3. `/v1/completions`, `/v1/embeddings`, the Responses API.

## 5. Roll back

1. Set the providers back to `openai_format: compat` and go back to v0.6.0.
2. An older binary refuses a database at schema v10. Undo v10 by hand first (tested in `TestRollbackV10`); the `prompt_cache` settings are lost:

   ```sql
   BEGIN;
   ALTER TABLE catalog_providers DROP COLUMN prompt_cache;
   DELETE FROM schema_migrations WHERE version = 10;
   COMMIT;
   ```
