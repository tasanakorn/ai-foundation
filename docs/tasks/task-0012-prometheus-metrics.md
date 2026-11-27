# task-0012: Prometheus metrics

- Status: done
- Spec: [spec-0003](../specs/spec-0003-production-readiness.md) section 4 (D22)
- Done when: with `metrics.listen` set, `GET /metrics` returns the families of spec-0003 section 4.4 in Prometheus text format; streams still flush per event; default is off.

## 1. Scope

1. `internal/metrics`: counters, gauges, histograms, cardinality cap, text writer.
2. Wiring: usage records (recorder observer), HTTP responses (wrapper that keeps `http.Flusher`), hook decisions, sink errors, dropped records, NATS and database checks, runtime gauges.
3. `cmd/aif`: the metrics listener.
4. `aif.example.yaml`, `.env.example`, README.

## 2. Verification

1. Unit tests: text format, cap, histogram buckets.
2. Gateway test: a request moves the counters; the stream test still passes.
3. `make -C gateway vet test`.

## 3. Result

1. Done 2026-11-27: `make -C gateway vet test` passes (race on).
