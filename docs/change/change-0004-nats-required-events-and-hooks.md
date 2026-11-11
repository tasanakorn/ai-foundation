# change-0004: NATS required, usage events, hooks, control events

- Status: applied
- Date: 2026-11-11
- Related: [spec-0001](../specs/spec-0001-gateway-core-architecture.md) §8, D13, D14; [task-0003](../tasks/task-0003-nats-events-and-hooks.md)

## 1. What changed

1. **NATS is now required.** The gateway connects at startup and exits if it cannot. New config section `nats` (all keys have defaults).
2. Usage records are published to JetStream stream `AIF_USAGE` on `aif.usage.v1.<tenant>.<app>`, next to the store.
3. New pre-request hook over NATS request/reply: allow, deny, or modify. Off unless `nats.hooks.pre_request.subject` is set. The dev config turns it on with `fail_open: true`.
4. Key revocation publishes `aif.control.v1.key_revoked`; all nodes drop the key from their auth cache.
5. `/healthz` answers 503 while NATS is disconnected.
6. New packages: `bus`, `hook`, `app` (shared wiring), `natstest` (test-only).
7. All gateway end-to-end tests now run on the real wiring with an in-process NATS server.
8. SQLite: safe concurrent open of one file by several processes (`BEGIN IMMEDIATE` migrations, retry on `SQLITE_BUSY`).
9. Dev loop: `make dev` starts Postgres + NATS first. `make dev-all` and `--services` are removed; `--no-services` skips Docker.
10. New `extensions/example-budget/` (Python): daily token budget per tenant.

## 2. Why

1. The user decided NATS should not be optional (D14). One code path, and multi-node setups are correct by default.
2. Extensions in any language need only a NATS client (D9, D13).

## 3. Impact

1. A gateway alone no longer runs: it needs a NATS server with JetStream (or `nats.usage.jetstream: false`).
2. Existing configs keep working: the `nats` section is optional in the file and defaults to `nats://127.0.0.1:4222`.
3. No admin API or database schema change.

## 4. Roll back

1. Revert this change. Configs with a `nats` section would then fail to parse only if strict parsing is added; today unknown keys are ignored.
