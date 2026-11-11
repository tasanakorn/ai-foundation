# change-0002: Monorepo layout, dev loop, admin UI

- Status: applied
- Date: 2026-11-11
- Related: [spec-0001](../specs/spec-0001-gateway-core-architecture.md) §1A, D11; [task-0002](../tasks/task-0002-admin-web-ui.md)

## 1. What changed

1. Go code moved from the repo root to `gateway/`. Module path `ai-foundation` → `ai-foundation/gateway`.
2. `aif.example.yaml` moved to `gateway/`. Added `gateway/aif.dev.yaml` (OpenRouter + llama.cpp).
3. Added `web/` (Nuxt admin UI) and `scripts/dev.py` (dev loop). Root `Makefile` delegates to each part.
4. Admin API: model `price` now serializes as `{input, output, cached_input}` (was Go field names).
5. Admin UI hosting: every non-file path returns the app shell (no trailing-slash redirects).
6. Spec-0001 accepted. Added PostgreSQL (task-0004) and NATS (task-0003, rescoped from HTTP hooks) as preferred backing services.

## 2. Why

1. One repo with one folder per part gives a fast dev loop and keeps each part independent.

## 3. Roll back

1. Move `gateway/*` back to the root and revert the module path. No data migration involved.
