# backlog-0001: Budgets and rate limits

- Status: open
- Source: initial scope review
- Promoted to: —

## 1. Idea

1. Per key / tenant / app / end user limits:
   1. Requests per minute, tokens per minute.
   2. Spend budget per day or month (USD).
2. First version as an external pre-request hook ([task-0003](../tasks/task-0003-nats-events-and-hooks.md)).
3. Move into core (in-memory token bucket + periodic DB sync) only if hook latency is a problem.
