# backlog-0004: Native provider adapters and more endpoints

- Status: open
- Source: initial scope review
- Promoted to: [spec-0002](../specs/spec-0002-anthropic-api-and-credits.md) (draft) for item 1.1 (Anthropic) and the Anthropic part of item 2 (`/anthropic/v1/messages`). Other items stay open.

## 1. Idea

1. Native adapters behind `provider.Provider` (translate OpenAI ⇄ native inside `Do`):
   1. Anthropic Messages.
   2. Google Gemini.
2. More public endpoints: `/v1/responses`, `/v1/messages` (Anthropic-compatible inbound), images, audio.
3. Router features: fallback chain, retries on 429/5xx, weighted load balancing across providers.
