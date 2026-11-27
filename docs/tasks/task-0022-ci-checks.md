# task-0022: CI checks on pull requests

- Status: done
- Change: [change-0010](../changes/change-0010-ci-checks.md)
- Spec: none (tooling); rules in [workflow.md](../workflow.md) section 5
- Done when: every PR runs the `gateway`, `web`, `deploy` and `docs` jobs, they pass on `main`, and `protect-main` requires them.

## 1. Plan

1. `.github/workflows/ci.yaml` with the four jobs, `permissions: contents: read`, one run per PR (an older run is cancelled), actions pinned by SHA.
2. Cache Go modules (setup-go), the pnpm store (setup-node) and `gateway/.cache/embedded-postgres`.
3. `scripts/check_docs.py`: indexes, statuses, names, links.
4. Pin pnpm in `web/package.json` (`packageManager`).
5. After the first green run on `main`: add the four jobs as required checks in `protect-main`.

## Result

1. Workflow `ci` with jobs `gateway`, `web`, `deploy`, `docs`. Versions: Go from `gateway/go.mod`, Node 24.16.0, pnpm 11.5.0, runner `ubuntu-24.04`.
2. `scripts/check_docs.py` finds 0 problems in the current docs; it reads every index row (changes 10, specs 6, tasks 22, backlogs 4).
3. Step 1.5 is done after the merge (see the PR).

## Verification

1. Locally: `python scripts/check_docs.py`, `make -C gateway vet test`, `cd web && pnpm typecheck`, compose config.
2. On GitHub: the first run of the PR.
