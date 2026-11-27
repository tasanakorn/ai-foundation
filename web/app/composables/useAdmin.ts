import type { Me } from '~/types/admin'

/**
 * The signed-in admin. The session is an HttpOnly cookie set by the gateway, so
 * scripts never see the token; we only keep who is signed in.
 */
export function useSession() {
  const me = useState<Me | null>('me', () => null)
  const canWrite = computed(() => me.value?.role === 'admin')

  /** Loads the current user. Returns false when not signed in. */
  async function load(): Promise<boolean> {
    try {
      me.value = await $fetch<Me>('/admin/v1/auth/me')
      return true
    }
    catch {
      me.value = null
      return false
    }
  }

  async function login(email: string, password: string) {
    me.value = await $fetch<Me>('/admin/v1/auth/login', { method: 'POST', body: { email, password } })
  }

  async function logout() {
    try {
      await $fetch('/admin/v1/auth/logout', { method: 'POST' })
    }
    finally {
      me.value = null
      // No auto redirect here: the IdP session would sign the user straight back in.
      await navigateTo('/login?signed_out=1')
    }
  }

  return { me, canWrite, load, login, logout }
}

/** Fetch client for /admin/v1. A 401 (session ended) returns to the login page. */
export function useAdminApi() {
  const { me } = useSession()
  return $fetch.create({
    baseURL: '/admin/v1',
    onResponseError({ response }) {
      if (response.status === 401) {
        me.value = null
        navigateTo(loginPath(window.location.pathname + window.location.search))
      }
    },
  })
}

const NEXT_KEY = 'aif-next'

/** A local path only, so a link cannot send the browser to another site after sign-in. */
export function safeNext(p: unknown): string {
  return typeof p === 'string' && p.startsWith('/') && !p.startsWith('//') && !p.startsWith('/\\') && !p.startsWith('/login') ? p : ''
}

/** The sign-in page, keeping the page that was asked for. */
export function loginPath(from: string): string {
  const next = safeNext(from)
  return next && next !== '/' ? `/login?next=${encodeURIComponent(next)}` : '/login'
}

/** Keeps the deep link across the IdP round trip (the IdP returns to /). */
export function keepNext(next: string) {
  if (safeNext(next)) sessionStorage.setItem(NEXT_KEY, next)
  else sessionStorage.removeItem(NEXT_KEY)
}

/** The kept deep link, once. */
export function takeNext(): string {
  const next = safeNext(sessionStorage.getItem(NEXT_KEY))
  sessionStorage.removeItem(NEXT_KEY)
  return next
}

/** Readable message from an admin API error (OpenAI error shape). */
export function apiErrorMessage(err: unknown): string {
  const e = err as { data?: { error?: { message?: string } }, message?: string }
  return e?.data?.error?.message ?? e?.message ?? 'Request failed'
}
