<script setup lang="ts">
import type { DropdownMenuItem, TableColumn } from '@nuxt/ui'
import type { AdminUser, CreateUserInput, ListResponse, Role, UpdateUserInput } from '~/types/admin'

const api = useAdminApi()
const toast = useToast()
const { me, canWrite } = useSession()

const { data, status, refresh } = await useAsyncData('users', () => api<ListResponse<AdminUser>>('/users'))

const roleItems = [
  { label: 'Admin: full access', value: 'admin' },
  { label: 'Viewer: read only', value: 'viewer' },
]

const columns: TableColumn<AdminUser>[] = [
  { accessorKey: 'email', header: 'Email' },
  { accessorKey: 'name', header: 'Name' },
  { accessorKey: 'role', header: 'Role' },
  { id: 'state', header: 'Status' },
  { accessorKey: 'last_login_at', header: 'Last sign-in', cell: ({ row }) => fmtTime(row.original.last_login_at) },
  { accessorKey: 'created_at', header: 'Created', cell: ({ row }) => fmtDate(row.original.created_at) },
  { id: 'actions', header: '' },
]

const isSelf = (u: AdminUser) => u.id === me.value?.user?.id

async function update(u: AdminUser, body: UpdateUserInput, done: string) {
  try {
    await api(`/users/${u.id}`, { method: 'PATCH', body })
    toast.add({ title: done, color: 'success' })
    refresh()
    return true
  }
  catch (err) {
    toast.add({ title: 'Could not update account', description: apiErrorMessage(err), color: 'error' })
    return false
  }
}

function actions(u: AdminUser): DropdownMenuItem[][] {
  const other: Role = u.role === 'admin' ? 'viewer' : 'admin'
  return [
    [
      { label: 'Edit name', icon: 'i-lucide-pencil', onSelect: () => openEdit(u) },
      { label: `Make ${other}`, icon: 'i-lucide-shield', onSelect: () => update(u, { role: other }, `${u.email} is now ${other}`) },
      { label: 'Set password', icon: 'i-lucide-lock-keyhole', onSelect: () => openPassword(u) },
    ],
    isSelf(u)
      ? []
      : [
          u.disabled_at
            ? { label: 'Enable', icon: 'i-lucide-circle-check', onSelect: () => update(u, { disabled: false }, `${u.email} enabled`) }
            : { label: 'Disable', icon: 'i-lucide-circle-pause', onSelect: () => update(u, { disabled: true }, `${u.email} disabled and signed out`) },
          { label: 'Delete', icon: 'i-lucide-trash-2', color: 'error' as const, onSelect: () => { deleting.value = u } },
        ],
  ]
}

// ---- create ----

const createOpen = ref(false)
const creating = ref(false)
const form = reactive<CreateUserInput>({ email: '', name: '', role: 'admin', password: '' })

function openCreate() {
  Object.assign(form, { email: '', name: '', role: 'admin', password: '' })
  createOpen.value = true
}

async function create() {
  creating.value = true
  try {
    await api('/users', { method: 'POST', body: { ...form, email: form.email.trim(), name: form.name.trim() } })
    toast.add({ title: `Created ${form.email}`, description: 'Share the password in a safe way. They can change it after sign-in.', color: 'success' })
    createOpen.value = false
    refresh()
  }
  catch (err) {
    toast.add({ title: 'Could not create account', description: apiErrorMessage(err), color: 'error' })
  }
  finally {
    creating.value = false
  }
}

// ---- edit name ----

const editing = ref<AdminUser | null>(null)
const editName = ref('')
const editOpen = computed({ get: () => editing.value !== null, set: (v) => { if (!v) editing.value = null } })

function openEdit(u: AdminUser) {
  editing.value = u
  editName.value = u.name
}

async function saveName() {
  if (editing.value && await update(editing.value, { name: editName.value }, 'Name saved')) editing.value = null
}

// ---- set password ----

const pwUser = ref<AdminUser | null>(null)
const pwValue = ref('')
const pwOpen = computed({ get: () => pwUser.value !== null, set: (v) => { if (!v) pwUser.value = null } })

function openPassword(u: AdminUser) {
  pwUser.value = u
  pwValue.value = ''
}

async function savePassword() {
  if (!pwUser.value) return
  const u = pwUser.value
  if (await update(u, { password: pwValue.value }, `Password set for ${u.email}; their sessions were signed out`)) {
    pwUser.value = null
    if (isSelf(u)) navigateTo('/login')
  }
}

// ---- delete ----

const deleting = ref<AdminUser | null>(null)
const deleteOpen = computed({ get: () => deleting.value !== null, set: (v) => { if (!v) deleting.value = null } })

