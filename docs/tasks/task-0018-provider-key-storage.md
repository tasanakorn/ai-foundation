# task-0018: Provider key references and encryption

- Status: todo
- Spec: [spec-0006](../specs/spec-0006-catalog-database-hot-reload.md) section 4.4
- Note: `env:` and `file:` references (resolve, form check, file re-read) were built in [task-0015](task-0015-catalog-tables-snapshot.md); this task adds encryption and the API view.
- Done when: account keys resolve from env: and file: references (file: re-read on reload) or from AES-256-GCM values with key IDs; keys are never returned by any API or log.
