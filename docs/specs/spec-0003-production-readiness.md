# spec-0003: Production readiness

- Status: built (tasks 0011-0014)
- Owner: project owner
- Related: [spec-0001](spec-0001-gateway-core-architecture.md) sections 6.1, 9, 10; tasks [task-0011](../tasks/task-0011-trusted-proxies.md), [task-0012](../tasks/task-0012-prometheus-metrics.md), [task-0013](../tasks/task-0013-refuse-dev-secrets.md), [task-0014](../tasks/task-0014-versioned-releases.md)

## 1. Summary

1. Make the gateway safe and observable behind a reverse proxy (ingress, CDN, load balancer).
2. Scope: code and docs only. The project ships **no container image** (D24): each operator builds the binary or an image from a tagged release.

## 2. Problems

1. The admin sign-in throttle counts failures per client IP (spec-0001 section 6.1, 100 per 15 minutes) and logs that IP. The IP is the TCP peer. Behind a proxy every client has the proxy's IP, so:
   1. one attacker can lock out every admin for 15 minutes;
   2. logs show the proxy, not the person.
2. There is no metrics endpoint. Operators cannot alert on error rate, latency, dropped usage records or a lost NATS connection.
3. The dev defaults (`dev-master-key-0000`, `dev-admin-password`) are in `.env.example` and `scripts/dev.py`. A copied `.env` in production gives a known admin key.
4. There are no release tags, so a deployment cannot pin a version.

## 3. Trusted proxies (D21)

1. New settings under `server`:

   | Key                | Default           | Meaning                                                                                     |
   | ------------------ | ----------------- | ------------------------------------------------------------------------------------------- |
   | `trusted_proxies`  | `[]`              | IPs or CIDRs of proxies allowed to set the client IP header                                 |
   | `client_ip_header` | `X-Forwarded-For` | Header that carries the client IP (`X-Forwarded-For`, `X-Real-IP`, `CF-Connecting-IP`, ...) |

2. Rule, for every listener (public, Anthropic, admin):
   1. The TCP peer is not in `trusted_proxies` (or the list is empty): the client IP is the TCP peer. This is today's behavior.
   2. The peer is trusted and the header is `X-Forwarded-For`: walk the list from the right, skip trusted addresses, and take the first untrusted one. If every entry is trusted, take the leftmost.
   3. The peer is trusted and the header is another one: take its value when it is one valid IP, else the TCP peer.
3. Uses: the admin sign-in throttle and the sign-in log.
4. Limit: a header is only as good as the network. If clients can reach the gateway around the proxy, they can set the header too. Operators must make the proxy the only way in.

## 4. Metrics (D22)

1. New setting `metrics.listen`, default `off`. When set (for example `:9090`), that listener serves `GET /metrics` in the Prometheus text format, 0.0.4.
2. Metrics are on their own listener so a public ingress that forwards every path never exposes them.
3. No new dependency: a small writer in `internal/metrics`.
4. Families:

   | Name                           | Type      | Labels                                  | From                                                |
   | ------------------------------ | --------- | --------------------------------------- | --------------------------------------------------- |
   | `aif_build_info`               | gauge     | `version`                               | build                                               |
   | `aif_http_responses_total`     | counter   | `listener`, `code`                      | every public and Anthropic response, metered or not |
   | `aif_requests_total`           | counter   | `format`, `endpoint`, `model`, `status` | each usage record                                   |
   | `aif_request_duration_seconds` | histogram | `format`, `model`                       | each usage record (latency)                         |
   | `aif_tokens_total`             | counter   | `format`, `model`, `kind`               | `kind`: prompt, completion, cached, cache_write     |
   | `aif_cost_usd_total`           | counter   | `format`, `model`                       | each usage record                                   |
   | `aif_hook_decisions_total`     | counter   | `format`, `decision`                    | allow, deny, modify, unavailable                    |
   | `aif_usage_dropped_total`      | counter   | -                                       | recorder buffer full or closed                      |
   | `aif_usage_sink_errors_total`  | counter   | `sink`                                  | failed batch writes (store, nats)                   |
   | `aif_nats_up`                  | gauge     | -                                       | the NATS check of `/healthz`                        |
   | `aif_database_up`              | gauge     | -                                       | the database check of `/healthz`                    |
   | `go_goroutines`                | gauge     | -                                       | runtime                                             |
   | `go_memstats_heap_alloc_bytes` | gauge     | -                                       | runtime                                             |
   | `process_start_time_seconds`   | gauge     | -                                       | runtime                                             |

5. Cardinality cap: each family keeps at most 1000 label sets. Later sets fold into one series whose label values are all `_other`. Passthrough model names come from clients, so the cap is needed.
6. Duration buckets (seconds): 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600.

## 5. Refuse dev secrets (D23)

1. New top-level setting `dev_mode`, default `false`. Only `gateway/aif.dev.yaml` sets it to `true`.
2. When `dev_mode` is false, startup fails if `admin.master_key` is `dev-master-key-0000` or `admin.bootstrap.password` is `dev-admin-password`. The error names the setting and says how to fix it.
3. When `dev_mode` is true, startup logs a warning that dev mode is on.

## 6. Releases (D24)

1. Versions are git tags `vMAJOR.MINOR.PATCH` on `main`. `0.x` while the API may still change.
2. `aif version` and the startup log print the version (`make -C gateway build` already sets it from `git describe`).
3. `CHANGELOG.md` at the repo root, "Keep a Changelog" style, one section per tag.
4. No prebuilt binary or image is published. README section "Run in production" says how to build one.

## 7. Decisions

| ID  | Decision                                                          | Why                                                               |
| --- | ----------------------------------------------------------------- | ----------------------------------------------------------------- |
| D21 | Client IP from a header only when the TCP peer is a trusted proxy | Headers are client-controlled; only a known proxy may set them    |
| D22 | Metrics on their own listener, default off, no new dependency     | Never public by accident; keeps the binary small                  |
| D23 | Known dev secrets refused outside `dev_mode`                      | A copied dev `.env` must not give a known admin key in production |
| D24 | Tagged releases, no published image or binary                     | Operators build from a pinned tag in their own supply chain       |

## 8. Out of scope

1. A per-IP rate limit on the public API (backlog-0001 covers budgets and rate limits).
2. Tracing (OpenTelemetry).
3. Using the client IP for `X-Forwarded-Proto` trust: `admin.cookie_secure: auto` keeps its current rule.
