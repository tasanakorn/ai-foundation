# CLAUDE.md

Guidance for Claude Code in this repository.

## 1. Documentation layout

1. All project docs live under `docs/`. Read `docs/README.md` before adding or moving a doc.
2. Folders:

   | Folder           | Holds                                 | Kind      | Name pattern                  |
   | ---------------- | ------------------------------------- | --------- | ----------------------------- |
   | `docs/products/` | Product overviews, goals, personas    | —         | `<slug>.<ext>`                |
   | `docs/changes/`  | Change records (what changed and why) | `change`  | `change-<nnnn>-<slug>.<ext>`  |
   | `docs/specs/`    | Technical / functional specs          | `spec`    | `spec-<nnnn>-<slug>.<ext>`    |
   | `docs/tasks/`    | Concrete, scoped units of work        | `task`    | `task-<nnnn>-<slug>.<ext>`    |
   | `docs/backlogs/` | Ideas and requests not yet scheduled  | `backlog` | `backlog-<nnnn>-<slug>.<ext>` |

## 2. Workflow

1. Follow [`docs/workflow.md`](docs/workflow.md) for every change.
2. Order: change document (`draft`) first, then the product and spec documents, then the task documents, then the code.
3. No code without a change document and a task. Close both when done (task `done` with a Result, change `applied`).
4. One branch and one pull request per change; never push to `main` directly. Releases are annotated `vX.Y.Z` tags on `main` (`docs/workflow.md` section 5).

## 3. File naming rules

1. Numbered kinds (`change`, `spec`, `task`, `backlog`) use `<kind>-<nnnn>-<slug>.<ext>`.
   1. `<kind>` is the singular kind from the table above. Backlog items use `backlog`.
   2. `<nnnn>` is a 4-digit, zero-padded number (`0001`, `0042`).
      1. Numbering is per kind: `spec-0001` and `task-0001` can both exist.
      2. Take the next number after the highest one in that folder. Never reuse a number, even after a delete.
   3. `<slug>` is lowercase kebab-case: `a-z`, `0-9`, `-` only. Keep it short (2–6 words).
   4. `<ext>` is usually `md`. Use another extension only when the content needs it (`yaml`, `json`, `svg`).
2. All other files (including everything in `docs/products/`) use `<slug>.<ext>`.
3. Reserved names in every folder:
   1. `README.md` — explains what the folder is for and its rules.
   2. `INDEX.md` — optional listing of the folder's files. Add it when a listing helps.

## 4. Working rules

1. When you create, rename, or change the status of a numbered doc, update that folder's `INDEX.md` in the same change.
2. Link related docs with relative paths (for example `../specs/spec-0003-auth-flow.md`).
3. When a backlog item becomes real work, keep the backlog file and link it from the new spec or task. Do not renumber it.

## 5. Code layout (monorepo)

1. One top-level folder per part. Each part has its own toolchain file and builds alone. Never put Go or JS files at the repo root.

   | Folder               | Part                           | Check before done                                    |
   | -------------------- | ------------------------------ | ---------------------------------------------------- |
   | `gateway/`           | Go gateway `aif`               | `make -C gateway vet test`                           |
   | `web/`               | Nuxt admin UI                  | `cd web && pnpm typecheck`                           |
   | `scripts/`           | Dev tooling                    | Python, stdlib only                                  |
   | `deploy/`            | Compose / deploy               | `docker compose -f deploy/local/compose.yaml config` |
   | `extensions/<name>/` | Example extensions (NATS only) | Runs against `make services-up`                      |

2. New parts (extensions, SDKs) get a new top-level folder, for example `extensions/<name>/`.
3. Architecture and decisions: `docs/specs/spec-0001-gateway-core-architecture.md`. Admin API shapes are mirrored in `web/app/types/admin.ts`; change both together.
4. Backing services: NATS (JetStream) and PostgreSQL are both required (spec D14, D15). There is no SQLite. Go tests start an embedded Postgres (`internal/pgtest`) or use `AIF_TEST_PG_URL`.
5. NATS subjects and payloads (spec §8.1) are a public contract. Version them (`.v1`); never change a released payload in a breaking way.
6. Local env: new settings go in `.env.example` (with a comment) and are read from the repo-root `.env`. Never commit `.env`.
