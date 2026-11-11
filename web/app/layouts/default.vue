<script setup lang="ts">
import type { NavigationMenuItem } from '@nuxt/ui'

const { set } = useMasterKey()

const links: NavigationMenuItem[] = [
  { label: 'Usage', icon: 'i-lucide-chart-column', to: '/' },
  { label: 'API keys', icon: 'i-lucide-key-round', to: '/keys' },
  { label: 'Models', icon: 'i-lucide-boxes', to: '/models' },
]

function signOut() {
  set('')
  navigateTo('/login')
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
          <UColorModeButton />
          <UButton
            icon="i-lucide-log-out"
            :label="collapsed ? undefined : 'Sign out'"
            color="neutral"
            variant="ghost"
            @click="signOut"
          />
        </div>
      </template>
    </UDashboardSidebar>

    <slot />
  </UDashboardGroup>
</template>
