# task-0002: Admin web UI (Nuxt)

- Status: done
- Spec: [spec-0001](../specs/spec-0001-gateway-core-architecture.md) §6, D10
- Done when: an operator can create/revoke keys and see usage by tenant, app, end user, model, and day from a browser on the admin port.

## 1. Scope (shipped)

1. Nuxt 4 + Nuxt UI 4 app in `web/`, `ssr: false`, built with `pnpm build` (`nuxt generate`) to `web/.output/public`.
2. Served by `aif` via `admin.ui_dir` (same origin as `/admin/v1`). Unknown paths get the app shell. Dev mode proxies `/admin/v1` to `127.0.0.1:8081` (`AIF_ADMIN_URL` overrides).
3. Login asks for the master key; it is kept in `sessionStorage` only.
4. Pages:
   1. Usage (`/`) — totals, breakdown by model / provider / day / tenant / app / end user / key, recent requests, date range and filters.
   2. API keys (`/keys`) — list, filter, create (secret shown once, copy), revoke with confirm.
   3. Models (`/models`) — aliases, providers, passthrough flag (read-only).
5. Icons are bundled (`@iconify-json/lucide`, `provider: 'none'`), so the UI works without internet.

## 2. Verification

1. `pnpm typecheck` passes.
2. Headless browser run (Playwright): login → usage → create key → models. No console errors.
3. Gateway serves the build: `/`, `/keys`, `/models`, deep links → app shell (200); assets → 200; `/admin/v1/*` unaffected.

## 3. Out of scope

1. Multi-user login / RBAC. The UI uses the master key only. (Done later in [task-0005](task-0005-admin-accounts.md).)
2. Embedding the UI into the Go binary (`go:embed`). Can follow later as a build flag.
