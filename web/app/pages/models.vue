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
        title="Models and providers come from the gateway config file."
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
        <template #upstream-cell="{ row }">
          <code class="text-muted">{{ row.original.upstream }}</code>
        </template>
        <template #price-cell="{ row }">
          <span v-if="row.original.price" class="tabular-nums">
            in ${{ row.original.price.input }} · out ${{ row.original.price.output }}
          </span>
          <span v-else class="text-muted">provider-reported or free</span>
        </template>
      </UTable>
    </template>
  </UDashboardPanel>
</template>
