<script setup lang="ts">
import type { TableColumn } from '@nuxt/ui'
import type { AccountState, ProvidersResponse } from '~/types/admin'

const api = useAdminApi()
const { data, status, refresh } = await useAsyncData('providers', () => api<ProvidersResponse>('/providers'))

const columns: TableColumn<AccountState>[] = [
  { accessorKey: 'name', header: 'Account' },
  { accessorKey: 'weight', header: 'Weight' },
  { accessorKey: 'state', header: 'State' },
  { accessorKey: 'last_error', header: 'Last error' },
]

function policyText(p: { policy: string, sticky: string, max_attempts: number, cooldown: string }) {
  const sticky = p.sticky !== 'none' ? `, sticky by ${p.sticky.replace('_', ' ')}` : ''
  return `${p.policy.replace('_', ' ')}${sticky} - up to ${p.max_attempts} tries - rest ${p.cooldown}`
}

function fmtTime(s?: string) {
  return s ? new Date(s).toLocaleTimeString() : ''
}
</script>

<template>
  <UDashboardPanel id="providers">
    <template #header>
      <UDashboardNavbar title="Providers">
        <template #leading>
          <UDashboardSidebarCollapse />
        </template>
        <template #right>
          <UButton icon="i-lucide-refresh-cw" color="neutral" variant="ghost" :loading="status === 'pending'" @click="refresh()" />
        </template>
      </UDashboardNavbar>
    </template>

    <template #body>
      <UAlert
        icon="i-lucide-info"
        color="neutral"
        variant="subtle"
        title="Account state is per gateway node."
        :description="`This page shows node ${data?.node ?? '?'}. An account rests after a rate limit, outage or bad key, and the next account takes its requests.`"
      />

      <UCard v-for="p in data?.data ?? []" :key="p.name">
        <template #header>
          <div class="flex flex-wrap items-center gap-2">
            <code class="font-semibold">{{ p.name }}</code>
            <UBadge :label="p.type" color="neutral" variant="outline" />
            <span class="text-sm text-muted">{{ policyText(p) }}</span>
          </div>
        </template>
        <UTable :data="p.accounts" :columns="columns">
          <template #name-cell="{ row }">
            <code>{{ row.original.name }}</code>
          </template>
          <template #weight-cell="{ row }">
            <span v-if="p.policy === 'round_robin'" class="tabular-nums">{{ row.original.weight }}</span>
            <span v-else class="text-muted">-</span>
          </template>
          <template #state-cell="{ row }">
            <UBadge v-if="row.original.state === 'ok'" label="ok" color="success" variant="subtle" />
            <UBadge v-else :label="`resting until ${fmtTime(row.original.cooldown_until)}`" color="warning" variant="subtle" />
          </template>
          <template #last_error-cell="{ row }">
            <span v-if="row.original.last_error" class="text-sm">
              {{ row.original.last_error }}
              <span class="text-muted">at {{ fmtTime(row.original.last_error_at) }}</span>
            </span>
            <span v-else class="text-muted">-</span>
          </template>
        </UTable>
      </UCard>
    </template>
  </UDashboardPanel>
</template>
