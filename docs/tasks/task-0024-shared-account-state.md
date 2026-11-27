# task-0024: Shared account state across nodes

- Status: done
- Change: [change-0012](../changes/change-0012-shared-account-state.md)
- Spec: [spec-0004](../specs/spec-0004-provider-accounts-and-policy.md) section 4.4, D40
- Done when: a rest seen by one node makes every node skip that account, a new node loads current rests, the Providers page shows one state, and a node without JetStream still works per node.

## 1. Plan

1. `provider`: an `AccountEvent` type and a `StateSync` interface in `BuildOptions`. `Pool.record` publishes when an account starts or ends a rest; `Pool.Apply` takes an event from another node if it is newer than what the account has.
2. `bus`: open or create the KV bucket; one sender goroutine writes events in order (a full queue drops and logs); a watcher applies every entry, its own node's entries skipped.
3. `app`: wire the sync into the catalog build; the watcher applies to the current snapshot; after a NATS reconnect, write own rests again.
4. Config `nats.state.bucket`, default `AIF_ACCOUNT_STATE`; `aif.example.yaml` comment.
5. Web: Providers page text without the node line.
6. Tests: pool publish and apply (newer wins, own events, unknown account); two buses on one embedded NATS: a rest on one shows on the other, and a late node loads it; no JetStream falls back.

## Result

1. `provider`: `AccountEvent` (the bucket's JSON value) and `StateSync` in `BuildOptions`. `Pool.record` publishes when a rest starts, or when a success ends a rest or clears an error; plain successes publish nothing. `Pool.Apply` takes an event when its `at` is newer than the account's last change. `Pool.Resting` lists current rests.
2. `bus.AccountSync`: opens or creates the bucket (history 1, entries expire after 24h, file storage); one sender goroutine with a queue of 256 (full = drop and warn); `Watch` applies the current entries and later changes, skipping its own node's.
3. `app`: shares cooldowns when the bucket opens, else warns and keeps per-node state; the watcher applies to the current snapshot; after a NATS reconnect the node writes its rests again.
4. Config `nats.state.bucket` (default `AIF_ACCOUNT_STATE`), in `aif.example.yaml`. CHANGELOG [Unreleased] Changed 1.
5. Web: the Providers page says account state is shared by all nodes; the node line is gone.
6. Tests: pool publish, apply, newest wins (`provider/sync_test.go`); two nodes, a late node, no JetStream, key names (`bus/state_test.go`); end to end with two gateway nodes and a late third (`TestAccountStateShared`).

## Verification

1. `make -C gateway vet test`, `cd web && pnpm typecheck`, `python scripts/check_docs.py`, compose config: all pass.
