# change-0012: Shared account state across nodes

- Status: applied (released in v0.4.0)
- Date: 2026-11-27
- Related: [task-0024](../tasks/task-0024-shared-account-state.md); [spec-0004](../specs/spec-0004-provider-accounts-and-policy.md) sections 4.4, 8 and D40 (replaces D29); [product](../products/ai-foundation.md) goal 2.4

## 1. What changed

1. When a node rests a provider account (rate limit, outage, bad key, no credits), or a rest ends early because a call succeeds, it writes the account state to a NATS JetStream key-value bucket, `AIF_ACCOUNT_STATE` (setting `nats.state.bucket`).
2. Every node watches the bucket and applies the state to its own copy, so all nodes skip a resting account, not only the node that saw the error. Requests never wait for NATS: each node reads its own copy in memory.
3. A node that starts, restarts or reconnects to NATS loads the current state from the bucket, and after a reconnect it writes its own rests again.
4. The Providers page shows the one shared state. The line "This page shows node ..." is gone.
5. Still per node: the round-robin position (weights hold over many requests) and the sign-in throttle. Sticky selection is a hash, so it already gives the same answer on every node.

## 2. Why

1. The owner expects the gateway to work as one system (2026-11-27). With per-node state (D29), each node had to hit the same error itself before it rested an account, the Providers page could show a different state on each refresh, and a restart forgot every rest.

## 3. Impact

1. Contract (additive):
   1. New KV bucket `AIF_ACCOUNT_STATE`, created by the first node if missing. Key: `<provider>.<account>` (a provider name with other characters than `A-Z a-z 0-9 _ -` is encoded as `b64_<base64url>`). Value: JSON `{"provider","account","until","last_error","last_error_at","node","at"}`. Entries expire after 24h.
   2. No existing NATS subject or payload changes. `GET /admin/v1/providers` keeps its `node` field.
2. Needs JetStream, which the usage stream already uses by default. Without JetStream (or if the bucket cannot be made), the node logs a warning and keeps per-node state, as before.
3. If NATS is down for a while, each node keeps working with its own state and syncs again on reconnect.
4. Config: new optional `nats.state.bucket` (default `AIF_ACCOUNT_STATE`). No new environment variable.
5. No database migration.

## 4. Roll back

1. Revert the commit and deploy the older binary. It ignores the bucket.
2. Optional: delete the bucket (`nats kv del AIF_ACCOUNT_STATE`).
