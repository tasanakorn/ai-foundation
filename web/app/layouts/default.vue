<script setup lang="ts">
import type { DropdownMenuItem, NavigationMenuItem } from '@nuxt/ui'

const { me, logout } = useSession()
const api = useAdminApi()
const toast = useToast()

const links: NavigationMenuItem[] = [
  { label: 'Usage', icon: 'i-lucide-chart-column', to: '/' },
  { label: 'API keys', icon: 'i-lucide-key-round', to: '/keys' },
  { label: 'Models', icon: 'i-lucide-boxes', to: '/models' },
  { label: 'Admins', icon: 'i-lucide-users', to: '/users' },
]

const who = computed(() => me.value?.user?.name || me.value?.user?.email || 'Master key')

const menu = computed<DropdownMenuItem[][]>(() => [
  [{ label: `${me.value?.user?.email ?? 'Master key'} (${me.value?.role})`, type: 'label' }],
  [
    ...(me.value?.user ? [{ label: 'Change password', icon: 'i-lucide-lock-keyhole', onSelect: () => { pwOpen.value = true } }] : []),
    { label: 'Sign out', icon: 'i-lucide-log-out', onSelect: () => logout() },
  ],
])

// ---- change own password ----

const pwOpen = ref(false)
const pw = reactive({ current: '', next: '', repeat: '' })
const pwSaving = ref(false)

watch(pwOpen, (open) => {
  if (open) Object.assign(pw, { current: '', next: '', repeat: '' })
})

async function changePassword() {
  if (pw.next !== pw.repeat) {
    toast.add({ title: 'New passwords do not match', color: 'error' })
    return
  }
  pwSaving.value = true
  try {
    await api('/auth/password', { method: 'POST', body: { current_password: pw.current, new_password: pw.next } })
    toast.add({ title: 'Password changed', description: 'Your other sessions were signed out.', color: 'success' })
    pwOpen.value = false
  }
  catch (err) {
    toast.add({ title: 'Could not change password', description: apiErrorMessage(err), color: 'error' })
  }
  finally {
    pwSaving.value = false
  }
}
</script>

<template>
  <UDashboardGroup>
    <UDashboardSidebar collapsible>
      <template #header="{ collapsed }">
        <div class="flex items-center gap-2 px-1">
          <UIcon name="i-lucide-waypoints" class="size-5 text-primary shrink-0" />
          <span v-if="!collapsed" class="font-semibold">aif admin</span>
        </div>
      </template>

      <template #default="{ collapsed }">
        <UNavigationMenu :items="links" orientation="vertical" :collapsed="collapsed" />
      </template>

      <template #footer="{ collapsed }">
        <div class="flex w-full items-center gap-1" :class="collapsed ? 'flex-col' : ''">
          <UDropdownMenu :items="menu" :content="{ side: 'top', align: 'start' }" class="min-w-0 flex-1">
            <UButton
              icon="i-lucide-circle-user-round"
              :label="collapsed ? undefined : who"
              color="neutral"
              variant="ghost"
              block
              class="justify-start"
            />
          </UDropdownMenu>
          <UColorModeButton />
        </div>
      </template>
    </UDashboardSidebar>

    <slot />
  </UDashboardGroup>

  <UModal v-model:open="pwOpen" title="Change password" description="Your other sessions will be signed out.">
    <template #body>
      <form id="change-password" class="space-y-4" @submit.prevent="changePassword">
        <UFormField label="Current password">
          <UInput v-model="pw.current" type="password" autocomplete="current-password" class="w-full" autofocus />
        </UFormField>
        <UFormField label="New password" help="At least 12 characters.">
          <UInput v-model="pw.next" type="password" autocomplete="new-password" class="w-full" />
        </UFormField>
        <UFormField label="Repeat new password">
          <UInput v-model="pw.repeat" type="password" autocomplete="new-password" class="w-full" />
        </UFormField>
      </form>
    </template>
    <template #footer>
      <div class="flex w-full justify-end gap-2">
        <UButton label="Cancel" color="neutral" variant="outline" @click="pwOpen = false" />
        <UButton type="submit" form="change-password" label="Change password" :loading="pwSaving" />
      </div>
    </template>
  </UModal>
</template>
