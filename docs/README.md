# Docs

All project documentation. Each folder has its own `README.md` with its rules.

## 1. Folders

| Folder                     | Purpose                                 | Kind      |
| -------------------------- | --------------------------------------- | --------- |
| [`products/`](products/)   | Product overviews, goals, personas      | —         |
| [`changes/`](changes/)     | Change records: what changed and why    | `change`  |
| [`specs/`](specs/)         | Technical and functional specs          | `spec`    |
| [`tasks/`](tasks/)         | Concrete, scoped units of work          | `task`    |
| [`backlogs/`](backlogs/)   | Ideas and requests not yet scheduled    | `backlog` |

## 2. File names

1. Numbered docs: `<kind>-<nnnn>-<slug>.<ext>`
   1. `<kind>` — `change`, `spec`, `task`, or `backlog`.
   2. `<nnnn>` — 4-digit, zero-padded, per-kind sequence. Use the next free number. Never reuse one.
   3. `<slug>` — lowercase kebab-case (`a-z`, `0-9`, `-`), 2–6 words.
   4. `<ext>` — usually `md`.
2. Other files: `<slug>.<ext>` (for example `products/ai-gateway.md`).
3. Reserved names:
   1. `README.md` — explains a folder.
   2. `INDEX.md` — optional listing of a folder's files.

## 3. Examples

| Path                                          | Why                         |
| --------------------------------------------- | --------------------------- |
| `products/ai-gateway.md`                      | Product doc, slug only      |
| `changes/change-0001-move-to-k8s.md`          | First change record         |
| `specs/spec-0004-model-routing.md`            | Fourth spec                 |
| `tasks/task-0012-add-gpu-node-pool.md`        | Twelfth task                |
| `backlogs/backlog-0007-cost-dashboard.md`     | Backlog uses kind `backlog` |

## 4. Workflow

Follow [workflow.md](workflow.md): every change starts with a change document (`draft`), then the product and spec documents, then the tasks, then the code. Ideas that are not scheduled go to `backlogs/`.
