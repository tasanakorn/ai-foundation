# task-0003: NATS events and hooks

- Status: done
- Spec: [spec-0001](../specs/spec-0001-gateway-core-architecture.md) §8, §8.1, §8.2, D13, D14
- Change: [change-0004](../changes/change-0004-nats-required-events-and-hooks.md)
- Done when: an extension in any language, connected only to NATS, can (a) receive every usage record, (b) allow, deny, or modify a request before it goes upstream, and (c) a key revoked on one gateway node stops working on all nodes within 1 s.

## 1. Scope

1. **NATS is required** (D14). The gateway exits at startup if NATS is unreachable, then reconnects forever.
2. **Config** (all keys have defaults; see `gateway/aif.example.yaml`):

   ```yaml
   nats:
     url: ${NATS_URL:-nats://127.0.0.1:4222}
     creds: ${NATS_CREDS:-}
     connect_timeout: 5s
     usage:   { subject_prefix: aif.usage.v1, jetstream: true, stream: AIF_USAGE, max_age: 720h }
     control: { subject_prefix: aif.control.v1 }
     hooks:
       pre_request: { subject: "", timeout: 200ms, fail_open: false, models: ["*"] }   # "" = off
   ```

3. **Usage sink** (`bus.UsageSink`, a `metering.Sink`): one JSON record per request to `aif.usage.v1.<tenant>.<app>`, JetStream ack, `Nats-Msg-Id` = request ID, retry with backoff, drops counted.
4. **Pre-request hook** (`hook.Hook`, `hook.Guard`, NATS transport `bus.PreRequestHook`): allow / deny / modify, timeout, model filter, fail-open or fail-closed. Big bodies are sent without the body.
5. **Control events**: `aif.control.v1.key_revoked` on revoke; every node drops the key. The cache is cleared on reconnect.
6. **Health**: `/healthz` answers 503 while NATS is disconnected.
7. **Dev loop**: `make dev` starts Postgres + NATS first. `--no-services` if NATS runs elsewhere.
8. **Example extension**: `extensions/example-budget/budget.py` (Python, `nats-py`): a daily token budget per tenant. It replays today's usage from JetStream on start.

## 2. Code

| Path                                    | What                                                        |
| --------------------------------------- | ----------------------------------------------------------- |
| `gateway/internal/bus/`                 | NATS connection, usage sink, control events, hook transport |
| `gateway/internal/hook/`                | Wire types, `Hook` interface, `Guard` policy                |
| `gateway/internal/app/`                 | Wiring shared by `cmd/aif` and the e2e tests                |
| `gateway/internal/natstest/`            | In-process NATS server for tests (test-only)                |
| `gateway/internal/gateway/nats_test.go` | End-to-end tests for usage, hooks, revoke, health           |
| `extensions/example-budget/`            | Example extension                                           |

## 3. Verification

1. `make -C gateway vet test`: all pass with `-race`. Tests start an in-process NATS server, so no Docker is needed.
2. Live run: NATS 2.15.2 in Docker, two gateway nodes sharing one SQLite file, llama.cpp (Qwen2.5-0.5B), the Python extension with `acme=40` tokens.

   | Step | Check                                        | Result                                        |
   | ---- | -------------------------------------------- | --------------------------------------------- |
   | 1    | No extension running, `fail_open: false`     | `503 hook_unavailable`                        |
   | 2    | Extension running, first request on node 1   | `200`, 41 tokens                              |
   | 3    | Same tenant on node 2                        | `429` "daily token budget used up (41/40)"    |
   | 4    | Extension restarted (replays today's stream) | still `429 budget_exceeded`                   |
   | 5    | Revoke on node 1, node 2 cache warm          | node 2 answers `401` after 3 ms               |
   | 6    | Stream `AIF_USAGE` vs. store                 | 4 messages = 4 stored records                 |
   | 7    | `docker compose stop nats`, then `start`     | `/healthz` 503, then 200; reconnected in ~2 s |

3. Found and fixed on the way: two processes opening a new SQLite file at once failed with `SQLITE_BUSY` (WAL switch and migration race). Migrations now run under `BEGIN IMMEDIATE` with a short retry; `TestConcurrentOpenMigratesOnce` covers it.

## 4. Later

1. HTTP webhook and gRPC transports with the same payloads (implement `hook.Hook` and `metering.Sink`).
2. Post-response hook (after upstream, e.g. output guardrails).
3. More control events: `key_created`, config reload.
4. Admin status endpoint: node ID, NATS state, sink drop counters.
