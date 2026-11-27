# change-0001: Initial gateway core

- Status: applied
- Date: 2026-11-11
- Related: [spec-0001](../specs/spec-0001-gateway-core-architecture.md), [task-0001](../tasks/task-0001-gateway-core-mvp.md)

## 1. What changed

1. Added the Go module `ai-foundation` and the `aif` binary (`cmd/aif`).
2. Added public `/v1` API, admin `/admin/v1` API, SQLite store, metering, keys, router, OpenAI-compatible provider.
3. Added `aif.example.yaml`, `Makefile`, `README.md`.

## 2. Why

1. First usable core of the gateway described in the [product doc](../products/ai-foundation.md).

## 3. Roll back

1. Nothing to roll back; this is the first release. Delete `aif.db` to reset data.
