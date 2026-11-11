# task-0005: Built-in admin accounts and sign-in

- Status: done
- Spec: [spec-0001](../specs/spec-0001-gateway-core-architecture.md) §6, §6.1, D16
- Change: [change-0006](../change/change-0006-admin-accounts-and-sessions.md)
- Done when: a fresh install can create its first admin, people sign in to the web UI with email + password, admins manage other admins, viewers can only read, and the master key is optional.

## 1. Scope

1. **Why:** "local admin" means the system has its own admin-level login, for the first account and for setups without an outside login ("login with …").
2. **Accounts** (`gateway/internal/auth`): email + password (argon2id), roles `admin` / `viewer`, disable, delete, set password. Guards: not yourself, never the last active admin.
3. **Sessions:** HttpOnly + SameSite=Strict cookie `aif_session`; SHA-256 of the token in `admin_sessions`; TTL `admin.session_ttl` (12h). Disable, delete or password set ends that user's sessions; an own password change ends the user's other sessions.
4. **Security:** Origin check on cookie writes (CSRF), per-node sign-in limit (10 per email, 100 per IP, 15 min), same timing for unknown emails, `Secure` cookie on HTTPS (`admin.cookie_secure: auto`).
5. **First account:** `admin.bootstrap` (`AIF_ADMIN_EMAIL`, `AIF_ADMIN_PASSWORD`) on first start, or `aif admin create-user`. Also `aif admin reset-password` and `aif admin list-users`.
6. **Master key:** now optional; bearer token with the admin role for scripts and emergency access.
7. **Admin API:** `/admin/v1/auth/{login,logout,me,password}`, `/admin/v1/users[/{id}]` (spec §6).
8. **Web UI:** email + password login, account menu (change password, sign out), new **Admins** page (create, edit name, role, set password, disable/enable, delete), write buttons hidden for viewers. The Nuxt dev proxy keeps the browser `Host` so the Origin check passes.

## 2. Code

| Path                                          | What                                                  |
| --------------------------------------------- | ----------------------------------------------------- |
| `gateway/internal/auth/`                      | Accounts, passwords, sessions, sign-in limiter        |
| `gateway/internal/admin/auth.go`              | Authentication, roles, CSRF, auth + users endpoints   |
| `gateway/internal/store/postgres/`            | Migration v2: `admin_users`, `admin_sessions`         |
| `gateway/cmd/aif/admin.go`                    | `aif admin create-user / reset-password / list-users` |
| `gateway/internal/gateway/admin_auth_test.go` | End-to-end tests                                      |
| `web/app/composables/useAdmin.ts`             | `useSession` (me, login, logout), API client          |
| `web/app/pages/login.vue`, `users.vue`        | Login, Admins page                                    |
| `web/app/layouts/default.vue`                 | Account menu, change password                         |
| `web/app/types/admin.ts`                      | `AdminUser`, `Me`, inputs                             |

## 3. Verification

1. End-to-end tests (real wiring, Postgres, NATS): login flow and cookie flags, master key and wrong-bearer behavior, roles (viewer read-only, own password allowed), last-admin and self guards, disable ends sessions at once, password change ends other sessions, cross-origin writes blocked, rate limit `429`, bootstrap once across two nodes, a session from node 1 works on node 2.
2. `pnpm typecheck` passes.
3. Live: `aif admin create-user` / `reset-password` / `list-users` against Postgres; a password shorter than 12 characters is refused.
4. Browser (Playwright, UI served by the gateway): failed and good login, Admins page, create account, account menu, sign out, viewer sees no write buttons. No console errors.
5. `make dev`: login and a cookie write through the Nuxt dev proxy (`localhost:3000`) work.

## 4. Later

1. Outside logins (OIDC / "login with …") through `auth.Service.IssueSession` and an `admin_identities` link table.
2. Personal API tokens for admins (to replace the shared master key in scripts).
3. Audit log of admin actions (today: log lines with `by`).
4. Shared sign-in limits across nodes (NATS KV or Postgres).
