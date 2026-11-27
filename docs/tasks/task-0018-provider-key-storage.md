# task-0018: Provider key references and encryption

- Status: done
- Spec: [spec-0006](../specs/spec-0006-catalog-database-hot-reload.md) section 4.4
- Note: `env:` and `file:` references (resolve, form check, file re-read) were built in [task-0015](task-0015-catalog-tables-snapshot.md); this task adds encryption and the API view.
- Done when: account keys resolve from env: and file: references (file: re-read on reload) or from AES-256-GCM values with key IDs; keys are never returned by any API or log.

## Result

1. `catalog.secret_keys` (`AIF_CATALOG_SECRET_KEY`): base64 keys of 32 bytes, checked at startup. The key ID is derived from the key (`k` + 8 hex of its SHA-256), so keys need no names. The first key seals new values; all keys open values sealed with them.
2. Migration v5: `catalog_accounts.key_cipher`, `key_id`, `key_hint` (last 4 characters, only for keys longer than 8).
3. `catalog.Keyring.SetKey` seals with AES-256-GCM (random nonce, fixed associated data) and clears any reference. `Build` decrypts with the keyring; an unknown key ID, a damaged value or a missing keyring fails the build, and the last good snapshot stays.
4. An account holds a reference or an encrypted key, never both. The ciphertext is not in JSON (`json:"-"`); errors name the key ID, never the value.
5. Not built: re-encrypting old values after a rotation. For now an admin sets each key again (task-0019 adds `aif catalog set-key`).
6. Tests: rotation, tamper, missing key, forms, database round trip, JSON without the key.
