# task-0009: Native Anthropic adapter for OpenAI-format clients

- Status: todo
- Spec: [spec-0002](../specs/spec-0002-anthropic-api-and-credits.md) section 7
- Depends on: [task-0008](task-0008-anthropic-messages-api.md)
- Done when: `openai_format: native` translates `/v1/chat/completions` to the Messages API and back (stream and non-stream), with prompt caching on and cache tokens metered.

## 1. Scope

1. Translation inside `Do` with the official Go SDK (`github.com/anthropics/anthropic-sdk-go`): system hoisting, tools, images, `max_tokens`; SSE back to OpenAI chunks with `usage` last; `refusal` -> `content_filter`.
2. Clear 400 for fields current models refuse (`temperature`, `top_p`, forced `tool_choice`).
3. Provider config `prompt_cache: auto | off` (default `auto`).
4. Example config switches to `openai_format: native`.

## 2. Verification

1. Table tests for request and response translation, including tools and streams.
2. Fake-upstream test: `cache_control` sent when `prompt_cache: auto`; usage record has cached tokens.
3. `make -C gateway vet test`.
