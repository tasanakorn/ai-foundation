# task-0020: Provider accounts and selection policy

- Status: done
- Spec: [spec-0004](../specs/spec-0004-provider-accounts-and-policy.md)
- Done when: spec-0004 section 12 is built on the catalog: accounts, priority and weighted round robin, sticky, cooldowns, failover, usage account and attempts, metrics, GET /admin/v1/providers; cooldowns and round-robin positions carry over a catalog reload (spec-0006 section 5.6, moved here from task-0017).

## Result

1. Config: `accounts` (name, api_key, weight, base_url, headers, workspace_id), `policy`, `sticky`, `max_attempts`, `cooldown`. `api_key` stays valid as one account named `default`; both at once is an error. Validation covers names, weights (0-100, round_robin only, at least one above 0) and sticky (round_robin only).
2. `provider.Pool` (and `NativePool` for Anthropic) wraps one client per account:
   1. `priority`: healthy accounts in config order.
   2. `round_robin`: smooth weighted round robin picks the first account (3:1 gives a a b a); the others follow as fallbacks; weight 0 is only a last fallback.
   3. `sticky`: weighted rendezvous hashing on the key ID, end user or session (falls back to the key ID).
   4. Accounts in cooldown are skipped; when all rest, the one whose rest ends first is tried.
   5. Failover before the first byte, by the table in spec-0004 section 5.3. An Anthropic 400 body is read (up to 64 KiB) to spot "credit balance" and given back unchanged. The client gets the first success or the last failure. A canceled request or a spent deadline stops the loop and rests no account.
3. Per-request `Trace` and `Identity` through the context; usage records get `account` and `attempts` (migration v6; NATS payload fields added, `aif.usage.v1` stays). Usage filter `account` and group `account` (`provider/account`).
4. Metrics: `aif_upstream_attempts_total{provider,account,outcome}`, `aif_account_cooldown{provider,account}`.
5. `GET /admin/v1/providers` (any role): policy and per-account state of the answering node, never a key. Web: Providers page, account in the usage table and grouping; types in `web/app/types/admin.ts`.
6. Catalog: accounts and policy stored (migration v6), exported and imported; cooldowns and round-robin positions carry over a reload by account name (from task-0017).
7. Tests: ratio over 1000 picks and the smooth cycle, cooldown share, drain, sticky spread (75% +- 4% at 3:1) and stability when an account is added, each failover rule, last failure unchanged, max_attempts, cancel, Retry-After forms, carry-over; end to end through the gateway (failover, usage, metrics, providers API, reload).
