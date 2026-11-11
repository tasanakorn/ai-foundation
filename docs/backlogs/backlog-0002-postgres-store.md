# backlog-0002: Postgres store

- Status: accepted
- Source: initial scope review
- Promoted to: [task-0004](../tasks/task-0004-postgres-store.md)

## 1. Idea

1. Add `internal/store/postgres` implementing `store.Store`, for multi-node deploys.
2. Same migrations list pattern. `database.driver: postgres`.
3. Consider a time-partitioned `usage_records` table for large volumes.
