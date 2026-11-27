# task-0015: Catalog tables and snapshot

- Status: todo
- Spec: [spec-0006](../specs/spec-0006-catalog-database-hot-reload.md) sections 4, 5.1, 5.2
- Done when: providers, accounts, models and targets load from PostgreSQL into an immutable snapshot that requests read through an atomic pointer; whole-catalog validation in one transaction bumps the revision; tests cover a swap during a request.
