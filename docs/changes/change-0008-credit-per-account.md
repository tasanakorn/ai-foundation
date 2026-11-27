# change-0008: Credit per provider account

- Status: applied (released in v0.3.0)
- Date: 2026-11-27
- Related: [task-0010](../tasks/task-0010-anthropic-credit-awareness.md); [spec-0002](../specs/spec-0002-anthropic-api-and-credits.md) section 8; [spec-0004](../specs/spec-0004-provider-accounts-and-policy.md); [spec-0006](../specs/spec-0006-catalog-database-hot-reload.md); [product](../products/ai-foundation.md) goal 3.1

## 1. What changed

1. Each provider account can have one `credit`, with a `type`:
   1. `monthly`: a plan credit that resets each cycle (`usd`, `cycle_day`, `timezone`).
   2. `payg`: pay-as-you-go; `usd` is a budget per cycle, for the warning only.
   3. `prepaid`: a balance raised by top-ups.
2. Prepaid top-ups are recorded in a new database table, `credit_ledger`. They are never in the YAML file or the catalog.
   1. Append-only: a negative entry corrects the balance.
   2. Added with `POST /admin/v1/credit-entries`, `aif credit add`, or the Providers page. Listed with `GET /admin/v1/credit-entries` and `aif credit list`.
3. `GET /admin/v1/providers` and the Providers page show each account's spend against its credit, with a warning at 80 percent.
4. OpenAI-format clients get `402 provider_credit_exhausted` when the last account tried is out of credits. The Anthropic API is unchanged.
5. The usage API filters by `provider`.
6. Migration v8: credit columns on `catalog_accounts`, the `credit_ledger` table, and the index `idx_usage_account`.
7. The binary carries the time zone database (`time/tzdata`, about 450 KB).

## 2. Why

1. The owner asked for a ledger per provider account: several Claude plans and paid accounts each have their own money, and the admin needs to see how much is left.
2. Decisions from the owner (2026-11-27):
   1. One credit type per account. An org with several kinds of credit is several accounts.
   2. The database is the source of truth; the YAML file only bootstraps.
   3. Top-ups go to the database only.

## 3. Impact

1. Additive. Accounts without `credit` work as before. No NATS subject or payload changed.
2. The only behavior change for clients: an out-of-credits error on the OpenAI format is now `402 provider_credit_exhausted` instead of the provider's own 400 or 402.
3. Spend is the gateway's metered cost, so it can differ a little from the provider's bill.

## 4. Roll back

1. An older binary refuses a database at schema v8 ("database schema v8 is newer than this binary"). Before starting the older binary, undo migration v8 by hand. This deletes all prepaid ledger entries, so export them first with `aif credit list`:

   ```sql
   BEGIN;
   DROP INDEX IF EXISTS idx_usage_account;
   DROP TABLE IF EXISTS credit_ledger;
   ALTER TABLE catalog_accounts
     DROP COLUMN credit_type, DROP COLUMN credit_usd,
     DROP COLUMN credit_cycle_day, DROP COLUMN credit_timezone;
   DELETE FROM schema_migrations WHERE version = 8;
   COMMIT;
   ```

2. Then revert the commit and deploy the older binary.
