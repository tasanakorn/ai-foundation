# task-0010: Credit awareness per provider account

- Status: done
- Spec: [spec-0002](../specs/spec-0002-anthropic-api-and-credits.md) section 8; [spec-0004](../specs/spec-0004-provider-accounts-and-policy.md) (accounts); [spec-0006](../specs/spec-0006-catalog-database-hot-reload.md) (catalog in the database)
- Change: [change-0008](../changes/change-0008-credit-per-account.md)
- Depends on: [task-0008](task-0008-anthropic-messages-api.md), [task-0020](task-0020-provider-accounts-policy.md)
- Done when: running out of credits gives a clear error, and the admin UI shows each account's spend against its credit: per cycle for plan credits and budgets, and as a balance for prepaid accounts.

## 1. Decisions (2026-11-27)

1. The credit is per provider account, not per provider: one provider holds several accounts (spec-0004), and each has its own money.
2. One credit type per account. An organization with several kinds of credit (for example a plan credit and purchased credits) is set up as several accounts.
3. The database is the source of truth. The YAML file only bootstraps the catalog.
4. Prepaid top-ups are recorded only in the database (a ledger), never in the YAML file or the catalog.

## 2. Credit types

| Type      | Meaning                                              | Fields                                         | Used against                          |
| --------- | ---------------------------------------------------- | ---------------------------------------------- | ------------------------------------- |
| `monthly` | Plan credit that resets each cycle and expires       | `usd`, `cycle_day` (1-31, 1), `timezone` (UTC) | Spend this cycle                      |
| `payg`    | Pay-as-you-go, billed later; `usd` is a budget       | same as monthly                                | Spend this cycle                      |
| `prepaid` | A balance that only top-ups raise; no reset          | none in the catalog                            | Spend since the first ledger entry    |

```yaml
providers:
  - name: anthropic
    type: anthropic
    openai_format: compat
    accounts:
      - name: max-a                      # Max plan credit
        api_key: ${ANTHROPIC_KEY_A}
        credit: { type: monthly, usd: 100, cycle_day: 5, timezone: Asia/Bangkok }
      - name: wallet-b                   # purchased credits
        api_key: ${ANTHROPIC_KEY_B}
        credit: { type: prepaid }
      - name: team-c                     # card billing with a budget
        api_key: ${ANTHROPIC_KEY_C}
        credit: { type: payg, usd: 300 }
```

1. A cycle starts at 00:00 on `cycle_day` in `timezone` (IANA name). In a month without that day it starts on the month's last day. The binary carries the time zone database.
2. A `credit` on the provider is the default of every account that has none.
3. `prepaid` takes no `usd`, `cycle_day` or `timezone`: its money is the ledger.

## 3. Prepaid ledger

1. Table `credit_ledger`: provider, account, `usd`, `note`, `at` (when the money arrived), `created_at`, `actor`. It is not part of the catalog: catalog export leaves it out, and catalog import does not touch it.
2. Append-only. Entries are never changed or deleted; a negative amount corrects the balance (for example to match the provider console).
3. An entry needs an account that exists in the catalog with type `prepaid`. `at` may be in the past, not in the future.
4. Balance = sum of entries - spend since the earliest `at`.
5. Ways to add: `POST /admin/v1/credit-entries` (admin role), `aif credit add -provider P -account A -usd 50 [-note] [-at]`, and the Providers page.
6. Entries match the account by provider and account name. Renaming an account in the catalog leaves its old entries under the old name.

## 4. Spend and levels

1. Spend is the sum of `cost_usd` of the usage records of that provider and account. All nodes write one usage table, so it counts every node.
2. It is the gateway's metered cost, so it can differ a little from the provider's bill.
3. Level: `ok`; `warning` from 80 percent; `exhausted` from 100 percent. A prepaid account with no entry is `exhausted`.

## 5. Out of scope

1. Stop using an account before its credit runs out (a spend limit). The provider's own error already moves requests to the next account.
2. Rest an out-of-credits account until its cycle ends (today: 1h, spec-0004 open question 11.2).
3. A NATS event for exhausted credit (spec-0002 section 8.4).
4. Editing an account's credit in the admin UI: that needs the catalog write API (spec-0006 section 8). Today: `aif catalog import`, or the file in file mode.

## Result

1. Config: `credit` with `type`, `usd`, `cycle_day`, `timezone` (`config.Credit`, `Credit.Cycle` in the credit's time zone), with defaults and validation; `time/tzdata` in the binary.
2. Storage: migration v8 adds `credit_type`, `credit_usd`, `credit_cycle_day` and `credit_timezone` to `catalog_accounts`, the table `credit_ledger`, and the index `idx_usage_account (provider, account, created_at)`. Export and import keep the credit, not the ledger.
3. Admin API:
   1. `GET /admin/v1/providers`: each account with a credit gets `credit`: `type`, the cycle (`usd`, `cycle_day`, `timezone`, `cycle_start`, `cycle_end`) or the ledger (`topups_usd`, `balance_usd`, `since`), plus `spent_usd`, `used_percent` and `level`.
   2. `GET /admin/v1/credit-entries[?provider=P&account=A]` (any role) and `POST /admin/v1/credit-entries` (admin role).
   3. The usage API filters by `provider`.
4. CLI: `aif credit add | list`.
5. Web: the Providers page shows each credit by type, and a ledger dialog for prepaid accounts (record a top-up or correction as admin; read as viewer). Types in `web/app/types/admin.ts`.
6. OpenAI format: when the last account tried is out of credits (402, or an Anthropic 400 that says "credit balance"), the client gets `402` with code `provider_credit_exhausted`. The usage record keeps 402 and the account. The Anthropic API passes the provider's error through unchanged.
7. Tests: cycles across month ends, a leap year, Bangkok and a summer-time change; defaults and validation per type; levels; ledger store; database and export round trips; end to end: monthly spend, prepaid balance after a top-up and a correction, refused entries, and the credit error.

## Verification

1. `make -C gateway vet test` and `cd web && pnpm typecheck`.
2. Open: the real HTTP status of Anthropic's "credit balance too low" error is not confirmed yet (spec-0002 open question 11.2).
