<script setup lang="ts">
definePageMeta({ layout: false })

const { login } = useSession()
const toast = useToast()

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

      <form class="space-y-4" @submit.prevent="submit">
        <UFormField label="Email">
          <UInput v-model="form.email" type="email" autocomplete="username" autofocus class="w-full" placeholder="admin@example.com" />
        </UFormField>
        <UFormField label="Password">
          <UInput v-model="form.password" type="password" autocomplete="current-password" class="w-full" />
        </UFormField>
        <UButton type="submit" block :loading="loading" label="Sign in" />
      </form>

      <template #footer>
        <p class="text-xs text-muted">
          No account yet? Set <code>admin.bootstrap</code> in the gateway config, or run
          <code>aif admin create-user -email you@example.com</code>.
        </p>
      </template>
    </UCard>
  </div>
</template>
