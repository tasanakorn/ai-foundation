# task-0010: Plan credit awareness

- Status: todo
- Spec: [spec-0002](../specs/spec-0002-anthropic-api-and-credits.md) section 8
- Depends on: [task-0008](task-0008-anthropic-messages-api.md)
- Done when: running out of credits gives a clear error, and the admin UI shows spend this billing cycle against the plan credit.

## 1. Scope

1. Detect the "credit balance is too low" error: `provider_credit_exhausted` on OpenAI format; unchanged pass-through on Anthropic format.
2. Provider `credit` block: `monthly_usd`, `cycle_day`. Admin API and UI: spend this cycle, warning at 80 percent.
3. Find out the HTTP status of the error (spec-0002 open question 11.2).
4. Out of scope: fallback to a paid provider (needs the fallback chain, backlog-0004 item 1.3).

## 2. Verification

1. Fake-upstream test for the error mapping.
2. Store test for the per-cycle spend sum across a `cycle_day` boundary.
3. `make -C gateway vet test` and `cd web && pnpm typecheck`.
