<script setup lang="ts">
import type { TableColumn } from '@nuxt/ui'
import type { AccountState, CreditEntry, CreditEntryInput, CreditState, ListResponse, ProvidersResponse } from '~/types/admin'

const api = useAdminApi()
const toast = useToast()
const { canWrite } = useSession()
const { data, status, refresh } = await useAsyncData('providers', () => api<ProvidersResponse>('/providers'))

const columns: TableColumn<AccountState>[] = [
  { accessorKey: 'name', header: 'Account' },
  { accessorKey: 'weight', header: 'Weight' },
  { accessorKey: 'state', header: 'State' },
  { accessorKey: 'credit', header: 'Credit' },
  { accessorKey: 'last_error', header: 'Last error' },
]

function policyText(p: { policy: string, sticky: string, max_attempts: number, cooldown: string }) {
  const sticky = p.sticky !== 'none' ? `, sticky by ${p.sticky.replace('_', ' ')}` : ''
  return `${p.policy.replace('_', ' ')}${sticky} - up to ${p.max_attempts} tries - rest ${p.cooldown}`
}

function fmtTime(s?: string) {
  return s ? new Date(s).toLocaleTimeString() : ''
}

const usd = (n = 0) => (n < 0 ? '-$' : '$') + Math.abs(n).toFixed(2)
const creditColor = { ok: 'success', warning: 'warning', exhausted: 'error' } as const
const creditLabel = { monthly: 'Plan credit', prepaid: 'Prepaid', payg: 'Pay-as-you-go' } as const

// The cycle in the credit's own time zone; cycle_end is exclusive, so the last day is the day before.
function cycleText(c: CreditState) {
  if (!c.cycle_start || !c.cycle_end) return ''
  const day = (ms: number) => new Date(ms).toLocaleDateString(undefined, { timeZone: c.timezone || 'UTC', day: 'numeric', month: 'short' })
  return `${day(Date.parse(c.cycle_start))} - ${day(Date.parse(c.cycle_end) - 1)} (${c.timezone || 'UTC'})`
}

function creditText(c: CreditState) {
  switch (c.type) {
    case 'monthly': return `${usd(c.spent_usd)} of ${usd(c.usd)} used`
    case 'payg': return `${usd(c.spent_usd)} of ${usd(c.usd)} budget`
    case 'prepaid': return c.since ? `${usd(c.balance_usd)} left of ${usd(c.topups_usd)}` : 'No top-up yet'
  }
}

// ---- prepaid ledger ----

const ledgerOpen = ref(false)
const ledgerFor = ref<{ provider: string, account: string } | null>(null)
const entries = ref<CreditEntry[]>([])
const saving = ref(false)
const form = reactive({ usd: '', note: '', date: '' })

const entryColumns: TableColumn<CreditEntry>[] = [
  { accessorKey: 'at', header: 'Date', cell: ({ row }) => new Date(row.original.at).toLocaleDateString() },
  { accessorKey: 'usd', header: 'USD' },
  { accessorKey: 'note', header: 'Note' },
  { accessorKey: 'actor', header: 'By' },
]

async function loadEntries() {
  if (!ledgerFor.value) return
  const res = await api<ListResponse<CreditEntry>>('/credit-entries', { query: ledgerFor.value })
  entries.value = res.data
}

async function openLedger(provider: string, account: string) {
  ledgerFor.value = { provider, account }
  Object.assign(form, { usd: '', note: '', date: '' })
  entries.value = []
  ledgerOpen.value = true
  try {
    await loadEntries()
  }
  catch (err) {
    toast.add({ title: 'Could not load the ledger', description: apiErrorMessage(err), color: 'error' })
  }
}

