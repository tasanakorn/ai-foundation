<script setup lang="ts">
import type { TableColumn } from '@nuxt/ui'
import type { ApiKey, CreatedKey, CreateKeyInput, ListResponse } from '~/types/admin'

const api = useAdminApi()
const toast = useToast()
const { canWrite } = useSession()

const filters = reactive({ tenant_id: '', app_id: '', include_revoked: false })
const query = computed(() => ({
  tenant_id: filters.tenant_id || undefined,
  app_id: filters.app_id || undefined,
  include_revoked: filters.include_revoked ? 'true' : undefined,
}))
const { data, status, refresh } = await useAsyncData('keys',
  () => api<ListResponse<ApiKey>>('/keys', { query: query.value }),
  { watch: [query] })

function keyState(k: ApiKey): { label: string, color: 'success' | 'error' | 'warning' } {
  if (k.revoked_at) return { label: 'Revoked', color: 'error' }
  if (k.expires_at && new Date(k.expires_at) <= new Date()) return { label: 'Expired', color: 'warning' }
  return { label: 'Active', color: 'success' }
}

const columns: TableColumn<ApiKey>[] = [
  { accessorKey: 'name', header: 'Name' },
  { accessorKey: 'prefix', header: 'Key' },
  { accessorKey: 'tenant_id', header: 'Tenant' },
  { accessorKey: 'app_id', header: 'App' },
  { accessorKey: 'allowed_models', header: 'Models' },
  { accessorKey: 'created_at', header: 'Created', cell: ({ row }) => fmtDate(row.original.created_at) },
  { accessorKey: 'expires_at', header: 'Expires', cell: ({ row }) => fmtDate(row.original.expires_at) },
  { id: 'state', header: 'Status' },
  { id: 'actions', header: '' },
]

// ---- create ----

const createOpen = ref(false)
const creating = ref(false)
const created = ref<CreatedKey | null>(null)
const form = reactive({ name: '', tenant_id: '', app_id: '', allowed_models: '', metadata: '', expires: '' })

function openCreate() {
  Object.assign(form, { name: '', tenant_id: '', app_id: '', allowed_models: '', metadata: '', expires: '' })
  created.value = null
  createOpen.value = true
}

function parseMetadata(text: string): Record<string, string> {
  const out: Record<string, string> = {}
  for (const line of text.split('\n')) {
    const i = line.indexOf('=')
    if (i > 0) out[line.slice(0, i).trim()] = line.slice(i + 1).trim()
  }
  return out
}

async function create() {
  creating.value = true
  const body: CreateKeyInput = {
    name: form.name.trim(),
    tenant_id: form.tenant_id.trim(),
    app_id: form.app_id.trim(),
    allowed_models: form.allowed_models.split(/[\s,]+/).filter(Boolean),
    metadata: parseMetadata(form.metadata),
    // End of the chosen day, local time.
    expires_at: form.expires ? new Date(`${form.expires}T23:59:59`).toISOString() : undefined,
  }
  try {
    created.value = await api<CreatedKey>('/keys', { method: 'POST', body })
    refresh()
  }
  catch (err) {
    toast.add({ title: 'Could not create key', description: apiErrorMessage(err), color: 'error' })
  }
  finally {
    creating.value = false
  }
}

async function copySecret() {
  if (!created.value) return
  await navigator.clipboard.writeText(created.value.secret)
  toast.add({ title: 'Key copied', color: 'success' })
}

// ---- revoke ----

const revoking = ref<ApiKey | null>(null)
const revokeOpen = computed({
  get: () => revoking.value !== null,
  set: (v) => { if (!v) revoking.value = null },
})

async function revoke() {
  if (!revoking.value) return
  try {
    await api(`/keys/${revoking.value.id}`, { method: 'DELETE' })
    toast.add({ title: `Revoked ${revoking.value.name || revoking.value.prefix}`, color: 'success' })
    refresh()
  }
  catch (err) {
    toast.add({ title: 'Could not revoke key', description: apiErrorMessage(err), color: 'error' })
  }
  finally {
    revoking.value = null
  }
}
</script>

