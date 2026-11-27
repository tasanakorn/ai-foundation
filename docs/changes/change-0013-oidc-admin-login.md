# change-0013: OIDC admin login

- Status: draft
- Date: 2026-11-27
- Related: [spec-0001](../specs/spec-0001-gateway-core-architecture.md) section 6.1 item 7 and D16; [product](../products/ai-foundation.md) goal 5; spec update and tasks to follow

## 1. What changes

1. The admin UI and admin API accept sign-in through an outside identity provider (IdP) that speaks OIDC (OpenID Connect, the standard "login with ..." protocol). Examples: Pocket ID, Casdoor, Authentik, Keycloak, Google.
2. The gateway is only an OIDC client. 2FA, passkeys and Google login are features of the IdP. The gateway builds none of them.
3. The built-in password login stays. It is not removed:
   1. It is on by default, so a fresh install still works with no IdP.
   2. A new setting turns it off once OIDC works (`admin.password_login: false`). Then the sign-in page shows only the IdP buttons, and `POST /admin/v1/auth/login` returns `403`.
   3. The master key (`admin.master_key`) keeps working in both modes, as the break-glass path.
4. One or more IdPs can be set, each with a name shown on the sign-in page.
5. No sign-up through the IdP. An outside login works only for an admin account that already exists in the gateway, and only after it is linked to that account:
   1. A link ties the account to the IdP's user id (`issuer` + `sub`). Every login matches by that link, never by email alone, so an email change at the IdP does not lock the user out or move access to someone else.
   2. Default way to link: a signed-in user starts "Link login" for an IdP on their own account page. An admin can remove any link.
   3. Optional, per IdP: `link_by_email: true` lets the first login link itself when the IdP's email matches an active `admin_users` row and the IdP says the email is verified. It is off by default, because some IdPs allow open sign-up or set `email_verified` loosely. Turn it on only for an IdP whose users you control.
   4. The role stays in the gateway (`admin` or `viewer`). Roles from the IdP are not read in this change.
6. After a good OIDC login, the gateway issues its normal session (`auth.Service.IssueSession`). Sessions, CSRF checks, roles and sign-out work as today.
7. The login flow:
   1. Start: the gateway makes a random state, nonce and PKCE verifier, stores them in Postgres (table `admin_oidc_flows`, 10 minute life, only the hash of the flow id), and sets a flow cookie `aif_oidc` holding the random flow id.
   2. The flow cookie is `HttpOnly`, `Secure` as the session cookie, `SameSite=Lax` and `Path=/admin/v1/auth/oidc`. Lax is needed: the IdP sends the browser back from another site, and a `Strict` cookie is not sent on that redirect.
   3. Callback: the gateway checks the flow cookie, state, nonce and the ID token (issuer, audience, expiry, signature), deletes the flow row (one use), and issues the session.
   4. The callback then answers with a small same-origin page that moves the browser to the admin UI, so the next request sends the `SameSite=Strict` session cookie.
   5. The flow state is in Postgres, so start and callback may land on different nodes, and no new shared secret is needed.
8. Session life: optional `admin.oidc.session_ttl` (for example `1h`) for sessions from an OIDC login, so users go back to the IdP more often. Default: `admin.session_ttl`. Known limit: a user disabled at the IdP keeps a gateway session until it expires. Back-channel logout (the IdP ending the session) is not in this change.

## 2. Why

1. The owner wants 2FA, Google login and passkeys for admins (2026-11-27), and wants to keep the core small.
2. Building TOTP, WebAuthn and recovery codes ourselves is large security code to own. Any good IdP already has them.
3. Generic OIDC, not one product: no IdP becomes a required service, so the "light" goal (one binary, Postgres and NATS) holds. The operator picks the IdP.
4. Not a forward-auth proxy (tinyauth, oauth2-proxy): the gateway would have to trust a user header, which can be faked when the admin port is reachable without the proxy.
5. Spec-0001 D16 already planned this seam.

## 3. Impact

1. Product document: goal 5 changes ("Outside logins can be added later" -> OIDC login, password login optional).
2. Spec: spec-0001 section 6.1 and a new decision (D-number set in the spec update): config shape, flow, account linking, the `password_login` switch.
3. Admin API (additive):
   1. `GET /admin/v1/auth/methods` (no auth): which sign-in methods are on, for the sign-in page.
   2. `GET /admin/v1/auth/oidc/{name}/start` and `GET /admin/v1/auth/oidc/{name}/callback`: the redirect flow, with state, nonce and PKCE. `start?link=1` links the IdP login to the signed-in user instead of signing in.
   3. `GET /admin/v1/users/{id}/identities` and `DELETE /admin/v1/users/{id}/identities/{identity_id}`: list and remove links. Users may list and remove their own links.
   4. The login response field `method` gains the value `oidc`.
   5. Mirrored in `web/app/types/admin.ts`.
4. Data: one migration adds two tables:
   1. `admin_identities (id, user_id, issuer, subject, email, created_at, last_login_at)`, unique on `(issuer, subject)`. Rows go when the user is deleted.
   2. `admin_oidc_flows (id_hash, provider, state, nonce, pkce_verifier, link_user_id, expires_at)`. Expired rows are deleted on each new flow.
5. Config, all optional:
   1. `admin.password_login` (default `true`).
   2. `admin.oidc.providers[]`: `name`, `title`, `issuer`, `client_id`, `client_secret`, `scopes`, `link_by_email` (default `false`).
   3. `admin.oidc.session_ttl` (default: `admin.session_ttl`).
   4. `admin.public_url`: the admin URL the IdP redirects back to. It is never built from the request `Host` header.
   5. Secrets come from env, and new settings go in `.env.example`.
6. Several nodes: the flow state is kept in a short-lived signed cookie, not in node memory, so the start and the callback may land on different nodes.
7. New Go dependencies (pinned): `github.com/coreos/go-oidc/v3` and `golang.org/x/oauth2`.
8. Web UI: the sign-in page shows one button per IdP, and hides the password form when it is off. The account page has "Link login" per IdP. The Users page shows each user's linked logins, with a way to remove a link.
9. No NATS subject or payload changes.
10. Without `admin.oidc` set, nothing changes for current installs.

## 4. Open questions (for the spec)

1. Should an admin be able to remove the password of a user who signs in only by OIDC, per user, instead of only the global `password_login` switch?
2. Should the IdP's group claim map to the gateway role later (for example `aif-admins` -> `admin`)? Not in this change.

## 5. Roll back

1. Set `admin.password_login: true` (or remove it) so password login works again, then deploy the older binary.
2. The `admin_identities` and `admin_oidc_flows` tables can stay; the older binary ignores them. A down migration drops them.
