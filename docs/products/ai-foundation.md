# ai-foundation

A lightweight LLM gateway. It is the shared base layer for AI chatbots and AI agents.

## 1. What it is

1. A minimal core of what LiteLLM and OpenRouter do, in one small Go binary.
2. One OpenAI-compatible API in front of many LLM providers, plus an Anthropic-compatible API (`/anthropic`) for coding agents and Anthropic SDK apps.
3. A base to build custom systems on (multi-tenant SaaS, internal AI platform, per-app billing).

## 2. Goals

1. **Light.** Low memory and CPU. One binary, plus two required services: PostgreSQL (the only store) and NATS (events and extensions) ([change-0004](../changes/change-0004-nats-required-events-and-hooks.md), [change-0005](../changes/change-0005-postgres-required-sqlite-removed.md)).
   1. Target: < 50 MB RAM at idle, < 5 ms added latency (p50) per request.
2. **Connectivity.** Reach the major LLMs through:
   1. OpenRouter (hundreds of models with one key).
   2. Any direct OpenAI-compatible API (OpenAI, Groq, Together, vLLM, Ollama, LM Studio, …).
   3. Anthropic directly, including the monthly API credits of Claude Max, Ultra and Team plans.
3. **Metering.** Count tokens and cost for every request, including streams.
   1. **Credit per provider account.** Each account has one credit type: a monthly plan credit, a pay-as-you-go budget, or a prepaid balance. The admin sees how much is used or left, with a warning at 80 percent. Prepaid top-ups are recorded in the database ([change-0008](../changes/change-0008-credit-per-account.md)).
4. **API keys.** Simple keys that are easy to map to tenants, apps, and end users.
5. **Built-in admin.** Own admin accounts (email + password, roles), so the gateway works without any outside identity system. Outside logins ("login with …") can be added later. Admin API on localhost by default, switchable to public. Web UI in Vue/Nuxt.
6. **Extensible from outside.** New features live in separate services (any language) and talk to the core over NATS (events, hooks) or HTTP.
7. **No big rewrites.** Start small, but put the seams (interfaces, contracts, schema) in place now.

## 3. Non-goals (core)

1. Not a full LiteLLM clone. No 100+ native provider SDKs in core.
2. No built-in billing, invoicing, guardrails, prompt management, caching, or RAG in core. These are extensions.
3. No end-user accounts in core. Core has only admin accounts, to operate the gateway. End-user identity lives in the apps on top (core sees only `end_user` labels).

## 4. Personas

| Persona           | Needs                                                                              |
| ----------------- | ---------------------------------------------------------------------------------- |
| App developer     | One endpoint and one key. Works with the OpenAI SDK.                               |
| Platform builder  | Admin API to create keys per tenant/app. Usage data for billing.                   |
| Operator          | Small footprint, easy deploy, clear logs. Sees each account's credit at a glance.  |
| Coding agent user | Point Claude Code or an Anthropic SDK app at the gateway with one URL and one key. |

## 5. Docs

1. Architecture: [spec-0001](../specs/spec-0001-gateway-core-architecture.md). Anthropic API and plan credits: [spec-0002](../specs/spec-0002-anthropic-api-and-credits.md) (draft).
2. Provider accounts and credit: [spec-0004](../specs/spec-0004-provider-accounts-and-policy.md). Catalog in the database (the YAML file only bootstraps it): [spec-0006](../specs/spec-0006-catalog-database-hot-reload.md).
3. Work: [tasks index](../tasks/INDEX.md). Ideas: [backlog index](../backlogs/INDEX.md).
