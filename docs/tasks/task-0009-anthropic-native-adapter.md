# task-0009: Native Anthropic adapter for OpenAI-format clients

- Status: done
- Change: [change-0015](../changes/change-0015-anthropic-native-adapter.md)
- Spec: [spec-0002](../specs/spec-0002-anthropic-api-and-credits.md) section 7
- Depends on: [task-0008](task-0008-anthropic-messages-api.md)
- Done when: `openai_format: native` translates `/v1/chat/completions` to the Messages API and back (stream and non-stream), with prompt caching on and cache tokens metered.

## 1. Scope

1. Translation inside `Do` with the gateway's own JSON types (D44, changed 2026-11-28 from the official Go SDK): system hoisting, tools, images, `max_tokens`; SSE back to OpenAI chunks with `usage` last; `refusal` -> `content_filter`.
2. Clear 400 for fields the gateway cannot carry (spec-0002 section 7.2.2). `temperature`, `top_p` and forced `tool_choice` pass through, and Anthropic's own 400 comes back in OpenAI shape (D45, changed 2026-11-28).
3. Provider config `prompt_cache: auto | off` (default `auto`).
4. Example config switches to `openai_format: native`.

## 2. Verification

1. Table tests for request and response translation, including tools and streams.
2. Fake-upstream test: `cache_control` sent when `prompt_cache: auto`; usage record has cached tokens.
3. `make -C gateway vet test`.

## Result

1. `openai_format: native` (`gateway/internal/provider/anthropic_native.go`): `Anthropic.Do` translates `/chat/completions` to `POST /messages` with `anthropic-version: 2023-06-01`, and the answer back, as spec-0002 section 7 lists. Streams are translated event by event through a pipe, so the gateway still sends each chunk as it comes. Own JSON types, no SDK (D44); `temperature`, `top_p` and forced `tool_choice` pass through (D45).
2. Errors: upstream errors keep status and headers, with the body in the OpenAI shape and Anthropic's message, so the pool's "credit balance" check still works. Gateway 400s for fields it cannot carry, and `unsupported_endpoint` for `/completions` and `/embeddings`.
3. `prompt_cache: auto | off` (config, catalog, export, migration v10). `auto` sends the top-level `cache_control: {type: "ephemeral"}`. Config errors for an unknown value, for `prompt_cache` with `compat`, and on other provider types.
4. Metering: the translated usage has `prompt_tokens_details.cache_write_tokens`; `metering.Apply` now records and prices it for every OpenAI-format response.
5. Tests: request and response translation tables, stream translation (thinking, text, tool calls, cut stream, error event), fake upstream (`TestNativeDo`, `TestAnthropicNativeChatCompletions` with the usage record and cost), config checks, `TestRollbackV10`.
6. Not done: no test against the real Anthropic API yet. Points to check there: a tool-use conversation on a model that always thinks (the thinking blocks are not sent back), and the default `max_tokens` of 16000.
7. The local dev config (`gateway/aif.dev.yaml`) still uses `compat`; only the example config was switched, as the spec says.

## Verification

1. `make -C gateway vet test`, `python3 scripts/check_docs.py`: pass locally.

