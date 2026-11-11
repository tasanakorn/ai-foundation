# example-budget

An example extension: a daily token budget per tenant. It connects only to NATS.

1. On start, it reads today's usage (from 00:00 UTC) from the JetStream stream `AIF_USAGE`, then follows new records live.
2. It answers the pre-request hook `aif.hook.pre_request.v1` in queue group `example-budget`. When a tenant has used its tokens for today, it denies with `429 budget_exceeded`.
3. Keys without a `tenant_id` are budgeted per key.

## 1. Run

```sh
make services-up                                   # NATS (JetStream) + Postgres
make dev                                           # gateway with the hook on (dev config)
BUDGET_TENANT_LIMITS="acme=5000" uv run extensions/example-budget/budget.py
```

| Variable               | Default                 | Meaning                                 |
| ---------------------- | ----------------------- | --------------------------------------- |
| `NATS_URL`             | `nats://127.0.0.1:4222` | NATS server                             |
| `BUDGET_DAILY_TOKENS`  | `100000`                | Limit per tenant per UTC day            |
| `BUDGET_TENANT_LIMITS` | empty                   | Overrides, e.g. `acme=5000,beta=200000` |

## 2. Notes

1. Soft limit: a request is counted when its usage event arrives, after the response. Parallel requests can go a little over.
2. Run more copies to scale. The queue group spreads hook calls, and each copy reads the full usage stream.
3. The gateway must have `nats.hooks.pre_request.subject: aif.hook.pre_request.v1`. With `fail_open: false`, requests fail with 503 while no copy runs.
