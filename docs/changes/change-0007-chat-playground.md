# change-0007: Chat playground

- Status: applied
- Date: 2026-11-11
- Related: [spec-0001](../specs/spec-0001-gateway-core-architecture.md) §6.2; [task-0006](../tasks/task-0006-chat-playground.md)

## 1. What changed

1. New admin route `/admin/v1/playground/…`: runs a `/v1` request in-process with the API key in `X-AIF-Key`.
2. `/admin/v1/usage` takes `id` (the request ID).
3. New **Playground** page in the admin UI: streaming chat with per-reply tokens, timing, cost and request ID.

## 2. Why

1. The user asked for a simple chat UI so a person can check that the gateway is usable end to end.

## 3. Impact

1. Additive only. Playground requests are normal metered requests of the chosen API key.

## 4. Roll back

1. Revert this change.
