# spec-0006: Catalog in the database, cached, with hot reload

- Status: draft
- Owner: project owner
- Related: [spec-0001](spec-0001-gateway-core-architecture.md) D4, sections 8.1, 9; [spec-0004](spec-0004-provider-accounts-and-policy.md); [spec-0005](spec-0005-model-routing-targets.md)

## 1. Summary

1. The **catalog** is providers, their accounts, models and their targets, prices and `passthrough`. Today it lives only in `aif.yaml` and loads once at startup (spec-0001 D4).
2. This spec moves the catalog into PostgreSQL as the source of truth. Each node keeps an in-memory **snapshot** (the cache) and swaps it when the catalog changes, with no restart.
3. The file stays for easy bootstrap: on the first start it seeds an empty catalog.
4. The admin API and UI can manage the catalog later (section 8). This spec only makes that possible.
5. This replaces D4 for the catalog. Infra settings stay in the file (section 2).

## 2. What moves, what stays

| Stays in `aif.yaml` (infra, needs a restart)                             | Moves to the database (catalog, hot reload)                         |
| ------------------------------------------------------------------------ | ------------------------------------------------------------------- |
| `server`, `admin`, `metrics`, `database`, `nats`, `metering`, `dev_mode` | `providers` (with accounts), `models` (with targets), `passthrough` |
| Hook settings (`nats.hooks`)                                             | Prices (part of models and targets)                                 |

## 3. Modes

New setting `catalog.mode`:

| Mode                 | Source of truth | At startup                                                                                                                   | Admin API writes                            |
| -------------------- | --------------- | ---------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------- |
| `database` (default) | Database        | If the catalog is empty, import the file's catalog once (seed). Later starts ignore the file's catalog and log that they did | Allowed                                     |
| `file`               | File            | Replace the database catalog with the file's catalog (one transaction). Reload when the file changes (section 5.4)           | Refused (409, "catalog is managed by file") |

1. `database` is for teams that manage models in the UI or API.
2. `file` is for GitOps: a reviewed commit changes the file, and the gateway applies it. The database still holds a copy, so the admin UI can show it and every node reads the same thing.
3. Seeding works like `admin.bootstrap`: safe when several nodes start at once (one transaction, under an advisory lock).

## 4. Storage

1. Tables: `catalog_providers`, `catalog_accounts`, `catalog_models`, `catalog_targets`, plus `catalog_meta` with one row: `revision bigint`, `updated_at`, `updated_by`.
2. Every change is one transaction that validates the **whole** catalog with the same rules as the file today (types, formats, names, prices, policies), then bumps `revision`. An invalid catalog is never committed.
3. `catalog_changes`: one row per revision: who (admin user, master key, `file`, `seed`), when, and a short summary. This replaces the git history that file-only setups had.
4. Provider keys (secrets): an account's key is stored in one of two forms.

   | Form      | Stored in the database                             | Use it for                               |
   | --------- | -------------------------------------------------- | ---------------------------------------- |
   | Reference | `env:ANTHROPIC_KEY_A` or `file:/run/secrets/key-a` | GitOps, Kubernetes Secrets, seeded files |
   | Encrypted | AES-256-GCM ciphertext and a key ID                | Keys typed into the admin UI later       |

   1. Encrypted keys need `catalog.secret_key` (env `AIF_CATALOG_SECRET_KEY`, 32 random bytes, base64). Without it, only references are accepted.
   2. Several key IDs may be configured, so the secret key can be rotated: new writes use the first key, and reads use the key ID stored with the value.
   3. The API never returns a key: only its form, and for encrypted keys the last 4 characters.
   4. A `file:` reference is read again on every reload, so a rotated Kubernetes Secret (mounted as a volume) takes effect without a restart. An `env:` reference changes only on restart.

## 5. Cache and hot reload

1. Each node holds one immutable snapshot: providers with their accounts, the router with its models and targets, and the revision. Requests read it through an atomic pointer, with no lock on the request path.
2. A request keeps the snapshot it started with, so a reload never changes a request halfway.
3. How nodes learn about a change:
   1. The writer publishes `aif.control.v1.catalog_changed` with `{"revision": N}` after the commit. This is a new subject on the existing control prefix, so it is additive (spec-0001 section 8.1).
   2. Every node also polls `catalog_meta.revision` every 30s (`catalog.poll_interval`), and once after a NATS reconnect, so a missed event costs at most one poll interval.
   3. A node loads the new catalog only when its revision is higher than the one it has.
