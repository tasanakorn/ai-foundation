# task-0021: Model routing with targets

- Status: done
- Spec: [spec-0005](../specs/spec-0005-model-routing-targets.md)
- Done when: aliases with several targets route and fall back with the spec-0004 policy engine, per API format; cost uses the answering target's price.

## Result

1. Config: `targets` (provider, upstream, price, weight, formats), `policy`, `sticky`, `max_attempts` on a model. The short form (provider + upstream + price) stays valid as one target; both at once is an error. Validation: known providers, no duplicate target, weights (round_robin only, at least one above 0), the anthropic format only on Anthropic-type targets, model formats served by some target.
2. One engine (D32): `provider.Selector` now orders both accounts and targets (priority, smooth weighted round robin, sticky by weighted rendezvous hashing, weight 0 drains).
3. `router.Route.Plan(format, identity)`: targets that serve the request's format, in policy order, skipping targets whose provider has no usable account. `/anthropic` plans only Anthropic-type targets (section 5.3). The hook sees the alias and the first planned target.
4. `router.Route.Call`: tries the plan in order with the spec-0004 rules (before the first byte, retryable outcomes only); the pool reports each outcome through `provider.Trace`. `max_attempts` caps calls across targets and accounts. Each target gets its own model name (OpenAI body, or the Anthropic body with only `model` changed).
5. Usage keeps `model` (the alias) and records `provider`, `upstream_model`, `account` and `attempts` of the answer; cost uses the answering target's price.
6. Storage: migration v7 (model policy, sticky, max_attempts; target weight, formats); export and import with `targets`; the alias round-robin position carries over a reload.
7. Admin `GET /admin/v1/models` adds `policy` and `targets`; the Models page lists every target.
8. Tests: 3:1 over 1000 plans, sticky session, format filter, carry-over, export round trip; end to end: fallback to the second provider with its model name and price, resting target skipped, `/anthropic` on a mixed alias, `max_attempts`, no fallback on 404.
