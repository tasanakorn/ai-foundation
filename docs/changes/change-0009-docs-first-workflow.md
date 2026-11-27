# change-0009: Docs-first workflow

- Status: applied (on merge of its pull request; GitHub rulesets set 2026-11-27)
- Date: 2026-11-27
- Related: [workflow.md](../workflow.md); [CLAUDE.md](../../CLAUDE.md) section 2; [docs/README.md](../README.md) section 4

## 1. What changed

1. New guide `docs/workflow.md`. Every change follows one order:

   ```
   change (draft) -> product + spec -> tasks -> code -> change (applied)
   ```

2. `CLAUDE.md` has a new section 2 "Workflow": follow the guide, and write no code without a change document and a task. Old sections 2-4 are now 3-5.
3. `docs/README.md` section 4 now points to the guide. The old "Typical flow" wrote the change record last.
4. `docs/changes/README.md`: the change record is written first, as `draft`, and set to `applied` when the change ships.
5. Branches and releases (`docs/workflow.md` section 5, CLAUDE.md section 2.4):
   1. One branch and one PR per change, squash merged into `main`. No direct push to `main`.
   2. A release is a CHANGELOG PR, then an annotated `vX.Y.Z` tag on `main`.
   3. GitHub rulesets: `main` takes changes only through a PR, with linear history, no force push and no deletion. Tags `v*` cannot be moved or deleted.

## 2. Why

1. The owner asked (2026-11-27) that every change is written down first, then the product and spec documents are updated, then tasks are created, and only then the code is written.
2. Before this, change records were written after the work, and some work had none (tasks 0007 to 0021, in v0.1.0 and v0.2.0).
3. Until now every commit went straight to `main`, with no review step, and nothing stopped a tag from being moved. tconio-ai builds from our tags, so a moved tag would change what it builds.

## 3. Impact

1. Docs and GitHub settings only. No code, config, API, data or NATS contract changes.
2. Product document: no update. Users of the gateway see no difference.
3. Specs: none needed; this is a process change, not a design.
4. Task: none. The rule starts with this change, so this record was written after its own edits, and it has no task.
5. With the `main` ruleset on, the owner too must use a PR. There is no CI yet, so the PR is a review step, not an automatic gate. CI is a later change.
6. Existing work is not rewritten. Change records for tasks 0007 to 0021 are not added by this change.

## 4. Roll back

1. Revert the commit. Nothing else depends on it.
2. Delete the two rulesets in GitHub (Settings -> Rules -> Rulesets).
