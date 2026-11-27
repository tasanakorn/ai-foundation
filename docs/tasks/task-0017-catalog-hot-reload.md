# task-0017: Catalog hot reload across nodes

- Status: todo
- Spec: [spec-0006](../specs/spec-0006-catalog-database-hot-reload.md) sections 5.3, 5.5, 5.6, 7
- Done when: a change on one node reaches the others by aif.control.v1.catalog_changed and by the revision poll; cooldowns carry over; reload metrics and logs exist; a failed reload keeps the last good snapshot.
