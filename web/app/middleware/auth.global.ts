export default defineNuxtRouteMiddleware((to) => {
  const { key } = useMasterKey()
  if (!key.value && to.path !== '/login') {
    return navigateTo('/login')
  }
})
