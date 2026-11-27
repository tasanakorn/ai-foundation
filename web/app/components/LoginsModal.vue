<script setup lang="ts">
import type { AdminIdentity, AdminUser, AuthMethods, ListResponse } from '~/types/admin'

/**
 * OIDC logins linked to an account. Without user: your own, with buttons to link
 * another identity provider. With user: that account's (admin view).
 */
const props = defineProps<{ user?: AdminUser | null }>()
const open = defineModel<boolean>('open', { default: false })

const api = useAdminApi()
const toast = useToast()

const links = ref<AdminIdentity[]>([])
const methods = ref<AuthMethods | null>(null)
const loading = ref(false)
const base = computed(() => props.user ? `/users/${props.user.id}/identities` : '/auth/identities')

async function load() {
  loading.value = true
  try {
    links.value = (await api<ListResponse<AdminIdentity>>(base.value)).data
    if (!props.user) methods.value = await api<AuthMethods>('/auth/methods')
  }
  catch (err) {
    toast.add({ title: 'Could not load logins', description: apiErrorMessage(err), color: 'error' })
  }
  finally {
    loading.value = false
  }
}

watch(open, (v) => { if (v) load() })

async function remove(l: AdminIdentity) {
  try {
    await api(`${base.value}/${l.id}`, { method: 'DELETE' })
    toast.add({ title: 'Login removed', color: 'success' })
    load()
  }
  catch (err) {
    toast.add({ title: 'Could not remove login', description: apiErrorMessage(err), color: 'error' })
  }
}
</script>

<template>
  <UModal
    v-model:open="open"
    :title="user ? `Logins of ${user.email}` : 'My logins'"
    description="Identity provider logins that sign in to this account."
  >
    <template #body>
      <p v-if="!loading && links.length === 0" class="text-sm text-muted">
        No login is linked yet.
      </p>
      <ul class="divide-y divide-(--ui-border)">
        <li v-for="l in links" :key="l.id" class="flex items-center justify-between gap-2 py-2">
          <div class="min-w-0">
            <p class="font-medium truncate">
              {{ l.email || l.subject }}
            </p>
            <p class="text-xs text-muted truncate">
              {{ l.provider }} - {{ l.issuer }} - last sign-in {{ fmtTime(l.last_login_at) }}
            </p>
          </div>
          <UButton icon="i-lucide-unlink" color="error" variant="ghost" size="sm" label="Remove" @click="remove(l)" />
        </li>
      </ul>
      <div v-if="!user && methods?.oidc.length" class="mt-4 space-y-2">
        <p class="text-sm text-muted">
          Link another login:
        </p>
        <UButton
          v-for="p in methods.oidc"
          :key="p.name"
          :href="`/admin/v1/auth/oidc/${p.name}/start?link=1`"
          external
          icon="i-lucide-link"
          color="neutral"
          variant="outline"
          :label="p.title"
          block
        />
      </div>
    </template>
  </UModal>
</template>
