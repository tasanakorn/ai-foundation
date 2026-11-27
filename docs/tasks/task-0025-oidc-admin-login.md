# task-0025: OIDC admin login

- Status: todo
- Change: [change-0013](../changes/change-0013-oidc-admin-login.md)
- Spec: [spec-0001](../specs/spec-0001-gateway-core-architecture.md) sections 4.2, 6, 6.3 and D41
- Done when: an admin signs in through an OIDC IdP and gets a normal session; only linked, active accounts get in; the flow works when start and callback land on different nodes; `admin.password_login: false` blocks password sign-in while the master key and the CLI still work; without `admin.oidc` nothing changes.

## 1. Plan

1. Config: `admin.public_url`, `admin.password_login` (default `true`), `admin.oidc.session_ttl`, `admin.oidc.providers[]` with validation (unique names in `a-z 0-9 -`, `issuer` and `public_url` are `https://` except `localhost`, `client_id` set). `aif.example.yaml` comments; client secrets as `${AIF_OIDC_<NAME>_SECRET}` in `.env.example`.
2. Store and migration: `admin_users.link_allowed_until`, tables `admin_identities` and `admin_oidc_flows`; store methods for flows (create, take once, delete expired) and identities (add, find by issuer + subject, list, delete). A down migration.
3. `auth`: an OIDC service over `github.com/coreos/go-oidc/v3` and `golang.org/x/oauth2` (pinned): discovery at startup, start (state, nonce, PKCE S256), callback (exchange, ID token checks), account lookup and linking rules (spec-0001 section 6.3 item 4), session with the OIDC TTL. Users without a password cannot pass the password check.
4. `admin`: the new endpoints, the `aif_oidc` cookie (Lax, path-scoped), the same-origin redirect page, `403` on password sign-in and change when off, `method: "oidc"`. `password` optional on user create.
5. Web: sign-in page from `GET /auth/methods` (IdP buttons, password form hidden when off, error text from the callback); account page "Link login" and own links; Users page links, remove, "Allow link". `web/app/types/admin.ts` in step.
6. Tests: a fake OIDC IdP in Go tests (`httptest`, own signing key). Cases: good login; unlinked account refused; one-time link (allowed, expired, email not verified); `link_by_email`; disabled account; replayed or wrong `state`; bad `nonce`, `aud` or signature; start on node A and callback on node B; password login off (login and password change `403`, master key works); link from a signed-in session.
7. Docs: CHANGELOG [Unreleased] Added; README admin section if it lists sign-in methods.

## Result

(filled in when done)
