# change-0014: OIDC-only sign-in, auto redirect

- Status: applied (released in v0.6.0)
- Date: 2026-11-27
- Related: [task-0026](../tasks/task-0026-oidc-only-sign-in.md); [spec-0001](../specs/spec-0001-gateway-core-architecture.md) sections 6, 6.1, 6.3, D42 and D43; [change-0013](change-0013-oidc-admin-login.md)

## 1. What changes

1. **Password sign-in only without OIDC.** The local account (`admin.bootstrap`) signs in with its password only when `admin.oidc.providers` is empty.
   1. With any provider set, password sign-in is off for every account: `POST /admin/v1/auth/login` returns `403 password_login_off`, and `GET /admin/v1/auth/methods` returns `password: false`.
   2. The local account is still created and kept `admin` on every start, and its password still follows config. It cannot sign in while OIDC is on, and it still cannot be linked to an IdP.
   3. `admin.password_login: false` stays: it turns the password off even without OIDC.
   4. Way back in when the IdP is down: remove `admin.oidc` from the config and restart. The local account's password then works again. The master key (`admin.master_key`) is not affected.
2. **Auto redirect to the IdP.** New setting `admin.oidc.auto_redirect: <provider name>`, empty (off) by default. It must name one of `admin.oidc.providers`.
   1. `GET /admin/v1/auth/methods` gains `auto_redirect` (the provider name, or empty).
   2. When it is set and there is no session, the sign-in page sends the browser straight to `/admin/v1/auth/oidc/<name>/start`, with no click.
   3. No loop: when a sign-in fails (IdP error, account not linked, group not allowed, IdP down), the browser lands on the sign-in page with the error and a "Try again" button, and is not sent to the IdP again.
   4. After sign-out, the page shows "Signed out" and a sign-in button, and does not redirect. Otherwise the IdP's own session would sign the user straight back in.
   5. The page the user first opened (for example `/usage`) is kept through the round trip, for the IdP buttons too. Only local paths are kept.

## 2. Why

1. A break-glass password beside OIDC is a way around the IdP's 2FA, and on a public admin host behind a proxy that blocks `/admin/v1/auth/login` its link can never work. The owner finds it useless and wants the single local account only for installs without OIDC (2026-11-27).
2. With one IdP, the extra click on the sign-in page adds nothing (request from a deploy of the gateway, 2026-11-27).

## 3. Impact

1. **Breaking** (on `0.x`, so the next minor version, v0.6.0): an install with both `admin.oidc` and the local account's password loses password sign-in. Before upgrading, make sure at least one other active `admin` account is linked to the IdP.
2. Spec-0001: sections 6 (methods, login), 6.1 item 2, 6.3 items 2 and 4.8, 6.4 (new, auto redirect); D42 changed, new D43.
3. Admin API: `GET /admin/v1/auth/methods` gains `auto_redirect` (additive). Mirrored in `web/app/types/admin.ts`.
4. Config: new `admin.oidc.auto_redirect`. No new env variable.
5. Web UI: auto redirect, "Signed out" page, deep link kept; the "Use the local break-glass account" link is removed (password and OIDC are never on together).
6. Errors from `/admin/v1/auth/oidc/{name}/start` (unknown provider, IdP down) now land on `/login?error=...`, so the sign-in page shows them and does not redirect again.
7. No data migration, no NATS change.

## 4. Left out

1. A loopback-only break-glass (`password_login: loopback`): replaced by item 1.4.

## 5. Roll back

1. No data change. Going back to v0.5.0 brings back password sign-in beside OIDC. v0.5.0 ignores `admin.oidc.auto_redirect`.
