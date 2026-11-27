<script setup lang="ts">
import type { TableColumn } from '@nuxt/ui'
import type { ModelInfo, ModelsResponse } from '~/types/admin'

const api = useAdminApi()
const { data, status } = await useAsyncData('models', () => api<ModelsResponse>('/models'))

const columns: TableColumn<ModelInfo>[] = [
  { accessorKey: 'id', header: 'Model (client name)' },
  { accessorKey: 'provider', header: 'Provider' },
  { accessorKey: 'upstream', header: 'Upstream model' },
  { id: 'price', header: 'Price / 1M tokens' },
]
</script>

<template>
  <UDashboardPanel id="models">
    <template #header>
      <UDashboardNavbar title="Models">
        <template #leading>
          <UDashboardSidebarCollapse />
        </template>
      </UDashboardNavbar>
    </template>

    <template #body>
      <UAlert
        icon="i-lucide-info"
        color="neutral"
        variant="subtle"
        title="Models and providers come from the catalog in the database (seeded by the config file, or kept in sync with it in file mode)."
        :description="data?.passthrough
          ? 'Passthrough is on: clients can also call <provider>/<model>, e.g. openrouter/openai/gpt-4o-mini.'
          : 'Passthrough is off: clients can only call the aliases below.'"
      />

      <div class="flex flex-wrap items-center gap-2">
        <span class="text-sm text-muted">Providers:</span>
        <UBadge v-for="p in data?.providers ?? []" :key="p" :label="p" color="neutral" variant="outline" />
      </div>

      <UTable :data="data?.data ?? []" :columns="columns" :loading="status === 'pending'">
        <template #id-cell="{ row }">
          <code>{{ row.original.id }}</code>
        </template>
        <template #provider-cell="{ row }">
          <div v-for="(t, i) in row.original.targets" :key="i">
            {{ t.provider }}
            <span v-if="row.original.policy === 'round_robin'" class="text-muted">(weight {{ t.weight }})</span>
          </div>
          <span v-if="row.original.targets.length > 1" class="text-xs text-muted">{{ row.original.policy.replace('_', ' ') }}, falls back in order</span>
        </template>
        <template #upstream-cell="{ row }">
          <div v-for="(t, i) in row.original.targets" :key="i">
            <code class="text-muted">{{ t.upstream }}</code>
          </div>
        </template>
        <template #price-cell="{ row }">
          <div v-for="(t, i) in row.original.targets" :key="i">
            <span v-if="t.price" class="tabular-nums">
              in ${{ t.price.input }} / out ${{ t.price.output }}
            </span>
            <span v-else class="text-muted">provider-reported or free</span>
          </div>
        </template>
      </UTable>
    </template>
  </UDashboardPanel>
</template>
