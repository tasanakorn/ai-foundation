<script setup lang="ts">
import type { AuthMethods } from '~/types/admin'

definePageMeta({ layout: false })

const { login } = useSession()
const toast = useToast()
const route = useRoute()

// Sign-in methods: identity provider buttons, and the local account's password form.
const { data: methods } = await useAsyncData('auth-methods', () => $fetch<AuthMethods>('/admin/v1/auth/methods'))
const error = computed(() => typeof route.query.error === 'string' ? route.query.error : '')

const form = reactive({ email: '', password: '' })
const loading = ref(false)
const showPassword = ref(false)
const passwordOnly = computed(() => (methods.value?.oidc.length ?? 0) === 0)

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

      <div class="space-y-4">
        <UAlert v-if="error" icon="i-lucide-circle-alert" color="error" variant="subtle" :title="error" />

        <UButton
          v-for="p in methods?.oidc ?? []"
          :key="p.name"
          :href="`/admin/v1/auth/oidc/${p.name}/start`"
          external
          block
          icon="i-lucide-log-in"
          :label="`Sign in with ${p.title}`"
        />

        <template v-if="methods?.password">
          <UButton
            v-if="!passwordOnly && !showPassword"
            block
            color="neutral"
            variant="link"
            label="Use the local break-glass account"
            @click="showPassword = true"
          />
          <form v-if="passwordOnly || showPassword" class="space-y-4" @submit.prevent="submit">
            <UFormField label="Email">
              <UInput v-model="form.email" type="email" autocomplete="username" :autofocus="passwordOnly" class="w-full" placeholder="admin@example.com" />
            </UFormField>
            <UFormField label="Password">
              <UInput v-model="form.password" type="password" autocomplete="current-password" class="w-full" />
            </UFormField>
            <UButton type="submit" block :loading="loading" label="Sign in" :color="passwordOnly ? 'primary' : 'warning'" />
          </form>
        </template>

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
