# change-0006: Built-in admin accounts and sessions

- Status: applied
- Date: 2026-11-11
- Related: [spec-0001](../specs/spec-0001-gateway-core-architecture.md) §6.1, D16; [task-0005](../tasks/task-0005-admin-accounts.md); [product](../products/ai-foundation.md) goal 5

## 1. What changed

1. Admin accounts (email + password, roles `admin` / `viewer`) in the new tables `admin_users` and `admin_sessions` (migration v2).
2. The admin API accepts a session cookie (`aif_session`) or the master key. New endpoints: `/admin/v1/auth/{login,logout,me,password}` and `/admin/v1/users[/{id}]`.
3. `admin.master_key` is now **optional** (empty = off).
4. New config: `admin.session_ttl` (12h), `admin.cookie_secure` (auto), `admin.bootstrap.{email,password,name}` (first start only).
5. New CLI: `aif admin create-user | reset-password | list-users`. `aif help` prints usage.
6. Web UI: email + password login, account menu with change password, new Admins page. Viewers see no write buttons.
7. The Nuxt dev proxy now keeps the browser `Host` header (`changeOrigin: false`).
8. Dev loop: `make dev` creates `admin@example.com` / `dev-admin-password` on first start (override with `AIF_ADMIN_EMAIL`, `AIF_ADMIN_PASSWORD`).

## 2. Why

1. The user clarified that "local admin" means the system has its own admin-level login: the first account, and the login for setups without an outside login system.
2. Sessions in Postgres work on every node, which matters now that the admin port can be public.

## 3. Impact

1. The web UI no longer accepts the master key. Scripts keep using it as a bearer token.
2. Configs without `admin.master_key` now load (before, it was required).
3. Admin API clients that only used the master key work unchanged.

## 4. Roll back

1. Revert this change. The two tables can stay; nothing else reads them.
