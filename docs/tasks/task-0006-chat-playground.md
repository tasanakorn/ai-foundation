# task-0006: Chat playground in the admin UI

- Status: done
- Spec: [spec-0001](../specs/spec-0001-gateway-core-architecture.md) §6, §6.2
- Change: [change-0007](../change/change-0007-chat-playground.md)
- Done when: a signed-in person can chat through the gateway from the admin UI with a real API key, see streaming replies, and see what was metered.

## 1. Scope

1. **Gateway:** `/admin/v1/playground/{chat/completions, completions, embeddings, models}` runs the public `/v1` handler in-process with the key from `X-AIF-Key`. The admin cookie never reaches the public handler or the upstream.
2. **Usage lookup:** `/admin/v1/usage?id=<request id>` (new `UsageFilter.ID`).
3. **Web page `/playground`:**
   1. API key (kept in `sessionStorage` for the tab), model picker filled from the models this key may use (free text allowed for passthrough), end user, optional system prompt.
   2. Streaming chat (Enter sends, Shift+Enter new line), Stop, New chat. Reasoning text (if the model streams it) in a fold.
   3. Per reply: model, prompt → completion tokens, time to first token, total time, then from the usage record: cost, estimated flag, provider and upstream model, request ID (copy).
   4. Errors show the HTTP status and the gateway message (bad key, model not allowed, hook deny, upstream error).
4. Out of scope: saved chats, Markdown rendering, files and images.

## 2. Code

| Path                                          | What                                  |
| --------------------------------------------- | ------------------------------------- |
| `gateway/internal/admin/playground.go`        | In-process proxy to `/v1`             |
| `gateway/internal/gateway/playground_test.go` | End-to-end tests                      |
| `web/app/pages/playground.vue`                | Page, SSE reader, usage record lookup |

## 3. Verification

1. End-to-end test: not signed in `401`, no key `400`, bad key `401`, model not allowed `403`, `/models` per key, chat and stream `200` with `X-Request-Id`, usage found by id with the end user, viewer allowed, cross-origin `403`.
2. Browser (Playwright, UI served by the gateway, llama.cpp Qwen2.5-0.5B): two-turn streaming chat, tokens and timing shown (first token ~140–210 ms), cost and provider from the usage record; unknown model shows `HTTP 404 model nope-model does not exist`. Rows in `usage_records` carry `end_user=alice`.
