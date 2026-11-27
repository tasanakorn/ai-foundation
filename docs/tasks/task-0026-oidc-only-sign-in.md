# task-0026: OIDC-only sign-in, auto redirect

- Status: done
- Change: [change-0014](../changes/change-0014-oidc-only-sign-in.md)
- Spec: [spec-0001](../specs/spec-0001-gateway-core-architecture.md) sections 6, 6.1, 6.3, 6.4, D42 and D43
- Done when: with any OIDC provider set, no account can sign in with a password and `methods` says `password: false`; without OIDC the local account works as before; with `auto_redirect` the sign-in page goes straight to the IdP, but not after an error or a sign-out, and the first page opened is kept.

## 1. Plan

1. Config: `admin.oidc.auto_redirect` (must name a provider); `PasswordLoginOn` is false when providers are set; startup log line says why.
2. Admin API: `methods` gains `auto_redirect`; `oidcStart` errors go to `/login?error=...`.
3. Web: `AuthMethods.auto_redirect`; sign-in page auto redirect (not with `error` or `signed_out`), "Try again", "Signed out"; the deep link through `/login?next=` and session storage; no break-glass link.
4. Tests: config validation; password refused and `methods` with OIDC on; `auto_redirect` in `methods`; start errors land on `/login?error=`.
5. Docs: spec-0001, `aif.example.yaml`, CHANGELOG, README if it mentions break-glass.

## Result

1. Config: `admin.oidc.auto_redirect` (a config error unless it names a provider, or when no provider is set). `PasswordLoginOn` is false while any provider is set; the startup log says the local account cannot sign in until `admin.oidc` is removed.
2. Admin API: `methods` returns `auto_redirect`. Every failed OIDC `start` and `callback` now lands on `/login?error=...` (before, `start` errors went to `/login` without a message, which would loop with auto redirect).
3. Web: the sign-in page redirects to the IdP when `auto_redirect` is set, but not with `error` or `signed_out`; it shows "Try again with ..." after an error and "Signed out" after sign-out. Pages opened without a session go to `/login?next=<path>`; `next` (local paths only) is kept in session storage and opened after sign-in. The break-glass link is gone.
4. Tests: password refused with OIDC on (`403 password_login_off`, `methods` `password: false`, also for a viewer); `auto_redirect` in `methods`; unknown provider and refused sign-in land on `/login?error=`; config checks for `auto_redirect`. Tests that signed in the local account beside OIDC now sign in an OIDC admin (`oidcAdmin`).
5. Not done: no browser test of the redirect itself (the UI part); the deploy at tconio-ai tries it against its IdP.

## Verification

1. `make -C gateway vet test`, `cd web && pnpm typecheck && pnpm build`, `python scripts/check_docs.py`, `docker compose -f deploy/local/compose.yaml config`: all pass locally.
