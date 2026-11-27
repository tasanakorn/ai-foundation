# task-0026: OIDC-only sign-in, auto redirect

- Status: todo
- Change: [change-0014](../changes/change-0014-oidc-only-sign-in.md)
- Spec: [spec-0001](../specs/spec-0001-gateway-core-architecture.md) sections 6, 6.1, 6.3, 6.4, D42 and D43
- Done when: with any OIDC provider set, no account can sign in with a password and `methods` says `password: false`; without OIDC the local account works as before; with `auto_redirect` the sign-in page goes straight to the IdP, but not after an error or a sign-out, and the first page opened is kept.

## 1. Plan

1. Config: `admin.oidc.auto_redirect` (must name a provider); `PasswordLoginOn` is false when providers are set; startup log line says why.
2. Admin API: `methods` gains `auto_redirect`; `oidcStart` errors go to `/login?error=...`.
3. Web: `AuthMethods.auto_redirect`; sign-in page auto redirect (not with `error` or `signed_out`), "Try again", "Signed out"; the deep link through `/login?next=` and session storage; no break-glass link.
4. Tests: config validation; password refused and `methods` with OIDC on; `auto_redirect` in `methods`; start errors land on `/login?error=`.
5. Docs: spec-0001, `aif.example.yaml`, CHANGELOG, README if it mentions break-glass.
