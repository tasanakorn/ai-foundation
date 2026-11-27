# Changelog

All notable changes. Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/). Versions: [Semantic Versioning](https://semver.org/); `0.x` while the API may still change.

There is no prebuilt image or binary: build from the tag (README section 6).

## [0.1.0] - 2026-11-27

First tagged release.

### Added

1. One OpenAI-compatible API (`/v1`, alias `/openai/v1`) in front of OpenAI-compatible providers, OpenRouter and Anthropic (`type: anthropic`, `openai_format: compat`).
2. Anthropic Messages API under `/anthropic` (or a dedicated port) for Claude Code and Anthropic SDK apps, with prompt-cache metering (spec-0002).
3. API keys with tenant, app and end-user labels; token and cost metering per request, streams included.
4. Built-in admin accounts (admin / viewer), admin API, web UI and chat Playground.
5. NATS extension points: usage events (JetStream), pre-request hook (allow / deny / modify), key revocation across nodes.
6. PostgreSQL store with migrations at startup.
7. Production readiness (spec-0003):
   1. `server.trusted_proxies` and `server.client_ip_header`: the admin sign-in throttle and logs use the real client IP behind a proxy.
   2. Prometheus metrics on their own listener (`metrics.listen`, default off).
   3. The public dev secrets are refused at startup unless `dev_mode: true`.

[0.1.0]: https://github.com/tasanakorn/ai-foundation/releases/tag/v0.1.0