4. File mode: the node watches `aif.yaml` (it checks the file's change time and content hash every 5s) and on SIGHUP. When the catalog part changed, the node with the advisory lock writes it to the database, and the event in 5.3 tells the others. A file that fails validation is logged and ignored; the last good catalog stays.
5. A reload that fails (database down, a reference that does not resolve) keeps the last good snapshot, logs an error and sets the metric in section 7. The node retries at the next poll.
6. State carries over: account cooldowns and round-robin positions (spec-0004 section 4.4) move to the new snapshot by provider and account name. Renamed or removed accounts start fresh.

## 6. Startup

1. Connect to the database and run migrations.
2. Seed (database mode, empty catalog) or sync (file mode).
3. Load the snapshot. If it fails at startup, the node exits: it never serves with no catalog.

## 7. Observability

| Name                              | Type    | Meaning                                 |
| --------------------------------- | ------- | --------------------------------------- |
| `aif_catalog_revision`            | gauge   | Revision this node serves               |
| `aif_catalog_reloads_total`       | counter | Reloads by `result` (`ok`, `error`)     |
| `aif_catalog_last_reload_seconds` | gauge   | Unix time of the last successful reload |

1. `/healthz` stays as it is. A node that serves an old revision is still healthy; the revision gauge shows the difference between nodes.
2. Log lines on every reload: old and new revision, source (event, poll, file, SIGHUP), duration.

## 8. Admin API (shape now, build later)

1. Read, any role: `GET /admin/v1/catalog` (whole catalog and revision), `GET /admin/v1/providers`, `GET /admin/v1/models`.
2. Write, admin role, database mode only: `POST`, `PATCH` and `DELETE` on `/admin/v1/providers/{name}`, `/admin/v1/providers/{name}/accounts/{account}` and `/admin/v1/models/{name}`.
3. Every write sends the revision it was based on (`If-Match: <revision>`). A stale revision gets `412`, so two admins cannot overwrite each other.
4. `POST /admin/v1/catalog/validate` checks a whole catalog without saving it (for the UI and CI).
5. CLI: `aif catalog export` writes the database catalog as YAML (references stay references, encrypted keys are left out). `aif catalog import -file x.yaml` replaces it in one transaction (database mode). These give backup and GitOps-style apply in database mode too.

## 9. Decisions

| ID  | Decision                                                                         | Why                                                               |
| --- | -------------------------------------------------------------------------------- | ----------------------------------------------------------------- |
| D34 | The catalog lives in PostgreSQL; the file seeds it (replaces D4 for the catalog) | Manage models in the UI or API later; no restart for changes      |
| D35 | Two modes: `database` (default) and `file` (GitOps)                              | Teams that review changes in git keep that workflow               |
| D36 | Immutable snapshot per node, swapped atomically                                  | No lock on the request path; a request never sees half a change   |
| D37 | Change event on NATS plus a revision poll                                        | Fast in the normal case, correct when an event is lost            |
| D38 | Keys as references or encrypted values; never returned                           | Kubernetes Secrets keep working; UI-entered keys are safe at rest |
| D39 | Whole-catalog validation in one transaction; last good snapshot kept             | A bad edit can never take the gateway down                        |

## 10. Open questions

1. Should file mode also accept a separate catalog file (`catalog.file: catalog.yaml`), so the infra file can stay a ConfigMap that never changes?
2. Catalog history: keep only summaries in `catalog_changes`, or full snapshots so an admin can roll back to a revision?
3. Does the Playground need to pick a specific target or account for testing?

## 11. Tasks

Build order: this spec first (tasks 0015-0019), then [spec-0004](spec-0004-provider-accounts-and-policy.md) (task-0020), then [spec-0005](spec-0005-model-routing-targets.md) (task-0021), because accounts and targets are stored in the catalog.


1. Schema and store methods for the catalog tables, revision and changes.
2. Snapshot type and atomic swap; router and providers built from a snapshot instead of the file.
3. Seed (database mode) and sync with file watch and SIGHUP (file mode).
4. Change event, revision poll, state carry-over, metrics.
5. Key references (`env:`, `file:`) and encryption with key IDs.
6. `aif catalog export | import`.
7. Later: admin API writes and the UI pages (with `web/app/types/admin.ts`).