async function remove() {
  if (!deleting.value) return
  try {
    await api(`/users/${deleting.value.id}`, { method: 'DELETE' })
    toast.add({ title: `Deleted ${deleting.value.email}`, color: 'success' })
    refresh()
  }
  catch (err) {
    toast.add({ title: 'Could not delete account', description: apiErrorMessage(err), color: 'error' })
  }
  finally {
    deleting.value = null
  }
}
</script>

<template>
  <UDashboardPanel id="users">
    <template #header>
      <UDashboardNavbar title="Admins">
        <template #leading>
          <UDashboardSidebarCollapse />
        </template>
        <template #right>
          <UButton v-if="canWrite" icon="i-lucide-user-plus" label="New admin" @click="openCreate" />
        </template>
      </UDashboardNavbar>
    </template>

    <template #body>
      <UAlert
        v-if="!canWrite"
        icon="i-lucide-eye"
        color="neutral"
        variant="subtle"
        title="You have the viewer role: you can see everything but change nothing."
        class="mb-4"
      />
      <UTable :data="data?.data ?? []" :columns="columns" :loading="status === 'pending'">
        <template #email-cell="{ row }">
          <span class="font-medium">{{ row.original.email }}</span>
          <UBadge v-if="isSelf(row.original)" label="you" size="sm" color="primary" variant="subtle" class="ms-2" />
        </template>
        <template #name-cell="{ row }">
          {{ row.original.name || '—' }}
        </template>
        <template #role-cell="{ row }">
          <UBadge :label="row.original.role" :color="row.original.role === 'admin' ? 'primary' : 'neutral'" variant="subtle" />
        </template>
        <template #state-cell="{ row }">
          <UBadge
            :label="row.original.disabled_at ? 'Disabled' : 'Active'"
            :color="row.original.disabled_at ? 'warning' : 'success'"
            variant="subtle"
          />
        </template>
        <template #actions-cell="{ row }">
          <UDropdownMenu v-if="canWrite" :items="actions(row.original)" :content="{ align: 'end' }">
            <UButton icon="i-lucide-ellipsis" color="neutral" variant="ghost" size="sm" aria-label="Actions" />
          </UDropdownMenu>
        </template>
      </UTable>
    </template>
  </UDashboardPanel>

  <UModal v-model:open="createOpen" title="New admin account" description="They sign in with this email and password.">
    <template #body>
      <form id="create-user" class="space-y-4" @submit.prevent="create">
        <UFormField label="Email" required>
          <UInput v-model="form.email" type="email" class="w-full" autofocus placeholder="ops@example.com" />
        </UFormField>
        <UFormField label="Name">
          <UInput v-model="form.name" class="w-full" />
        </UFormField>
        <UFormField label="Role">
          <USelect v-model="form.role" :items="roleItems" class="w-full" />
        </UFormField>
        <UFormField label="Password" help="At least 12 characters." required>
          <UInput v-model="form.password" type="password" autocomplete="new-password" class="w-full" />
        </UFormField>
      </form>
    </template>
    <template #footer>
      <div class="flex w-full justify-end gap-2">
        <UButton label="Cancel" color="neutral" variant="outline" @click="createOpen = false" />
        <UButton type="submit" form="create-user" label="Create account" :loading="creating" />
      </div>
    </template>
  </UModal>

  <UModal v-model:open="editOpen" :title="`Edit ${editing?.email}`">
    <template #body>
      <form id="edit-user" @submit.prevent="saveName">
        <UFormField label="Name">
          <UInput v-model="editName" class="w-full" autofocus />
        </UFormField>
      </form>
    </template>
    <template #footer>
      <div class="flex w-full justify-end gap-2">
        <UButton label="Cancel" color="neutral" variant="outline" @click="editing = null" />
        <UButton type="submit" form="edit-user" label="Save" />
      </div>
    </template>
  </UModal>

  <UModal
    v-model:open="pwOpen"
    :title="`Set password for ${pwUser?.email}`"
    description="All their sessions are signed out."
  >
    <template #body>
      <form id="set-password" @submit.prevent="savePassword">
        <UFormField label="New password" help="At least 12 characters.">
          <UInput v-model="pwValue" type="password" autocomplete="new-password" class="w-full" autofocus />
        </UFormField>
      </form>
    </template>
    <template #footer>
      <div class="flex w-full justify-end gap-2">
        <UButton label="Cancel" color="neutral" variant="outline" @click="pwUser = null" />
        <UButton type="submit" form="set-password" label="Set password" />
      </div>
    </template>
  </UModal>

  <UModal
    v-model:open="deleteOpen"
    title="Delete account?"
    :description="`${deleting?.email} is signed out and can no longer sign in. This cannot be undone.`"
  >
    <template #footer>
      <div class="flex w-full justify-end gap-2">
        <UButton label="Cancel" color="neutral" variant="outline" @click="deleting = null" />
        <UButton label="Delete" color="error" @click="remove" />
      </div>
    </template>
  </UModal>
</template>