<template>
  <UDashboardPanel id="keys">
    <template #header>
      <UDashboardNavbar title="API keys">
        <template #leading>
          <UDashboardSidebarCollapse />
        </template>
        <template #right>
          <UButton v-if="canWrite" icon="i-lucide-plus" label="New key" @click="openCreate" />
        </template>
      </UDashboardNavbar>
      <UDashboardToolbar>
        <div class="flex flex-wrap items-center gap-2 py-1">
          <UInput v-model.lazy="filters.tenant_id" placeholder="Tenant" icon="i-lucide-building-2" size="sm" />
          <UInput v-model.lazy="filters.app_id" placeholder="App" icon="i-lucide-app-window" size="sm" />
          <USwitch v-model="filters.include_revoked" label="Show revoked" size="sm" />
        </div>
      </UDashboardToolbar>
    </template>

    <template #body>
      <UTable :data="data?.data ?? []" :columns="columns" :loading="status === 'pending'">
        <template #name-cell="{ row }">
          <span class="font-medium">{{ row.original.name || '—' }}</span>
        </template>
        <template #prefix-cell="{ row }">
          <code class="text-xs">{{ row.original.prefix }}…</code>
        </template>
        <template #tenant_id-cell="{ row }">
          {{ row.original.tenant_id || '—' }}
        </template>
        <template #app_id-cell="{ row }">
          {{ row.original.app_id || '—' }}
        </template>
        <template #allowed_models-cell="{ row }">
          <span v-if="!row.original.allowed_models.length" class="text-muted">all</span>
          <div v-else class="flex flex-wrap gap-1">
            <UBadge v-for="m in row.original.allowed_models" :key="m" :label="m" color="neutral" variant="subtle" />
          </div>
        </template>
        <template #state-cell="{ row }">
          <UBadge v-bind="keyState(row.original)" variant="subtle" />
        </template>
        <template #actions-cell="{ row }">
          <UButton
            v-if="canWrite && !row.original.revoked_at"
            icon="i-lucide-ban"
            label="Revoke"
            color="error"
            variant="ghost"
            size="sm"
            @click="revoking = row.original"
          />
        </template>
      </UTable>
    </template>
  </UDashboardPanel>

  <UModal
    v-model:open="createOpen"
    :title="created ? 'Key created' : 'New API key'"
    :description="created ? 'Copy the key now. It is shown only once.' : 'Owner labels are free-form. Use them to split usage by tenant and app.'"
  >
    <template #body>
      <div v-if="created" class="space-y-3">
        <UInput :model-value="created.secret" readonly class="w-full font-mono" />
        <UAlert
          icon="i-lucide-shield-alert"
          color="warning"
          variant="subtle"
          title="Only a hash is stored. If you lose this key, revoke it and create a new one."
        />
      </div>
      <form v-else id="create-key" class="space-y-4" @submit.prevent="create">
        <UFormField label="Name">
          <UInput v-model="form.name" placeholder="chatbot-prod" class="w-full" autofocus />
        </UFormField>
        <div class="grid grid-cols-2 gap-3">
          <UFormField label="Tenant ID">
            <UInput v-model="form.tenant_id" placeholder="acme" class="w-full" />
          </UFormField>
          <UFormField label="App ID">
            <UInput v-model="form.app_id" placeholder="chatbot" class="w-full" />
          </UFormField>
        </div>
        <UFormField label="Allowed models" help="Comma or space separated. Empty = all. A trailing * matches a prefix, e.g. openrouter/anthropic/*">
          <UInput v-model="form.allowed_models" placeholder="local, openrouter/*" class="w-full" />
        </UFormField>
        <UFormField label="Expires" help="Optional. The key stops working after this day.">
          <UInput v-model="form.expires" type="date" class="w-full" />
        </UFormField>
        <UFormField label="Metadata" help="One key=value per line.">
          <UTextarea v-model="form.metadata" :rows="2" placeholder="plan=pro" class="w-full" />
        </UFormField>
      </form>
    </template>
    <template #footer>
      <div class="flex w-full justify-end gap-2">
        <template v-if="created">
          <UButton icon="i-lucide-copy" label="Copy key" @click="copySecret" />
          <UButton label="Done" color="neutral" variant="outline" @click="createOpen = false" />
        </template>
        <template v-else>
          <UButton label="Cancel" color="neutral" variant="outline" @click="createOpen = false" />
          <UButton type="submit" form="create-key" label="Create key" :loading="creating" />
        </template>
      </div>
    </template>
  </UModal>

  <UModal
    v-model:open="revokeOpen"
    title="Revoke key?"
    :description="`Requests with ${revoking?.prefix}… will fail right away. This cannot be undone.`"
  >
    <template #footer>
      <div class="flex w-full justify-end gap-2">
        <UButton label="Cancel" color="neutral" variant="outline" @click="revoking = null" />
        <UButton label="Revoke" color="error" @click="revoke" />
      </div>
    </template>
  </UModal>
</template>
