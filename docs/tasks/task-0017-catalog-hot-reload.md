# task-0017: Catalog hot reload across nodes

- Status: done
- Spec: [spec-0006](../specs/spec-0006-catalog-database-hot-reload.md) sections 5.3, 5.5, 5.6, 7
- Done when: a change on one node reaches the others by aif.control.v1.catalog_changed and by the revision poll; cooldowns carry over; reload metrics and logs exist; a failed reload keeps the last good snapshot.

## Result

1. `aif.control.v1.catalog_changed` `{type, revision, node, at}`: published after every catalog write (seed, file sync, `Manager.Committed` for later writers); every node reloads when the revision is newer. Measured in the test: the peer reloaded about 12 ms after the commit.
2. Revision poll every `catalog.poll_interval` (30s), once after a NATS reconnect, and once right after the subscription starts.
3. A failed reload (for example a key reference that does not resolve) keeps the last good snapshot, logs an error and is tried again at the next poll.
4. Metrics: `aif_catalog_revision`, `aif_catalog_reloads_total{result}`, `aif_catalog_last_reload_seconds`.
5. Moved: cooldown carry-over goes to [task-0020](task-0020-provider-accounts-policy.md), because accounts have no state yet.
6. Tests: event reaches a peer, poll without an event, failed reload keeps the last good catalog and counts the error, event payload contract.
