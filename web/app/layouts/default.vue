<script setup lang="ts">
import type { DropdownMenuItem, NavigationMenuItem } from '@nuxt/ui'

const { me, logout } = useSession()

const links: NavigationMenuItem[] = [
  { label: 'Usage', icon: 'i-lucide-chart-column', to: '/' },
  { label: 'API keys', icon: 'i-lucide-key-round', to: '/keys' },
  { label: 'Models', icon: 'i-lucide-boxes', to: '/models' },
  { label: 'Providers', icon: 'i-lucide-server', to: '/providers' },
  { label: 'Playground', icon: 'i-lucide-messages-square', to: '/playground' },
  { label: 'Admins', icon: 'i-lucide-users', to: '/users' },
]

const who = computed(() => me.value?.user?.name || me.value?.user?.email || 'Master key')

const isLocal = computed(() => me.value?.user?.local === true)

const menu = computed<DropdownMenuItem[][]>(() => [
  [{ label: `${me.value?.user?.email ?? 'Master key'} (${me.value?.role})`, type: 'label' }],
  [
    ...(me.value?.user && !isLocal.value ? [{ label: 'My logins', icon: 'i-lucide-link', onSelect: () => { loginsOpen.value = true } }] : []),
    { label: 'Sign out', icon: 'i-lucide-log-out', onSelect: () => logout() },
  ],
])

const loginsOpen = ref(false)
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
        <UAlert
          v-if="isLocal && !collapsed"
          icon="i-lucide-triangle-alert"
          color="warning"
          variant="subtle"
          title="Break-glass account"
          description="Use it only when the identity provider is down. Every use is logged."
        />
        <UTooltip v-else-if="isLocal" text="Break-glass account: every use is logged">
          <UIcon name="i-lucide-triangle-alert" class="size-5 text-warning mx-auto" />
        </UTooltip>
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

  <LoginsModal v-model:open="loginsOpen" />
</template>
