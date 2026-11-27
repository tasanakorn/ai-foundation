# task-0016: Catalog seed and file mode

- Status: todo
- Spec: [spec-0006](../specs/spec-0006-catalog-database-hot-reload.md) sections 3, 5.4, 6
- Done when: database mode seeds an empty catalog from the file once (safe with parallel starts); file mode syncs the file to the database at start, on change and on SIGHUP; an invalid file keeps the last good catalog.
