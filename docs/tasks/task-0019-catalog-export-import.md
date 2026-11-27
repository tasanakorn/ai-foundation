# task-0019: Catalog export and import CLI

- Status: done
- Spec: [spec-0006](../specs/spec-0006-catalog-database-hot-reload.md) section 8.5
- Done when: aif catalog export writes YAML (references kept, encrypted keys left out) and aif catalog import replaces the catalog in one transaction.

## Result

1. `aif catalog export [-out FILE]`: YAML in the config file format, with a header (revision, when, by whom). References stay (`api_key: env:NAME`); encrypted keys are left out and listed in a comment.
2. `aif catalog import -file FILE [-if-revision N]`: database mode only; one transaction; `-if-revision` refuses a stale import. Accounts with no key in the file keep their encrypted key. Key values in the file are refused.
3. Also built: `aif catalog set-key` (encrypted key from the terminal or stdin) and `aif catalog history`.
4. After a write, the CLI publishes `catalog_changed` when NATS is up; otherwise nodes reload at their next poll.
5. Tests: export then import gives the same catalog; encrypted keys kept or replaced; key values and invalid catalogs refused. A smoke run with a live node: import, set-key, stale import refused, the node reloaded on each event, no key in the log.
