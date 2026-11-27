# task-0016: Catalog seed and file mode

- Status: done
- Spec: [spec-0006](../specs/spec-0006-catalog-database-hot-reload.md) sections 3, 5.4, 6
- Done when: database mode seeds an empty catalog from the file once (safe with parallel starts); file mode syncs the file to the database at start, on change and on SIGHUP; an invalid file keeps the last good catalog.

## Result

1. `catalog.mode` (`database` default, `file`) and `catalog.watch_interval` (5s); `AIF_CATALOG_MODE` in `.env.example`. The dev config uses file mode.
2. Keys from the file become references: `${NAME}` -> `env:NAME`; `env:` and `file:` stay as written; key values in the file are refused at startup.
3. `catalog.Manager`:
   1. Database mode seeds only at revision 0; parallel starts seed once (revision check). Later starts log that the file's catalog is ignored.
   2. File mode writes the file only when its content differs, so restarts and other nodes add no revisions.
   3. `Run` checks the file (time and size, then a content hash) and handles SIGHUP (`Trigger`). An invalid file is logged once and ignored; a database error is retried at the next check.
   4. `Reload` loads only a higher revision and keeps the last good snapshot on failure.
4. The app loads the catalog from the database at startup and exits if it cannot.
5. Tests: key references, parallel seeding, file-mode sync, file watch (rename like a ConfigMap, invalid files ignored, SIGHUP restores a drifted database), database-mode trigger. A smoke run of the binary showed a file edit applied in under a second.
