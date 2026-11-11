# backlog-0003: Tenant and app registry

- Status: open
- Source: initial scope review
- Promoted to: —

## 1. Idea

1. Optional `tenants` and `apps` tables with names, status, and default limits.
2. Keys keep their free-form `tenant_id` / `app_id` labels (spec-0001 D6). The registry only adds data; it does not change keys.
3. Admin endpoints: `/admin/v1/tenants`, `/admin/v1/apps`. Disabling a tenant blocks all its keys.
