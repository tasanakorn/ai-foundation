# task-0007: Anthropic provider type

- Status: done
- Spec: [spec-0002](../specs/spec-0002-anthropic-api-and-credits.md) section 5
- Done when: an `anthropic` provider in `aif.yaml` serves `/v1/chat/completions` through Anthropic's OpenAI-compatible endpoint (`openai_format: compat`), with tests, example config and `.env.example` updated.

## 1. Scope

1. `provider.Build`: new type `anthropic`.
   1. Default `base_url` `https://api.anthropic.com/v1`.
   2. Auth header `x-api-key`. Optional `workspace_id` -> `anthropic-workspace-id`.
   3. `openai_format` is required; `compat` is the only value accepted until task-0009. Startup fails with a clear message when it is missing.
2. `config`: new provider fields `workspace_id`, `openai_format`; validation.
3. `aif.example.yaml`: `anthropic` provider and the aliases and prices of spec-0002 section 5.4.
4. `.env.example`: `ANTHROPIC_API_KEY` with a comment.
5. Out of scope: `/anthropic` routes (task-0008), native translation (task-0009).

## 2. Verification

1. Unit tests with a fake upstream (`httptest`): default base URL, `x-api-key` set, `Authorization` not sent, workspace header, missing `openai_format` refused.
2. `make -C gateway vet test`.
3. Done 2026-11-27: unit tests in `gateway/internal/provider/provider_test.go` and `gateway/internal/config/config_test.go`; `make -C gateway vet test` passes.
4. Done 2026-11-27, live end to end: the gateway with one `anthropic` provider (`openai_format: compat`), Postgres and NATS from `make services-up`, and a gateway key.
   1. `GET /v1/models` lists `claude-haiku-5-5`.
   2. `POST /v1/chat/completions` returns 200, both plain and with `stream: true` (the last chunk carries usage).
   3. Usage records show the tenant, app, tokens and cost; `cached_tokens` is 0, as expected on the compat endpoint.
   4. An upstream error passes through unchanged: `temperature` returns 400 "`temperature` is deprecated for this model." (spec-0002 section 5).
