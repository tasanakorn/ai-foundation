# change-0003: Local Docker Compose and env

- Status: applied
- Date: 2026-11-11
- Related: [spec-0001](../specs/spec-0001-gateway-core-architecture.md) §1A, D12, D13; [task-0003](../tasks/task-0003-nats-events-and-hooks.md); [task-0004](../tasks/task-0004-postgres-store.md)

## 1. What changed

1. Added `deploy/local/compose.yaml`: PostgreSQL 18 and NATS 2.15 (JetStream), healthchecks, named volumes, ports bound to `127.0.0.1`.
2. Added `.env.example` at the repo root: upstream keys, llama.cpp, gateway, Postgres and NATS settings.
3. `scripts/dev.py`: `--services` flag (compose up `--wait`; services keep running on exit). The `.env` parser now handles inline comments, quotes and `export`.
4. Root `Makefile`: `dev-all`, `services-up`, `services-down`, `services-reset`, `services-ps`, `services-logs`, `psql`.

## 2. Why

1. Postgres and NATS are the preferred backing services. Having them one command away unblocks task-0003 and task-0004.
2. App processes stay native for a fast loop. llama.cpp stays native for GPU access on macOS.

## 3. Roll back

1. `make services-reset`, then delete `deploy/local/` and `.env.example`. The gateway does not depend on them yet.
