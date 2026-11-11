<script setup lang="ts">
import type { TableColumn } from '@nuxt/ui'
import type { GroupBy, ListResponse, UsageRecord, UsageSummary } from '~/types/admin'

const api = useAdminApi()

const ranges = [
  { label: 'Last 24 hours', value: '24h' },
  { label: 'Last 7 days', value: '7d' },
  { label: 'Last 30 days', value: '30d' },
  { label: 'All time', value: 'all' },
]
const groups: { label: string, value: GroupBy }[] = [
  { label: 'Model', value: 'model' },
  { label: 'Provider', value: 'provider' },
  { label: 'Day', value: 'day' },
  { label: 'Tenant', value: 'tenant' },
  { label: 'App', value: 'app' },
  { label: 'End user', value: 'end_user' },
  { label: 'Key', value: 'key' },
]
const hours: Record<string, number> = { '24h': 24, '7d': 24 * 7, '30d': 24 * 30 }

const range = ref('7d')
const groupBy = ref<GroupBy>('model')
const filters = reactive({ tenant_id: '', app_id: '', end_user: '', model: '' })

const query = computed(() => {
  const q: Record<string, string> = {}
  for (const [k, v] of Object.entries(filters)) if (v) q[k] = v
  const h = hours[range.value]
  if (h) q.from = new Date(Date.now() - h * 3600_000).toISOString()
  return q
})

const { data: total, refresh: refreshTotal } = await useAsyncData('usage-total',
  () => api<ListResponse<UsageSummary>>('/usage/summary', { query: { ...query.value, group_by: 'none' } }),
  { watch: [query] })
const { data: grouped, status: groupedStatus, refresh: refreshGrouped } = await useAsyncData('usage-grouped',
  () => api<ListResponse<UsageSummary>>('/usage/summary', { query: { ...query.value, group_by: groupBy.value } }),
  { watch: [query, groupBy] })
const { data: recent, status: recentStatus, refresh: refreshRecent } = await useAsyncData('usage-recent',
  () => api<ListResponse<UsageRecord>>('/usage', { query: { ...query.value, limit: 50 } }),
  { watch: [query] })

const t = computed(() => total.value?.data[0])
const stats = computed(() => [
  { label: 'Requests', value: fmtInt(t.value?.requests), icon: 'i-lucide-activity' },
  { label: 'Errors', value: fmtInt(t.value?.errors), icon: 'i-lucide-triangle-alert' },
  { label: 'Tokens', value: fmtInt(t.value?.total_tokens), icon: 'i-lucide-hash',
    hint: `${fmtInt(t.value?.prompt_tokens)} in · ${fmtInt(t.value?.completion_tokens)} out` },
  { label: 'Cost', value: fmtUSD(t.value?.cost_usd), icon: 'i-lucide-circle-dollar-sign' },
])

const groupColumns = computed<TableColumn<UsageSummary>[]>(() => [
  { accessorKey: 'group', header: groups.find(g => g.value === groupBy.value)?.label ?? 'Group' },
  { accessorKey: 'requests', header: 'Requests', cell: ({ row }) => fmtInt(row.original.requests) },
  { accessorKey: 'errors', header: 'Errors', cell: ({ row }) => fmtInt(row.original.errors) },
  { accessorKey: 'prompt_tokens', header: 'Prompt', cell: ({ row }) => fmtInt(row.original.prompt_tokens) },
  { accessorKey: 'completion_tokens', header: 'Completion', cell: ({ row }) => fmtInt(row.original.completion_tokens) },
  { accessorKey: 'cost_usd', header: 'Cost', cell: ({ row }) => fmtUSD(row.original.cost_usd) },
])

