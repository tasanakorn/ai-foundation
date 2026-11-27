<script setup lang="ts">
import type { AuthMethods } from '~/types/admin'

definePageMeta({ layout: false })

const { login } = useSession()
const toast = useToast()
const route = useRoute()

// Sign-in methods: identity provider buttons, or the local account's password form.
// The password is never on together with an identity provider (spec-0001 D43).
const { data: methods } = await useAsyncData('auth-methods', () => $fetch<AuthMethods>('/admin/v1/auth/methods'))
const error = computed(() => typeof route.query.error === 'string' ? route.query.error : '')
const signedOut = computed(() => route.query.signed_out === '1')

// The page that was asked for, opened after sign-in (the IdP returns to /).
keepNext(typeof route.query.next === 'string' ? route.query.next : '')

const startURL = (name: string) => `/admin/v1/auth/oidc/${name}/start`

// Auto redirect, but never after an error or a sign-out, so it cannot loop (section 6.4).
const redirecting = ref(false)
const auto = methods.value?.auto_redirect ?? ''
if (auto && !error.value && !signedOut.value && methods.value?.oidc.some(p => p.name === auto)) {
  redirecting.value = true
  window.location.assign(startURL(auto))
}

const form = reactive({ email: '', password: '' })
const loading = ref(false)

async function submit() {
  if (!form.email || !form.password) return
  loading.value = true
  try {
    await login(form.email, form.password)
    await navigateTo('/')
  }
  catch (err) {
    toast.add({ title: 'Sign in failed', description: apiErrorMessage(err), color: 'error' })
  }
  finally {
    loading.value = false
  }
}
</script>

<template>
  <div class="min-h-screen flex items-center justify-center p-4 bg-(--ui-bg-muted)">
    <UCard class="w-full max-w-sm">
      <template #header>
        <div class="flex items-center gap-2">
          <UIcon name="i-lucide-waypoints" class="size-5 text-primary" />
          <span class="font-semibold">aif admin</span>
        </div>
      </template>

      <p v-if="redirecting" class="text-sm text-muted">
        Going to sign-in...
      </p>

      <div v-else class="space-y-4">
        <UAlert v-if="error" icon="i-lucide-circle-alert" color="error" variant="subtle" :title="error" />
        <UAlert v-else-if="signedOut" icon="i-lucide-log-out" color="neutral" variant="subtle" title="Signed out" />

        <UButton
          v-for="p in methods?.oidc ?? []"
          :key="p.name"
          :href="startURL(p.name)"
          external
          block
          :icon="error ? 'i-lucide-rotate-cw' : 'i-lucide-log-in'"
          :label="error ? `Try again with ${p.title}` : `Sign in with ${p.title}`"
        />

        <form v-if="methods?.password" class="space-y-4" @submit.prevent="submit">
          <UFormField label="Email">
            <UInput v-model="form.email" type="email" autocomplete="username" autofocus class="w-full" placeholder="admin@example.com" />
          </UFormField>
          <UFormField label="Password">
            <UInput v-model="form.password" type="password" autocomplete="current-password" class="w-full" />
          </UFormField>
          <UButton type="submit" block :loading="loading" label="Sign in" />
        </form>

        <p v-if="methods && !methods.password && methods.oidc.length === 0" class="text-sm text-muted">
          No sign-in method is on. Set <code>admin.oidc</code> or <code>admin.bootstrap</code> in the gateway config.
        </p>
      </div>

      <template #footer>
        <p class="text-xs text-muted">
          No access yet? Ask an admin to add your account and allow a login link.
        </p>
      </template>
    </UCard>
  </div>
</template>
