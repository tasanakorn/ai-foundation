const STORAGE_KEY = 'aif.masterKey'

function readStored(): string {
  try {
    return sessionStorage.getItem(STORAGE_KEY) ?? ''
  }
  catch {
    return ''
  }
}

/** Master key for the admin API. Kept in sessionStorage only (cleared when the tab closes). */
export function useMasterKey() {
  const key = useState<string>('masterKey', readStored)
  function set(value: string) {
    key.value = value
    try {
      if (value) sessionStorage.setItem(STORAGE_KEY, value)
      else sessionStorage.removeItem(STORAGE_KEY)
    }
    catch { /* storage blocked: keep in memory only */ }
  }
  return { key, set }
}

/** Fetch client for /admin/v1. A 401 clears the key and returns to the login page. */
export function useAdminApi() {
  const { key, set } = useMasterKey()
  return $fetch.create({
    baseURL: '/admin/v1',
    onRequest({ options }) {
      options.headers.set('Authorization', `Bearer ${key.value}`)
    },
    onResponseError({ response }) {
      if (response.status === 401) {
        set('')
        navigateTo('/login')
      }
    },
  })
}

/** Readable message from an admin API error (OpenAI error shape). */
export function apiErrorMessage(err: unknown): string {
  const e = err as { data?: { error?: { message?: string } }, message?: string }
  return e?.data?.error?.message ?? e?.message ?? 'Request failed'
}
