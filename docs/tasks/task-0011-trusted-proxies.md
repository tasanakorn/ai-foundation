# task-0011: Trusted proxies and client IP

- Status: done
- Spec: [spec-0003](../specs/spec-0003-production-readiness.md) section 3 (D21)
- Done when: with `server.trusted_proxies` set, the admin sign-in throttle and log use the client IP from the configured header; without it, behavior is unchanged. Tests cover the rules in spec-0003 section 3.2.

## 1. Scope

1. `config`: `server.trusted_proxies` (IPs or CIDRs, validated), `server.client_ip_header`.
2. New package `internal/clientip`: `Resolver.IP(r)`.
3. `admin`: sign-in uses the resolver.
4. `aif.example.yaml`, `.env.example`, README section "Run in production".

## 2. Verification

1. Unit tests for the resolver and config validation.
2. `make -C gateway vet test`.

## 3. Result

1. Done 2026-11-27: `make -C gateway vet test` passes (race on).
