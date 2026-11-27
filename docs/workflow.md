# Workflow

A light guide: how every change moves from an idea to code. Folder rules are in [README.md](README.md) and in each folder's `README.md`.

## 1. The rule

Every change gets a change document **first**, before any code. Then the product and spec documents, then the tasks, then the code.

```
change (draft) -> product + spec -> tasks -> code -> change (applied)
```

## 2. Steps

| Step | Write                                              | What goes in it                                                                   |
| ---- | -------------------------------------------------- | --------------------------------------------------------------------------------- |
| 1    | `changes/change-<nnnn>-<slug>.md`, status `draft`  | What changes, why, impact (contract, data, config), how to roll back              |
| 2    | `products/ai-foundation.md`                        | What the product now does and for whom, when the change is visible to users       |
| 3    | `specs/spec-<nnnn>-<slug>.md` (new, or update one) | Design, config shape, API shape, decisions with IDs, open questions               |
| 4    | `tasks/task-<nnnn>-<slug>.md`, status `todo`       | One scoped unit of work each, in build order, with "Done when"                    |
| 5    | Code, one task at a time                           | Run the checks of the part you touched (CLAUDE.md section 5)                      |
| 6    | Close                                              | Task: `Result`, status `done`. Change: status `applied`, date, version. CHANGELOG |

1. Step 2 is skipped when users see no difference (for example a refactor). Say so in the change document.
2. A small change may use one task. Do not skip the change document or the task.
3. When the plan changes during the work, update the change, spec and task first, then the code.
4. Ideas that are not scheduled go to `backlogs/`. A backlog item becomes real work through step 1.

## 3. Links

1. The change document links its spec and tasks. Each task links its spec and its change.
2. Update the folder's `INDEX.md` whenever a document is added or its status changes.

## 4. Statuses

| Kind   | Statuses                                                       |
| ------ | -------------------------------------------------------------- |
| change | `draft` -> `applied` (or `reverted`)                           |
| spec   | `draft` -> `review` -> `accepted` -> `built` (or `superseded`) |
| task   | `todo` -> `doing` -> `done` (or `dropped`)                     |

## 5. Branches and releases

1. `main` always builds and passes its checks. Nobody pushes to it directly; every change comes in through a pull request (PR).
2. Branch: one short branch per change document, named after it, for example `change-0008-credit-per-account`. Branch from the latest `main`.
3. PR:
   1. One PR per change. The description links the change document and its tasks.
   2. Before you ask for a merge, run the checks of the parts you touched (CLAUDE.md section 5).
   3. Squash merge, so each change is one commit on `main`. The branch is deleted after the merge.
   4. The PR sets the change to `applied` and its tasks to `done` (step 6).
4. Release:
   1. A release PR moves `[Unreleased]` in CHANGELOG.md to the new version and date. Merge it.
   2. Tag that commit on `main` with an annotated tag `vX.Y.Z` and push the tag. Tags are never moved or deleted.
   3. Versions follow semver (semantic versioning). While on `0.x`, a breaking change raises the minor number.
   4. A fix for an older release gets a `release/vX.Y` branch only when one is needed.
5. GitHub rules (the repository is public, so rulesets are free):
   1. `main`: changes only through a PR, linear history, no force push, no deletion. Required checks are added when CI exists.
   2. Tags `v*`: no update, no deletion.
