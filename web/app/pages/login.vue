<script setup lang="ts">
definePageMeta({ layout: false })

const { set } = useMasterKey()
const api = useAdminApi()
const toast = useToast()

const value = ref('')
const loading = ref(false)

async function submit() {
  if (!value.value) return
  loading.value = true
  set(value.value)
  try {
    await api('/models')
    await navigateTo('/')
  }
  catch (err) {
    set('')
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
        <UFormField label="Master key" help="Set by admin.master_key in the gateway config.">
          <UInput v-model="value" type="password" autofocus class="w-full" placeholder="AIF_MASTER_KEY" />
        </UFormField>
        <UButton type="submit" block :loading="loading" label="Sign in" />
      </form>
    </UCard>
  </div>
</template>
