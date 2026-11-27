# spec-0004: Several accounts per provider, with a selection policy

- Status: draft
- Owner: project owner
- Related: [spec-0001](spec-0001-gateway-core-architecture.md) sections 3, 5.1, 7, 8.1; [spec-0002](spec-0002-anthropic-api-and-credits.md) section 2.1 (plan credits); [backlog-0004](../backlogs/backlog-0004-native-provider-adapters.md) item 3; [task-0010](../tasks/task-0010-anthropic-credit-awareness.md); [spec-0005](spec-0005-model-routing-targets.md) (targets); [spec-0006](spec-0006-catalog-database-hot-reload.md) (the catalog, including accounts, moves to the database)

## 1. Summary

1. Today a provider has one `api_key`. This spec lets one provider hold several **accounts** (keys), and picks one per request with a simple **policy**: `priority` (fallback in order) or `round_robin` (optionally weighted, for example 3:1).
2. When an account fails in a way another account can fix (rate limit, overload, outage, bad key, no credits), the gateway tries the next account before it answers the client.
3. Clients, model aliases and keys do not change. One alias still points at one provider.

## 2. Why

1. Several Claude plans (Max, Ultra, Team) each link one Console org with its own monthly credits (spec-0002 section 2.1). One gateway should spend them all, in a chosen order.
2. Separate budgets: a team key first, a shared key as the fallback.
3. Resilience: a revoked key or an org-level outage should not stop every request.
4. Note: use only accounts you are entitled to use, and follow each provider's terms. The feature is for budgets and resilience, not to get around one account's rate limits where the terms forbid that.

## 3. Config

```yaml
providers:
  - name: anthropic
    type: anthropic
    openai_format: compat
    policy: priority            # priority (default) | round_robin
    sticky: key                 # round_robin only: none (default) | key | end_user | session
    max_attempts: 3             # accounts tried per request; default = number of accounts
    cooldown: 30s               # default rest after a retryable failure
    accounts:
      - name: plan-a            # shown in usage records, logs and metrics; never the key
        api_key: ${ANTHROPIC_KEY_A}
      - name: plan-b
        api_key: ${ANTHROPIC_KEY_B}
        workspace_id: wrkspc_...   # per-account fields override the provider's
```

Weighted round robin, 3:1 (plan-a gets 3 of every 4 requests):

```yaml
    policy: round_robin
    accounts:
      - { name: plan-a, api_key: ${ANTHROPIC_KEY_A}, weight: 3 }
      - { name: plan-b, api_key: ${ANTHROPIC_KEY_B}, weight: 1 }
```

