# change-0010: CI checks on pull requests

- Status: applied (on merge of its pull request)
- Date: 2026-11-27
- Related: [task-0022](../tasks/task-0022-ci-checks.md); [workflow.md](../workflow.md) section 5; [change-0009](change-0009-docs-first-workflow.md)

## 1. What changed

1. New GitHub Actions workflow `.github/workflows/ci.yaml`, run on every pull request (PR) and on each push to `main`. Four jobs, the same checks as CLAUDE.md section 5:
   1. `gateway`: `gofmt`, `go vet`, `go test -race`, `make build`.
   2. `web`: `pnpm install --frozen-lockfile`, `pnpm typecheck`, `pnpm build`.
   3. `deploy`: `docker compose -f deploy/local/compose.yaml config -q`.
   4. `docs`: `python3 scripts/check_docs.py`.
2. New script `scripts/check_docs.py` (standard library only):
   1. Every numbered document is listed in its folder's `INDEX.md`, the index lists no missing file, and the status in the index matches the document.
   2. Document names follow `<kind>-<nnnn>-<slug>.<ext>`.
   3. Relative links in `docs/`, `CLAUDE.md`, `README.md` and `CHANGELOG.md` point to files that exist.
   4. `--chars` also reports characters a normal keyboard does not type. It only reports; CI does not use it yet.
3. `web/package.json` pins pnpm with `packageManager: pnpm@11.5.0`.
4. After the first green run on `main`, the four jobs become required checks in the `protect-main` ruleset.

## 2. Why

1. Since change-0009, every change comes through a PR, but nothing checked a PR automatically.
2. tconio-ai builds from our tags, so `main` must always build.

## 3. Impact

1. No change to the gateway, its API, data, config or NATS contract. Product document: no update; users see no difference.
2. Spec: none; this is tooling.
3. Security: the workflow token can only read; no secrets are used; actions are pinned by commit SHA.
4. Contributors: a PR can only be merged when the four checks pass (after step 1.4).
5. CHANGELOG: no entry, as it does not change the product.

## 4. Roll back

1. Remove the required checks from the `protect-main` ruleset first, or no PR can merge.
2. Revert the commit.
