# task-0025: OIDC admin login, one local account

- Status: todo
- Change: [change-0013](../changes/change-0013-oidc-admin-login.md)
- Spec: [spec-0001](../specs/spec-0001-gateway-core-architecture.md) sections 4.2, 6, 6.1, 6.3, 8.1, D41 and D42
- Done when: an admin signs in through an OIDC IdP and gets a normal session; only linked, active accounts get in; the flow works when start and callback land on different nodes; only the config's local account can use a password, its password follows config on every start, and each use is logged and published; `admin.password_login: false` blocks even that while the master key still works.

## 1. Plan

1. Config: `admin.bootstrap.password_hash` (PHC string check; error when both it and `password` are set), `admin.public_url`, `admin.password_login` (default `true`), `admin.oidc.session_ttl`, `admin.oidc.providers[]` (with `allowed_groups`, `groups_claim`) with validation (unique names in `a-z 0-9 -`, `issuer` and `public_url` are `https://` except `localhost`, `client_id` set). `aif.example.yaml` comments; `AIF_ADMIN_PASSWORD_HASH` and `${AIF_OIDC_<NAME>_SECRET}` in `.env.example`.
2. Store and migration: `admin_users.link_allowed_until`, tables `admin_identities` and `admin_oidc_flows`; store methods for flows (create, take once, delete expired) and identities (add, find by issuer + subject, list, delete). A down migration.
3. `auth`, local account (D42): sync from config on every start (create, role `admin`, replace the hash when it does not match, end its sessions); password check only for that account; the other accounts' old hashes ignored and counted in a log line; `ErrLocalAccount` guards for role, disable, delete, password and links.
4. `auth`, OIDC: a service over `github.com/coreos/go-oidc/v3` and `golang.org/x/oauth2` (pinned): discovery at startup with retry on `start` (at most every 30 s), start (state, nonce, PKCE S256), callback (exchange, ID token checks, email from the token or userinfo), linking rules and the `allowed_groups` check (spec-0001 section 6.3 item 4), session with the OIDC TTL.
5. Break-glass audit: warning logs and `aif.audit.v1.breakglass` on each local sign-in, and for the master key at most once per node per minute (`bus`).
6. `admin`: the new endpoints, the `aif_oidc` cookie (Lax, path-scoped), the same-origin redirect page, `403` on password sign-in when off and on `/auth/password`, `400` on `password` in user create and update, `local` on user objects, `method: "oidc"`.
7. CLI: `create-user` without a password, `hash-password` (hidden prompt or stdin), `reset-password` removed with a message that points to config.
8. Web: sign-in page from `GET /auth/methods` (IdP buttons, password form hidden when off, error text from the callback); break-glass banner; account page "Link login" and own links; Users page without password fields, with links, remove and "Allow link". `web/app/types/admin.ts` in step.
9. Tests: a fake OIDC IdP in Go tests (`httptest`, own signing key). Cases: good login; unlinked account refused; one-time link (allowed, expired, email not verified, local account); `link_by_email`; `allowed_groups` (in group, not in group, claim missing, custom `groups_claim`); disabled account; replayed or wrong `state`; bad `nonce`, `aud` or signature; email from userinfo; discovery retry; start on node A and callback on node B; link from a signed-in session. Local account: created and synced from plain and hash config, sessions end on change, other accounts' passwords refused, guards, audit event, `password_login: false` with the master key still working.
10. Docs: CHANGELOG [Unreleased] with a Breaking note and upgrade steps (add an IdP and allow links for other accounts before upgrading); README admin section.

## Result

(filled in when done)
