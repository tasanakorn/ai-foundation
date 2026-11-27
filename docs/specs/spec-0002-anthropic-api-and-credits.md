# spec-0002: Anthropic API, plan credits and Messages API inbound

- Status: draft
- Owner: project owner
- Related: [product](../products/ai-foundation.md); [spec-0001](spec-0001-gateway-core-architecture.md) sections 5, 7, 8.1, 9; [backlog-0004](../backlogs/backlog-0004-native-provider-adapters.md) items 1.1, 1.3, 2; tasks [task-0007](../tasks/task-0007-anthropic-provider-type.md), [task-0008](../tasks/task-0008-anthropic-messages-api.md), [task-0009](../tasks/task-0009-anthropic-native-adapter.md), [task-0010](../tasks/task-0010-anthropic-credit-awareness.md)

## 1. Summary

1. Add Anthropic as a provider, so the gateway can spend the monthly API credits of Claude Max, Ultra and Team plans.
2. Add the Anthropic Messages API as a second public API format under `/anthropic`, so coding agents (Claude Code, Agent SDK) and Anthropic SDK apps use the gateway with no code change.
3. Keep one shape per path: `/v1` stays OpenAI only.

## 2. Background

### 2.1 Plan API credits

Source: [API credits for Max, Ultra, and Team plans](https://platform.claude.com/docs/en/about-claude/api-credits-for-subscribers) and [Help Center article](https://support.claude.com/en/articles/17154008-monthly-api-credits-for-max-ultra-and-team-plans) (read 2026-11-27).

1. Who gets credits
   1. Max: 100 USD a month. Ultra: 200 USD a month.
   2. Team: 20 USD per Standard seat, 100 USD per Premium seat, pooled, max 500 USD a month.
   3. Free, Pro and Enterprise get nothing.
2. How it works
   1. The subscriber links one Claude Console organization on claude.ai (Settings > Billing). The link cannot be changed without support.
   2. Credits land in that organization each billing cycle. They expire at the end of the cycle (no rollover).
   3. Any normal API key in that organization spends them. All keys and workspaces share one balance.
   4. Included credits are spent first, then purchased credits.
3. What they cover
   1. Covered: Claude API (Messages, Message Batches), Managed Agents, Agent SDK, Console playground. `claude -p` is covered when run with an API key from the linked organization.
   2. Not covered: interactive Claude Code (terminal, IDE, desktop, web), extra usage in Claude apps, Bedrock, Vertex AI, Foundry.
4. When credits run out
   1. With no purchased credits, requests fail with: `Your credit balance is too low to access the Anthropic API. Please go to Plans & Billing to upgrade or purchase credits.`
   2. Usage is never charged to the Claude plan.
5. What this means for the gateway
   1. The gateway needs only a normal Console API key from the linked organization. No claude.ai login or OAuth token is involved.
   2. Anthropic decides which product a request counts as. The gateway forwards client headers unchanged and does not try to change that. Interactive Claude Code sent through the gateway is likely to count as Claude Code, so it needs purchased credits.

### 2.2 Anthropic-format clients

Source: [Claude Code LLM gateway](https://code.claude.com/docs/en/llm-gateway), [gateway protocol](https://code.claude.com/docs/en/llm-gateway-protocol), [connect guide](https://code.claude.com/docs/en/llm-gateway-connect) (read 2026-11-27).

1. Clients reach a gateway through `ANTHROPIC_BASE_URL`.
2. Auth they send
   1. `ANTHROPIC_AUTH_TOKEN` -> `Authorization: Bearer <token>`.
   2. `ANTHROPIC_API_KEY` -> `x-api-key: <key>`.
   3. `apiKeyHelper` -> both headers.
   4. `httpx.BearerToken` already reads both headers, so aif keys work as they are.
3. What Claude Code needs from a gateway
   1. `POST /v1/messages` under the base URL (sent as `/v1/messages?beta=true`). Required.
   2. `POST /v1/messages/count_tokens`. Optional; without it Claude Code estimates.
   3. `GET /v1/models` in Anthropic format. Only used when the client sets `CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1`. Model IDs must contain `claude` or `anthropic`, or Claude Code drops them. No redirects.
   4. Forward every `anthropic-*` request header unchanged (`anthropic-version`, `anthropic-beta`). Do not allowlist beta values.
   5. Do not rewrite the body: keep `cache_control`, block-form `system` (attribution block first), earlier `messages` and thinking blocks exactly as sent. Rewrites break prompt caching (silently) and preserved thinking (`bound to a different conversation`).
   6. Stream without buffering, keep `ping` events, and send every event through `message_stop`.
   7. Pass upstream errors through unmodified (status, body, `retry-after` in seconds, `x-should-retry`, `anthropic-ratelimit-unified-*`). Claude Code retries based on the error text.

### 2.3 How others lay out the paths

1. Pattern 1, a subpath per format: DeepSeek and Kimi (`/anthropic`), Z.ai (`/api/anthropic`), Alibaba Model Studio (`/apps/anthropic`), LiteLLM passthrough (`/anthropic`, native format, no translation), Cloudflare AI Gateway (`.../anthropic`).
2. Pattern 2, both formats on one root: OpenRouter (`/api`), Vercel AI Gateway, LiteLLM unified mode.
3. Local test (2026-11-27): the Anthropic Python SDK, the OpenAI Python SDK and `claude -p` all keep a subpath in the base URL (`.../anthropic` -> `POST /anthropic/v1/messages?beta=true`).
4. This spec uses pattern 1 (D17).

## 3. Paths and listeners

1. Public paths on the main listener (`server.listen`):

   | Path prefix      | API format | Notes                                            |
   | ---------------- | ---------- | ------------------------------------------------ |
   | `/v1/...`        | OpenAI     | Main OpenAI path. Unchanged.                     |
   | `/openai/v1/...` | OpenAI     | Alias of `/v1/...`, same handlers.               |
   | `/anthropic/...` | Anthropic  | Anthropic Messages API, passthrough (section 6). |

2. Client base URLs
   1. OpenAI clients: `https://<gateway>/v1` (or `https://<gateway>/openai/v1`).
   2. Anthropic clients: `https://<gateway>/anthropic`, without `/v1`. The SDK and Claude Code add `/v1/...`.
   3. The README shows both and warns about the common mistake `.../anthropic/v1`. A request to `/anthropic/v1/v1/...` gets a 404 that names the fix.
3. Optional dedicated Anthropic listener, for ingress setups that cannot route or rewrite by path, and for apps that accept only a host:
   ```yaml
   server:
     listen: ${AIF_LISTEN:-:8080}
     anthropic:
       path_prefix: ${AIF_ANTHROPIC_PREFIX:-/anthropic}   # "" = off on the main listener
       listen: ${AIF_ANTHROPIC_LISTEN:-off}               # Anthropic API at the root of its own port
   ```
   1. The dedicated listener serves the Anthropic API at `/v1/...` and `/healthz`, nothing else.
   2. Both listeners share one process, router, key store and metering pipeline.
4. Not planned: Anthropic routes at the root (`/v1/messages`, pattern 2). No path ever serves two shapes.
5. Usage records store the endpoint without the prefix (`/chat/completions`, `/messages`), so `/v1` and `/openai/v1` traffic is counted together.

## 4. Model formats

1. New optional model field `formats`: which API formats may use the alias.
   1. Default for a model on an OpenAI-format provider: `[openai]`.
   2. Default for a model on an `anthropic` provider: `[openai, anthropic]`.
2. A request on a format the model does not list gets 404 `model_not_found`, in that API's error shape.
3. `/v1/models` lists only models with `openai`. `/anthropic/v1/models` lists only models with `anthropic`, in the Anthropic shape (`data[].id`, `type: "model"`, `display_name`, `created_at`, `has_more`, `first_id`, `last_id`).
4. Passthrough model names (`<provider>/<model>`) follow the same defaults as the provider.
5. Translating Anthropic requests to an OpenAI-format provider is out of scope. If added later, it must be opt-in per model, because it loses features (D18).

## 5. Provider type `anthropic`

1. Config
   1. Default `base_url`: `https://api.anthropic.com/v1` (same pattern as `openRouterBaseURL`).
   2. Auth to Anthropic: `x-api-key: <api_key>`.
   3. Optional `workspace_id`, sent as `anthropic-workspace-id`. Needed only for keys not scoped to one workspace.
   4. Required `openai_format: compat | native`: how OpenAI-format requests reach this provider. The gateway refuses to start without it (D19).
2. `openai_format: compat`: `/v1/chat/completions` goes to Anthropic's OpenAI-compatible endpoint.
   1. Limits ([OpenAI SDK compatibility](https://platform.claude.com/docs/en/api/openai-sdk)): Anthropic calls it "not considered a long-term or production-ready solution for most use cases"; no prompt caching; no thinking text; `strict` ignored; `response_format` only `json_schema` with `strict: true`; `temperature` below 1 and non-default `top_p` return 400 on Claude 4.7 and later; `usage.prompt_tokens_details` is always empty, so cached tokens are not metered.
3. `openai_format: native`: section 7.
4. Example config: aliases equal the real model IDs, so Claude Code model discovery keeps them (rule 2.2.3.3). Prices in USD per 1M tokens:

   | Alias               | input | output | cached_input | Note                                         |
   | ------------------- | ----- | ------ | ------------ | -------------------------------------------- |
   | `claude-opus-5-5`   | 4.00  | 20.00  | 0.20         |                                              |
   | `claude-sonnet-5-5` | 2.00  | 10.00  | 0.10         |                                              |
   | `claude-haiku-5-5`  | 0.10  | 0.50   | -            | Up to 100K-token prompts; 0.50 / 2.50 beyond |

## 6. Anthropic Messages API (`/anthropic`)

1. Routes, behind the same `auth` middleware:
   1. `POST /anthropic/v1/messages`
   2. `POST /anthropic/v1/messages/count_tokens`
   3. `GET /anthropic/v1/models`
2. Routing: the `model` field resolves through the router, the key allow-list and `formats` (section 4). Only `anthropic` providers serve this format.
3. Passthrough rules
   1. Body: change only the value of the top-level `model` field, and only when the alias differs from the upstream name. Edit those bytes in place; do not re-encode the body. Everything else is forwarded byte for byte.
   2. Request headers: forward all `anthropic-*` headers, `user-agent` and `x-claude-code-*` headers unchanged. Remove the client's `Authorization` and `x-api-key` (they carry the aif key) and set the upstream `x-api-key`. Also drop the client's `anthropic-workspace-id`: a client must not pick a workspace in the gateway's Anthropic org (the provider's `workspace_id` applies).
   3. Query string: forward it (`?beta=true`).
   4. Response: forward status, body and headers unchanged, including `request-id`, `retry-after`, `x-should-retry` and `anthropic-ratelimit-*`. Stream SSE with a flush after each event; never buffer.
   5. Gateway-made errors (bad key, model not allowed, hook deny) use the Anthropic envelope: `{"type":"error","error":{"type":"...","message":"..."}}`.
4. Metering
   1. Non-stream: read `usage` from the response JSON.
   2. Stream: read `usage` from `message_start` (input and cache tokens) and the last `message_delta` (output tokens), without changing the bytes sent to the client.
   3. Token rule (D20): usage records always use OpenAI meaning. `PromptTokens` = all input, cache included (`input_tokens + cache_read_input_tokens + cache_creation_input_tokens`). `CachedTokens` = `cache_read_input_tokens`.
   4. New usage field `CacheWriteTokens` (= `cache_creation_input_tokens`) and new price field `cache_write` (default 1.25x input, the 5-minute cache write price). Cost = `(prompt - cached - cache_write) x input + cached x cached_input + cache_write x cache_write_price + completion x output`. 1-hour cache writes (2x input) are priced at the same `cache_write` rate for now; see open question 11.3.
   5. `output_tokens_details.thinking_tokens` goes to `ReasoningTokens` (part of `CompletionTokens`, as in OpenAI).
   6. New usage field `Format` (`openai` or `anthropic`).
   7. Record `x-claude-code-session-id` (when present) so the admin UI can group a coding session.
   8. `count_tokens` calls are logged with zero tokens and zero cost (Anthropic does not bill them), and skip the hook.
   9. Mirror the new usage and price fields in `web/app/types/admin.ts`.
5. Pre-request hook (NATS)
   1. New hook config `formats`, default `["openai"]`. Requests in a format not listed skip the hook. An extension gets Anthropic bodies only after it opts in.
   2. The hook request gets a new field `format`. Additive; no released field changes.
   3. `modify` on an Anthropic request is accepted but logged as a warning: a changed body loses prompt caching and can break preserved thinking.
6. Connect guide (README)
   1. Claude Code: `ANTHROPIC_BASE_URL=https://<gateway>/anthropic` and `ANTHROPIC_AUTH_TOKEN=<aif key>`.
   2. Anthropic SDK apps: `base_url="https://<gateway>/anthropic"` and the aif key as the API key.

## 7. Native adapter for OpenAI-format clients

1. With `openai_format: native`, `/v1/chat/completions` to an `anthropic` provider is translated inside `Do` to `POST /v1/messages`, as `provider.go` already plans.
   1. Request: system hoisting, tools, images, `max_tokens`. Fields current models refuse (`temperature`, `top_p`, forced `tool_choice`) get a clear 400 from the gateway.
   2. Response and SSE stream back to OpenAI chat format, with `usage` in the last chunk. `stop_reason: refusal` -> `finish_reason: content_filter`.
   3. Prompt caching on by default: top-level `cache_control: {type: "ephemeral"}`. Provider config `prompt_cache: auto | off`.
2. Use the official Go SDK (`github.com/anthropics/anthropic-sdk-go`) for request and stream types.
3. The example config switches to `openai_format: native` once this ships.

## 8. Credit awareness

1. Detect the "credit balance is too low" error.
   1. OpenAI format: return the gateway error code `provider_credit_exhausted`, not a generic 502.
   2. Anthropic format: pass the upstream error through unchanged (rule 2.2.3.7).
2. Optional `credit` block per account (spec-0004), with a `type`: `monthly` (a plan credit: `usd`, `cycle_day`, `timezone`; it resets on the billing day, not on the 1st), `payg` (a budget per cycle) or `prepaid` (a balance from top-ups recorded in the database). One type per account. Built in task-0010.
   1. Admin UI shows spend this cycle against the credit, from the usage table.
   2. Warn at 80 percent.
3. With the fallback chain (backlog-0004 item 1.3), route a model to a paid provider when credits run out.
4. A NATS event would be a new versioned subject, for example `aif.control.v1.provider_credit_exhausted`. Additive.

## 9. Decisions

Numbers continue from spec-0001 section 9.

| ID  | Decision                                              | Why                                                                                                                  |
| --- | ----------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------- |
| D17 | Pattern 1 paths: `/v1` OpenAI, `/anthropic` Anthropic | Matches most providers and gateways; one shape per path; an ingress can split formats by path or by a dedicated port |
| D18 | Per-model `formats`; lossy translation only by opt-in | One alias list per format; a later translation can never turn a 400 into a silent loss of features                   |
| D19 | `openai_format` is required on `anthropic` providers  | Changing a default later would silently change results, errors and cost                                              |
| D20 | Usage records use OpenAI token meaning                | One meaning for dashboards and cost across both formats                                                              |

## 10. Contract impact

1. Additive. Existing paths, providers, aliases and NATS payloads keep their shape.
   1. New fields: `format` on the hook request; `format`, `cache_write_tokens` and the session ID on usage records; `cache_write` on prices; `formats` on models and hooks; `openai_format`, `workspace_id` on `anthropic` providers.
   2. Existing hook extensions see no Anthropic traffic unless they opt in (rule 6.5.1).
2. Security
   1. The Anthropic key stays in `.env`, like the other provider keys.
   2. The client's aif key is never sent upstream (rule 6.3.2).
3. On acceptance, update spec-0001: section 5 (path table, Anthropic error shape), section 7 (token rule, cache write cost), section 8.1 (hook `format` and `formats`), section 9 (D17-D20).

## 11. Open questions

1. Should `/anthropic/v1/messages/batches` (50 percent cheaper, good for credits) be in task-0008 or later? It needs async metering.
2. Which HTTP status does the "credit balance too low" error use? The docs give only the message text. task-0010 treats a 402, or an Anthropic 400 whose body says "credit balance", as out of credits; confirm with a real exhausted account.
3. Price 1-hour cache writes at 2x input? The usage object splits them (`cache_creation.ephemeral_1h_input_tokens`). Claude Code used only 5-minute writes in the task-0008 test.