const recentColumns: TableColumn<UsageRecord>[] = [
  { accessorKey: 'created_at', header: 'Time', cell: ({ row }) => fmtTime(row.original.created_at) },
  { accessorKey: 'model', header: 'Model' },
  { accessorKey: 'provider', header: 'Provider' },
  { accessorKey: 'tenant_id', header: 'Tenant / App' },
  { accessorKey: 'end_user', header: 'End user' },
  { accessorKey: 'status_code', header: 'Status' },
  { accessorKey: 'total_tokens', header: 'Tokens' },
  { accessorKey: 'cost_usd', header: 'Cost', cell: ({ row }) => fmtUSD(row.original.cost_usd) },
  { accessorKey: 'latency_ms', header: 'Latency', cell: ({ row }) => `${fmtInt(row.original.latency_ms)} ms` },
]

function refresh() {
  refreshTotal()
  refreshGrouped()
  refreshRecent()
}
</script>

<template>
  <UDashboardPanel id="usage">
    <template #header>
      <UDashboardNavbar title="Usage">
        <template #leading>
          <UDashboardSidebarCollapse />
        </template>
        <template #right>
          <USelect v-model="range" :items="ranges" class="w-40" />
          <UButton icon="i-lucide-refresh-cw" color="neutral" variant="ghost" aria-label="Refresh" @click="refresh" />
        </template>
      </UDashboardNavbar>
      <UDashboardToolbar>
        <div class="flex flex-wrap gap-2 py-1">
          <UInput v-model.lazy="filters.tenant_id" placeholder="Tenant" icon="i-lucide-building-2" size="sm" />
          <UInput v-model.lazy="filters.app_id" placeholder="App" icon="i-lucide-app-window" size="sm" />
          <UInput v-model.lazy="filters.end_user" placeholder="End user" icon="i-lucide-user" size="sm" />
          <UInput v-model.lazy="filters.model" placeholder="Model" icon="i-lucide-box" size="sm" />
        </div>
      </UDashboardToolbar>
    </template>

    <template #body>
      <div class="grid grid-cols-2 lg:grid-cols-4 gap-4">
        <UCard v-for="s in stats" :key="s.label">
          <div class="flex items-center gap-2 text-sm text-muted">
            <UIcon :name="s.icon" class="size-4" />
            {{ s.label }}
          </div>
          <div class="mt-2 text-2xl font-semibold tabular-nums">{{ s.value }}</div>
          <div v-if="s.hint" class="mt-1 text-xs text-muted tabular-nums">{{ s.hint }}</div>
        </UCard>
      </div>

      <UCard>
        <template #header>
          <div class="flex items-center justify-between gap-2">
            <span class="font-medium">Breakdown</span>
            <USelect v-model="groupBy" :items="groups" class="w-36" size="sm" />
          </div>
        </template>
        <UTable :data="grouped?.data ?? []" :columns="groupColumns" :loading="groupedStatus === 'pending'" />
      </UCard>

      <UCard>
        <template #header>
          <span class="font-medium">Recent requests</span>
        </template>
        <UTable :data="recent?.data ?? []" :columns="recentColumns" :loading="recentStatus === 'pending'">
          <template #tenant_id-cell="{ row }">
            <span>{{ row.original.tenant_id || '—' }}</span>
            <span class="text-muted"> / {{ row.original.app_id || '—' }}</span>
          </template>
          <template #end_user-cell="{ row }">
            {{ row.original.end_user || '—' }}
          </template>
          <template #status_code-cell="{ row }">
            <UTooltip :text="row.original.error" :disabled="!row.original.error">
              <UBadge
                :label="String(row.original.status_code)"
                :color="row.original.status_code < 400 ? 'success' : 'error'"
                variant="subtle"
              />
            </UTooltip>
          </template>
          <template #total_tokens-cell="{ row }">
            <span class="tabular-nums">{{ fmtInt(row.original.total_tokens) }}</span>
            <UBadge v-if="row.original.estimated" label="est." color="warning" variant="subtle" size="sm" class="ml-1" />
          </template>
        </UTable>
      </UCard>
    </template>
  </UDashboardPanel>
</template>
