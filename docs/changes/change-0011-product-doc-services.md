# change-0011: Product document names the required services

- Status: applied (on merge of its pull request)
- Date: 2026-11-27
- Related: [task-0023](../tasks/task-0023-product-doc-services.md); [product](../products/ai-foundation.md) goals 1 and 6; [change-0004](change-0004-nats-required-events-and-hooks.md); [change-0005](change-0005-postgres-required-sqlite-removed.md)

## 1. What changed

1. Product goal 1 said "No required external services". It now says: one binary, plus PostgreSQL (the only store) and NATS (events and extensions), both required.
2. Product goal 6 said extensions talk to the core "over REST or gRPC". It now says "over NATS (events, hooks) or HTTP", as built in change-0004.

## 2. Why

1. Both lines were written before change-0004 (NATS required) and change-0005 (PostgreSQL required, SQLite removed), and were not updated then.

## 3. Impact

1. Docs only. The product document now matches what is built. No code, config, API, data or NATS contract changes.
2. Spec: none. spec-0001 already says both services are required (D14, D15).

## 4. Roll back

1. Revert the commit.
