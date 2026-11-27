export default defineNuxtRouteMiddleware(async (to) => {
  const { me, load } = useSession()
  if (!me.value) await load()
  if (!me.value && to.path !== '/login') return navigateTo(loginPath(to.fullPath))
  if (me.value) {
    // After sign-in (the IdP returns to /), open the page that was asked for.
    const next = takeNext()
    if (to.path === '/login') return navigateTo(next || '/')
    if (next && next !== to.fullPath) return navigateTo(next)
  }
})
