# change-0013: OIDC admin login, one local account

- Status: applied (released in v0.5.0)
- Date: 2026-11-27
- Related: [task-0025](../tasks/task-0025-oidc-admin-login.md); [spec-0001](../specs/spec-0001-gateway-core-architecture.md) sections 4.2, 6, 6.1, 6.3, 8.1, D41 and D42; [product](../products/ai-foundation.md) goal 5

## 1. What changes

1. **OIDC login.** The admin UI and admin API accept sign-in through an outside identity provider (IdP) that speaks OIDC (OpenID Connect, the standard "login with ..." protocol). Examples: Rauthy, Pocket ID, Authentik, Keycloak, Google.
   1. The gateway is only an OIDC client. 2FA, passkeys and Google login are features of the IdP. The gateway builds none of them.
   2. One or more IdPs can be set, each with a button on the sign-in page.
2. **One local account (break-glass).** Password sign-in works for one account only: the one in config or env (`admin.bootstrap.email`, `AIF_ADMIN_EMAIL`). It is the emergency way in when the IdP is down. Every other account signs in only through OIDC.
   1. The account is created at startup if missing, and is always role `admin`.
   2. Its password comes from config on every start: plain `admin.bootstrap.password` (`AIF_ADMIN_PASSWORD`, fine for local dev) or an argon2id hash `admin.bootstrap.password_hash` (`AIF_ADMIN_PASSWORD_HASH`, for production, so the secret store holds no plain password). A new command `aif admin hash-password` makes the hash. When the stored hash does not match config, the gateway updates it and ends the account's sessions.
   3. The API and UI cannot change its role, disable it, delete it or set its password. It cannot be linked to an IdP, so it stays separate from the IdP.
   4. Other accounts cannot have a password. The API refuses `password` for them. Existing hashes of other accounts are ignored (not deleted, so a roll back still works), and the gateway logs how many at startup.
   5. Every use is loud: a password sign-in or a master-key call writes a warning log and a NATS event `aif.audit.v1.breakglass`. The UI shows a warning in the sidebar while signed in with this account.
   6. `admin.password_login: false` turns off even this password. Then only OIDC and the master key are left.
   7. Without `admin.bootstrap.email`, there is no local account: only OIDC and the master key.
3. **Master key.** `admin.master_key` stays as it is, optional. The docs advise leaving it empty in production unless scripts need it.
4. **No sign-up through the IdP.** An OIDC login works only for an existing, active account that is linked to it:
   1. A link ties the account to the IdP's user id (`issuer` + `sub`). Logins match by the link, never by email alone, so an email change at the IdP does not lock the user out or move access to someone else.
   2. Ways to link:
      1. An admin allows a one-time link for an account (valid 24h). The next OIDC login with that account's email, verified by the IdP, links it and ends the allowance. This is how a new account gets its first login.
      2. A signed-in user links another IdP from "My logins" (the account menu).
      3. Optional, per IdP: `link_by_email: true` allows the one-time link for every active account, without the admin step. Off by default, because some IdPs allow open sign-up or set `email_verified` loosely.
   3. An admin can remove any link; a user can remove their own.
   4. Optional, per IdP: `allowed_groups` (for example `[aif-admins]`). A login, or a link, is accepted only when the token's groups claim (`groups_claim`, default `groups`) has one of them. So people who sign up at the IdP by themselves are refused even after a link mistake. It only filters who may sign in; it does not set the role.
   5. The role stays in the gateway (`admin` or `viewer`). Roles from the IdP are not read.
5. **Login flow.**
   1. Start: the gateway makes a random flow id, state, nonce and PKCE verifier, stores them in Postgres (table `admin_oidc_flows`, 10 minute life, only the hash of the flow id) and sets a flow cookie `aif_oidc` with the flow id.
   2. The flow cookie is `HttpOnly`, `Secure` as the session cookie, `SameSite=Lax` and `Path=/admin/v1/auth/oidc`. Lax is needed: the IdP sends the browser back from another site, and a `Strict` cookie is not sent on that redirect.
   3. Callback: the gateway checks the flow cookie and state, exchanges the code, verifies the ID token (issuer, audience, expiry, signature, nonce), uses the flow row once, and issues the normal session (`auth.Service.IssueSession`).
   4. The email and `email_verified` come from the ID token, or from the IdP's userinfo endpoint when the token lacks them.
   5. The callback answers with a small same-origin page that moves the browser to the UI, so the next request sends the `SameSite=Strict` session cookie.
   6. If the IdP cannot be reached at startup, the gateway tries its discovery again on the next login attempt (at most every 30 seconds), so no restart is needed.
6. **Sessions.** Optional `admin.oidc.session_ttl` (for example `1h`) for sessions from an OIDC login. Default: `admin.session_ttl`. Known limit: an account disabled at the IdP keeps its gateway session until it expires. Sessions, CSRF checks, roles and sign-out work as today.