1. Per-account fields: `name` (required, unique in the provider, `a-z0-9-`), `api_key`, `weight` (round_robin only, see 4.2), and the optional overrides `base_url`, `headers` (merged over the provider's) and `workspace_id` (anthropic only).
2. `api_key` at provider level stays valid: it means one account named `default`. A provider has `api_key` or `accounts`, not both.
3. `policy`, `sticky`, `max_attempts` and `cooldown` are optional. With one account they do nothing.

## 4. Policies

| Policy        | Order of accounts for one request                                                                    | Use it for                                       |
| ------------- | ---------------------------------------------------------------------------------------------------- | ------------------------------------------------ |
| `priority`    | Config order. Healthy accounts first; an account in cooldown is skipped                              | Spend account A first, then B (credits, budgets) |
| `round_robin` | Weighted round robin: each account gets requests in proportion to its `weight` (default 1, so equal) | Spread load over accounts, evenly or by a ratio  |

### 4.1 Fallback inside one request

1. The policy picks the **first** account. If it fails (section 5), the next attempts go to the other healthy accounts: in config order for `priority`, in the policy's order for `round_robin`.
2. An account in cooldown is skipped. If every account is in cooldown, the gateway still tries the one whose cooldown ends first (never refuse without trying).

### 4.2 Weights

1. `weight` is an integer from 0 to 100, default 1. Only `round_robin` reads it; setting it with `priority` is a config error, so nobody expects it to work there.
2. Ratio, not percent: weights 3 and 1 mean 75% and 25%. Weights 2, 2, 1 mean 40%, 40%, 20%.
3. Algorithm: smooth weighted round robin (the nginx method). It spreads picks evenly inside each cycle (for 3:1: A, A, B, A, not A, A, A, B), so no account gets bursts.
4. Cooldown: an account in cooldown leaves the cycle. Its share goes to the others in proportion to their weights, and it comes back when the cooldown ends.
5. `weight: 0` drains an account: round_robin never picks it first, and it is only a last fallback when every other account fails. Use it before removing a key.
6. The ratio holds per node and over many requests. With few requests, or several nodes, the real split is close to the ratio but not exact.

### 4.3 Sticky

1. `sticky` (round_robin only) picks the first account by the request's identity instead of the counter:
   1. `key`: the gateway key ID. `end_user`: the end-user label. `session`: `X-Claude-Code-Session-Id`, else the key ID.
   2. Why: Anthropic prompt caches belong to one org or workspace. Plain round_robin sends the next turn of a conversation to another account, so the cache misses and input costs up to 10 times more. `priority` is naturally sticky.
2. Sticky respects weights: it uses weighted rendezvous hashing, so with 3:1 about 75% of keys (or sessions) land on account A. Adding or removing an account moves only the identities that must move.
3. If the sticky account is in cooldown, that request falls back as in 4.1, and the identity returns to its account after the cooldown.

### 4.4 State

1. State (round-robin position, cooldowns) is per node, in memory, like the sign-in throttle. Nodes do not share it. This is fine for 1-3 nodes; shared state is out of scope.

## 5. Failover

1. The gateway reads the whole request body before it calls upstream (true today), so it can send the same bytes to another account.
2. Failover happens only **before the first byte reaches the client**: the decision is made on the upstream status and headers. A stream that breaks in the middle is not retried.
3. Outcome per attempt:

   | Upstream result                                                          | Next account? | Cooldown of this account                       |
   | ------------------------------------------------------------------------ | ------------- | ---------------------------------------------- |
   | 2xx                                                                      | no (done)     | cleared                                        |
   | 429                                                                      | yes           | `Retry-After` (capped at 10m), else `cooldown` |
   | 500, 502, 503, 504, 529, connection error, timeout before headers        | yes           | `cooldown`                                     |
   | 401, 403 (key revoked or wrong)                                          | yes           | 10m, logged as an error                        |
   | "No credits" (Anthropic: 400 with a credit-balance message; others: 402) | yes           | 1h (open question 11.2)                        |
   | Other 4xx (bad request, model not found, too large)                      | no            | none: the client's request is wrong            |

4. The client gets the first success, or the **last** failure unchanged (same body and status as today).
5. Total time stays within `server.upstream_timeout`. Each attempt gets what is left.
6. The pre-request hook runs once, before the first attempt. Retries do not call it again.

## 6. Metering and events

1. `UsageRecord` gets `account` (account name) and `attempts` (number of upstream calls). Migration v4 adds both columns with defaults `''` and `1`.
2. The NATS usage payload gets the same two fields. This is additive (spec-0001 section 8.1 rules): `aif.usage.v1` stays.
3. Tokens and cost come from the successful attempt only. Failed attempts cost nothing at the provider, except a stream cut in the middle, which is metered as today.
4. Logs: one line per failed attempt (provider, account, status, cooldown), then the usual request line with `account` and `attempts`.

## 7. Metrics (adds to spec-0003 section 4)

| Name                          | Type    | Labels                           | Meaning                                                             |
| ----------------------------- | ------- | -------------------------------- | ------------------------------------------------------------------- |
| `aif_upstream_attempts_total` | counter | `provider`, `account`, `outcome` | `ok`, `rate_limited`, `error`, `auth`, `no_credits`, `client_error` |
| `aif_account_cooldown`        | gauge   | `provider`, `account`            | 1 while the account rests                                           |

## 8. Admin API and UI

1. `GET /admin/v1/providers` (new, any role): each provider with its policy and accounts: `name`, `state` (`ok` or `cooldown`), `cooldown_until`, `last_error` (status and short text), per node. Never the key.
2. Usage list and filters get `account`. The web UI shows it in the usage table and a small "Providers" page (types in `web/app/types/admin.ts` change with the API).

## 9. Design in code

1. `provider.Build` makes one provider object per account (the current types, unchanged inside), then wraps them in a `pool` that implements `Provider` and `Native`.
2. `pool.Do` and `pool.Native` run the policy and the failover loop, and return the response plus the account used and the attempt count (for example through a small result struct, or the request context).
3. Router, model aliases, passthrough (`<provider>/<model>`) and both API formats work unchanged on top of a pool.

## 10. Decisions

| ID  | Decision                                                                                    | Why                                                              |
| --- | ------------------------------------------------------------------------------------------- | ---------------------------------------------------------------- |
| D25 | Accounts live inside one provider; an alias still names one provider                        | Small change; no model-level routing yet                         |
| D26 | Two policies only: `priority` (default) and `round_robin` (weighted, smooth), plus `sticky` | Covers credits-first, even load and ratios; easy to reason about |
| D27 | Fail over only before the first byte to the client                                          | A started stream cannot be replayed safely                       |
| D28 | Retry only on errors another account can fix (table 5.3)                                    | A bad request fails the same on every account                    |
| D29 | Per-node, in-memory state                                                                   | No new shared store; good enough for a few nodes                 |
| D30 | `account` and `attempts` are additive fields in DB and NATS v1                              | Keeps the public contract                                        |

## 11. Open questions

1. Moved: cross-provider fallback is [spec-0005](spec-0005-model-routing-targets.md) (model routing with targets).
2. How to detect "out of credits" reliably per provider, and how long to rest: a fixed 1h, or until the next billing cycle (task-0010 credit awareness)?
3. Resolved: weights are part of `round_robin` (section 4.2).
4. `weight: 0` drains an account in round_robin (4.2.5). Do we also need a drain switch for `priority`, or in the admin API without a restart?

## 12. Tasks (when accepted)

1. Config and validation (`accounts`, `weight`, `policy`, `sticky`, `max_attempts`, `cooldown`), old `api_key` kept.
2. `provider.pool` with both policies, smooth weighted round robin, weighted rendezvous hashing for sticky, cooldowns and failover; unit tests with a fake upstream per account (ratio over 1000 picks, cooldown share, drain).
3. Usage `account` and `attempts`: migration v4, store, NATS payload, admin usage API, web types.
4. Metrics, `GET /admin/v1/providers`, web Providers page.
5. Docs: example config, README, spec-0001 cross-links.
