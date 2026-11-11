# task-0003: NATS events and hooks

- Status: todo
- Spec: [spec-0001](../specs/spec-0001-gateway-core-architecture.md) §8, D13
- Done when: an extension in any language, connected only to NATS, can (a) receive every usage record, (b) allow, deny, or modify a request before it goes upstream, and (c) a key revoked on one gateway node stops working on all nodes within 1 s.

## 1. Scope

1. **Config** (NATS is optional; without it the gateway works as today):

   ```yaml
   nats:
     url: ${NATS_URL:-nats://127.0.0.1:4222}
     creds: ${NATS_CREDS:-}          # optional
     usage:
       subject_prefix: aif.usage.v1  # publish to aif.usage.v1.<tenant>.<app>
       jetstream: true               # durable stream AIF_USAGE
     hooks:
       pre_request:
         subject: aif.hook.pre_request.v1
         timeout: 200ms
         fail_open: false
         models: ["*"]
   ```

2. **Usage sink** (`metering.Sink`):
   1. Publish one message per `UsageRecord` (JSON) to `aif.usage.v1.<tenant>.<app>`. Empty labels become `_`. Unsafe subject chars are escaped.
   2. JetStream on: create/verify stream `AIF_USAGE` (subjects `aif.usage.v1.>`), publish with ack and `Nats-Msg-Id` = request ID (dedup).
   3. Publish failure: retry with backoff; log and count drops.
3. **Pre-request hook** (new `Hook` interface in `gateway`):
   1. NATS request/reply. Extensions subscribe with a queue group, so they scale out.
   2. Request: `{request_id, key: {id, tenant_id, app_id, metadata}, end_user, endpoint, model, body}`.
   3. Reply: `{"action":"allow"}` | `{"action":"deny","status":429,"message":"..."}` | `{"action":"modify","body":{...}}`.
   4. Timeout or no responder → `fail_open` decides.
4. **Control events**: on revoke, publish `aif.control.v1.key_revoked` `{id}`. All nodes drop that key from their auth cache.
5. **Dev loop**: NATS with JetStream is already in `deploy/local/compose.yaml` (`make services-up`, `NATS_URL` in `.env`).
6. **Example extension**: a tiny Python or Go subscriber in `extensions/example-budget/` that denies requests over a per-tenant daily token limit.

## 2. Later

1. HTTP webhook and gRPC transports with the same payloads, for setups without NATS.
