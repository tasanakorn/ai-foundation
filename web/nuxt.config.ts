// Admin UI for the aif gateway.
// Built as a static SPA (`pnpm build` -> .output/public) and served by the gateway admin
// listener via `admin.ui_dir`. Same origin as /admin/v1, so no CORS.
const adminURL = process.env.AIF_ADMIN_URL ?? 'http://127.0.0.1:8081'

export default defineNuxtConfig({
  compatibilityDate: '2026-11-01',
  ssr: false,
  modules: ['@nuxt/ui'],
  css: ['~/assets/css/main.css'],
  devtools: { enabled: false },
  telemetry: false,
  app: {
    head: { title: 'aif admin' },
  },
  // Local admin may run without internet: bundle used icons, never fetch from a CDN.
  icon: {
    provider: 'none',
    clientBundle: { scan: true },
  },
  nitro: {
    devProxy: {
      '/admin/v1': { target: `${adminURL}/admin/v1`, changeOrigin: true },
    },
  },
})
