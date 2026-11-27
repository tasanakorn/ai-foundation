# task-0013: Refuse dev secrets outside dev mode

- Status: done
- Spec: [spec-0003](../specs/spec-0003-production-readiness.md) section 5 (D23)
- Done when: startup fails with a clear message when `admin.master_key` or `admin.bootstrap.password` is a known dev value and `dev_mode` is false; `aif.dev.yaml` sets `dev_mode: true` and `make dev` still works.

## 1. Verification

1. Config tests for both values, with and without `dev_mode`.
2. `make -C gateway vet test`.

## 2. Result

1. Done 2026-11-27: `make -C gateway vet test` passes (race on).
