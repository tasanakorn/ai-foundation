export default defineNuxtRouteMiddleware(async (to) => {
  const { me, load } = useSession()
  if (!me.value) await load()
  if (!me.value && to.path !== '/login') return navigateTo('/login')
  if (me.value && to.path === '/login') return navigateTo('/')
})
