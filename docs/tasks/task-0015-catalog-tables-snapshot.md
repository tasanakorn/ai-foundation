# task-0015: Catalog tables and snapshot

- Status: done
- Spec: [spec-0006](../specs/spec-0006-catalog-database-hot-reload.md) sections 4, 5.1, 5.2
- Done when: providers, accounts, models and targets load from PostgreSQL into an immutable snapshot that requests read through an atomic pointer; whole-catalog validation in one transaction bumps the revision; tests cover a swap during a request.

## Result

1. Migration v4: `catalog_meta` (one row: revision, passthrough), `catalog_providers`, `catalog_accounts`, `catalog_models`, `catalog_targets`, `catalog_changes`.
2. Store: `GetCatalog` (one repeatable-read transaction), `CatalogRevision`, `UpdateCatalog` (row lock on the meta row, `If-Match`-style revision check with `ErrStale`, whole rewrite, revision + 1, change row), `ListCatalogChanges`.
3. `internal/catalog`:
   1. `Validate`: the file's rules (`config.NormalizeCatalog` and `config.ValidateCatalog`) plus account names and key reference forms. Until spec-0004 and spec-0005, exactly one account per provider and one target per model.
   2. `Update` and `Replace` validate inside the store transaction, so an invalid catalog is never committed.
   3. `Build` and `Load` resolve key references (`env:NAME`, `file:/path`, read on every build) and make the providers and the router.
   4. `Holder`: atomic pointer; `Offer` swaps only to a higher revision.
4. The gateway reads the router once per request (`router.Source`) and keeps it, the pre-request hook included.
5. The app still builds its first snapshot from the file (revision 0). Seeding the database and loading from it at startup is task-0016.
6. Tests: store round trip and order, stale and failed updates roll back, concurrent writers; validation cases; key references; concurrent swaps under `-race`; a swap during a request (the in-flight request keeps its provider and price).
