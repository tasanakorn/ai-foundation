# spec-0005: Model routing with targets

- Status: draft
- Owner: project owner
- Related: [spec-0001](spec-0001-gateway-core-architecture.md) section 5.1; [spec-0004](spec-0004-provider-accounts-and-policy.md) (accounts and the policy rules reused here); [spec-0006](spec-0006-catalog-database-hot-reload.md) (where this config lives); [backlog-0004](../backlogs/backlog-0004-native-provider-adapters.md) item 3

## 1. Summary

1. Today a model alias points at one provider and one upstream name. This spec lets an alias have several **targets**. Each target is a provider plus the upstream model name and price on that provider.
2. The alias picks a target with the same policy rules as spec-0004 (`priority`, `round_robin` with `weight`, `sticky`), and falls back to the next target on the same errors.
3. Targets may use providers of different types (Anthropic, OpenRouter, any OpenAI-compatible API). Accounts stay inside one provider and one type (spec-0004).

## 2. Config

```yaml
models:
  - name: claude-sonnet
    policy: priority                 # priority (default) | round_robin
    targets:
      - { provider: anthropic,  upstream: claude-sonnet-5-5,           price: { ... } }
      - { provider: openrouter, upstream: anthropic/claude-sonnet-5-5, price: { ... } }

  - name: coder
    policy: round_robin
    sticky: session
    targets:
      - { provider: anthropic, upstream: claude-sonnet-5-5, weight: 3, price: { ... } }
      - { provider: other-llm, upstream: vendor-coder-1,    weight: 1, price: { ... } }

  # The old one-target form stays valid: provider + upstream + price at model level.
  - { name: mini, provider: openrouter, upstream: openai/gpt-4o-mini, price: { ... } }
```

1. A model has `targets`, or the old `provider` / `upstream` / `price` fields, not both. The old form is one target.
2. Target fields: `provider` (required), `upstream` (default: the alias name), `price`, `weight` (round_robin only, 0-100, default 1), `formats` (default: what the provider supports, spec-0002 section 4).
3. Model fields: `name`, `policy`, `sticky`, `max_attempts` (default: number of targets), `formats` (the alias serves a format when at least one target does).

## 3. Selection

1. Two levels, one engine:
   1. The alias picks a target (this spec).
   2. That target's provider picks an account (spec-0004).
2. Only targets that serve the request's API format take part. A `/anthropic` request uses only targets on `type: anthropic` providers. The OpenAI-format `/v1` path may use any target.
3. A target is in cooldown when its provider has no usable account left: every account is in cooldown, or every account returned an error that another target could fix. The alias then moves to its next target.
4. Failover rules are those of spec-0004 section 5 (before the first byte only; retryable errors only). The total time stays within `server.upstream_timeout`, and `max_attempts` counts calls across both levels.
5. The key's model allow-list checks the **alias** name, as today. The pre-request hook runs once, sees the alias and the first chosen target, and may change the body. A `modify` that changes `model` resolves again from the start.

## 4. Metering

1. The usage record keeps `model` (the alias) and fills `provider`, `account` and `upstream_model` from the target that answered, plus `attempts`.
2. Cost uses the price of the target that answered. Each target has its own price, because the same model can cost different amounts on different providers.
3. Metrics: `aif_upstream_attempts_total` (spec-0004 section 7) already shows the target through its `provider` label. No new family.

## 5. Things to watch

1. Different targets can answer differently (tokenizer, limits, features). Fallback across providers is for availability, not for identical output.
2. Prompt caches do not cross providers or orgs. Use `priority`, or `sticky` with round_robin, for long conversations.
3. The Anthropic passthrough keeps the client's body. Falling back to an OpenAI-format target would need translation, which is out of scope (spec-0002 D18). So `/anthropic` falls back only between Anthropic-type targets.

## 6. Decisions

| ID  | Decision                                                                | Why                                             |
| --- | ----------------------------------------------------------------------- | ----------------------------------------------- |
| D31 | An alias has targets (provider + upstream + price); types may mix there | Model names and prices differ per backend       |
| D32 | One policy engine for targets and accounts                              | Same rules to learn; one implementation to test |
| D33 | The allow-list and the hook see the alias, not the target               | Keys grant models, not backends                 |

## 7. Open questions

1. Should a target be able to override request settings (for example `max_tokens` caps) per backend?
2. Should the admin UI show a per-target success rate (from metrics) next to each alias?