## 2. Why

1. The owner wants 2FA, Google login and passkeys for admins, and wants to keep the core small (2026-11-27).
2. Building TOTP, WebAuthn and recovery codes ourselves is large security code to own. Any good IdP already has them.
3. Generic OIDC, not one product: no IdP becomes a required service, so the "light" goal holds. The operator picks the IdP.
4. Not a forward-auth proxy (tinyauth, oauth2-proxy): the gateway would have to trust a user header, which can be faked when the admin port is reachable without the proxy.
5. `allowed_groups`: the same IdP may also serve end users of other apps, with open sign-up. A group check keeps them out of the admin site even if a link goes wrong (owner, 2026-11-27).
6. One local account: the owner does not want extra local accounts by accident (2026-11-27). One break-glass account plus SSO for everyone is common practice (Argo CD has the same model), and local accounts without 2FA are the weak spot it removes.
7. Spec-0001 D16 already planned the OIDC seam.

## 3. Impact

1. **Breaking** (on `0.x`, so the next minor version):
   1. Accounts other than the bootstrap account can no longer sign in with a password. They need an IdP and a link first.
   2. `POST /admin/v1/users` and `PATCH /admin/v1/users/{id}` refuse `password` (`400`). `POST /admin/v1/auth/password` returns `403`: the password is set in config.
   3. `aif admin create-user` makes accounts without a password. `aif admin reset-password` is removed; it prints that the password is set in config.
   4. An install without an IdP has one admin and no viewer accounts.
2. Product document: goal 5.
3. Spec-0001: sections 4.2, 6, 6.1, new 6.3, 8.1; decisions D41 (OIDC) and D42 (one local account).
4. Admin API (added):
   1. `GET /admin/v1/auth/methods` (no auth): the sign-in methods that are on.
   2. `GET /admin/v1/auth/oidc/{name}/start` and `/callback`: the redirect flow. `start?link=1` links the IdP to the signed-in user.
   3. Own links, any role: `GET /admin/v1/auth/identities`, `DELETE /admin/v1/auth/identities/{id}`.
   4. Admin: `GET /admin/v1/users/{id}/identities`, `DELETE /admin/v1/users/{id}/identities/{identity_id}`, `POST /admin/v1/users/{id}/identities/allow`.
   5. User objects gain `local` (true for the break-glass account) and `link_allowed_until`.
   6. Mirrored in `web/app/types/admin.ts`.
5. Data: migration v9 adds the column `admin_users.link_allowed_until`, and the tables `admin_identities` (with the IdP name; unique on `issuer, subject`; rows go with the user) and `admin_oidc_flows`.
6. Config:
   1. New: `admin.bootstrap.password_hash`, `admin.password_login` (default `true`), `admin.public_url`, `admin.oidc.session_ttl`, `admin.oidc.providers[]` (`name`, `title`, `issuer`, `client_id`, `client_secret`, `scopes`, `link_by_email`, `allowed_groups`, `groups_claim`).
   2. Changed: `admin.bootstrap.*` is read on every start, not only on the first.
   3. Env: `AIF_ADMIN_PASSWORD_HASH` and one client secret per IdP go in `.env.example`.
7. NATS (additive): new subject `aif.audit.v1.breakglass`, core pub/sub, payload `{type, method, user_id, email, ip, node, at}`. For the master key, at most one event per node per minute.
8. Several nodes: the flow state is in Postgres, so start and callback may land on different nodes, and no shared secret is needed.
9. New Go dependencies (pinned): `github.com/coreos/go-oidc/v3`, `golang.org/x/oauth2`.
10. Web UI: IdP buttons on the sign-in page (the password form behind "Use the local break-glass account", and hidden when off); the break-glass warning; "My logins" in the account menu; "Logins" and "Allow a login link (24h)" on the Admins page, where a new account gets its first link allowance at once; no password fields.

## 4. Left out

1. Roles from IdP groups (for example `aif-admins` -> `admin`), back-channel logout, and sign-up through the IdP. Each can be a later change.

## 5. Roll back

1. An older binary refuses a database at schema v9 ("database schema v9 is newer than this binary"). Before starting it, undo migration v9 by hand. This deletes all OIDC links (tested in `TestRollbackV9`):

   ```sql
   BEGIN;
   DROP TABLE IF EXISTS admin_oidc_flows;
   DROP TABLE IF EXISTS admin_identities;
   ALTER TABLE admin_users DROP COLUMN link_allowed_until;
   DELETE FROM schema_migrations WHERE version = 9;
   COMMIT;
   ```

2. Then deploy the older binary. Old password hashes of other accounts are still stored, so their password sign-in works again. Accounts added after the upgrade have no password; set one with the older `aif admin reset-password`.