async function addEntry() {
  if (!ledgerFor.value) return
  const amount = Number(form.usd)
  if (!Number.isFinite(amount) || amount === 0) {
    toast.add({ title: 'Enter an amount other than 0', color: 'error' })
    return
  }
  saving.value = true
  const body: CreditEntryInput = {
    ...ledgerFor.value,
    usd: amount,
    note: form.note.trim(),
    // A chosen day means the start of that day, local time; empty means now.
    at: form.date ? new Date(`${form.date}T00:00:00`).toISOString() : undefined,
  }
  try {
    await api<CreditEntry>('/credit-entries', { method: 'POST', body })
    toast.add({ title: `Recorded ${usd(amount)} for ${body.provider}/${body.account}`, color: 'success' })
    Object.assign(form, { usd: '', note: '', date: '' })
    await Promise.all([loadEntries(), refresh()])
  }
  catch (err) {
    toast.add({ title: 'Could not record the entry', description: apiErrorMessage(err), color: 'error' })
  }
  finally {
    saving.value = false
  }
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
        :description="`This page shows node ${data?.node ?? '?'}. An account rests after a rate limit, outage or bad key, and the next account takes its requests. Credit spend is the cost the gateway metered on all nodes, so it can differ a little from the provider's bill.`"
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
          <template #credit-cell="{ row }">
            <div v-if="row.original.credit" class="min-w-56 space-y-1">
              <div class="flex items-center justify-between gap-2 text-sm">
                <span class="text-muted">{{ creditLabel[row.original.credit.type] }}</span>
                <UBadge :label="`${row.original.credit.used_percent}%`" :color="creditColor[row.original.credit.level]" variant="subtle" />
              </div>
              <div class="text-sm tabular-nums">{{ creditText(row.original.credit) }}</div>
              <UProgress :model-value="Math.min(row.original.credit.used_percent, 100)" :color="creditColor[row.original.credit.level]" size="xs" />
              <div v-if="row.original.credit.type !== 'prepaid'" class="text-xs text-muted">{{ cycleText(row.original.credit) }}</div>
              <UButton
                v-else
                size="xs"
                variant="link"
                class="px-0"
                :icon="canWrite ? 'i-lucide-plus' : 'i-lucide-list'"
                :label="canWrite ? 'Top-ups' : 'Ledger'"
                @click="openLedger(p.name, row.original.name)"
              />
            </div>
            <span v-else class="text-muted">-</span>
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

  <UModal
    v-model:open="ledgerOpen"
    :title="`Ledger of ${ledgerFor?.provider}/${ledgerFor?.account}`"
    description="Top-ups and corrections of this prepaid account. Entries cannot be changed or deleted: record a negative amount to correct the balance."
  >
    <template #body>
      <div class="space-y-4">
        <form v-if="canWrite" id="add-entry" class="space-y-3" @submit.prevent="addEntry">
          <div class="grid grid-cols-2 gap-3">
            <UFormField label="Amount (USD)" help="Negative corrects the balance.">
              <UInput v-model="form.usd" type="number" step="0.01" placeholder="50" class="w-full" autofocus />
            </UFormField>
            <UFormField label="Date" help="When the money arrived. Empty = now.">
              <UInput v-model="form.date" type="date" class="w-full" />
            </UFormField>
          </div>
          <UFormField label="Note">
            <UInput v-model="form.note" maxlength="200" placeholder="invoice number" class="w-full" />
          </UFormField>
        </form>
        <UTable :data="entries" :columns="entryColumns" :empty="'No entries yet'">
          <template #usd-cell="{ row }">
            <span class="tabular-nums" :class="row.original.usd < 0 ? 'text-error' : ''">{{ usd(row.original.usd) }}</span>
          </template>
        </UTable>
      </div>
    </template>
    <template #footer>
      <div class="flex w-full justify-end gap-2">
        <UButton label="Close" color="neutral" variant="outline" @click="ledgerOpen = false" />
        <UButton v-if="canWrite" type="submit" form="add-entry" label="Record" :loading="saving" />
      </div>
    </template>
  </UModal>
</template>
