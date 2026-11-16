<script setup lang="ts">
import type { ListResponse, UsageRecord } from '~/types/admin'

// Chat with the gateway as a real client would: the request runs through /v1 with an
// API key (auth, allow-list, hook, upstream, streaming, metering), via the admin
// playground route on the same origin.

interface Usage { prompt_tokens?: number, completion_tokens?: number, total_tokens?: number }
interface Meta {
  model?: string
  requestId?: string
  status?: number
  ttftMs?: number
  totalMs?: number
  usage?: Usage
  record?: UsageRecord
  stopped?: boolean
}
interface Msg {
  role: 'user' | 'assistant'
  content: string
  reasoning?: string
  error?: string
  meta?: Meta
}

const KEY_STORE = 'aif.playground.key'
const toast = useToast()
const api = useAdminApi()
const { me } = useSession()

function stored(): string {
  try {
    return sessionStorage.getItem(KEY_STORE) ?? ''
  }
  catch {
    return ''
  }
}

const apiKey = ref(stored())
const key = computed(() => apiKey.value.trim()) // pasted keys often carry spaces or a newline
const showKey = ref(false)
watch(apiKey, (v) => {
  try {
    if (v) sessionStorage.setItem(KEY_STORE, v)
    else sessionStorage.removeItem(KEY_STORE)
  }
  catch { /* storage blocked: keep in memory */ }
})

const model = ref('')
const models = ref<string[]>([])
const endUser = ref('')
const system = ref('')
const showSystem = ref(false)
const input = ref('')
const messages = ref<Msg[]>([])
const busy = ref(false)
let ctrl: AbortController | null = null

// ---- models the key may use ----

const keyError = ref('') // why the key could not list models

async function loadModels() {
  keyError.value = ''
  if (!key.value.startsWith('sk-')) {
    models.value = []
    return
  }
  try {
    const res = await $fetch<{ data: { id: string }[] }>('/admin/v1/playground/models', { headers: { 'X-AIF-Key': key.value } })
    models.value = res.data.map(m => m.id)
    if (!models.value.includes(model.value)) model.value = models.value[0] ?? model.value
  }
  catch (err) {
    models.value = []
    keyError.value = apiErrorMessage(err)
  }
}
let keyTimer: ReturnType<typeof setTimeout> | undefined
watch(apiKey, () => {
  clearTimeout(keyTimer)
  keyTimer = setTimeout(loadModels, 400)
})
onMounted(loadModels)

// The menu filters by what is typed, but shows every model while the box still
// holds the current choice (otherwise opening it would list only that one).
const modelSearch = ref('')
const modelItems = computed(() => {
  const q = modelSearch.value.trim().toLowerCase()
  return !q || q === model.value.toLowerCase() ? models.value : models.value.filter(m => m.toLowerCase().includes(q))
})

function addModel(name: string) {
  models.value.push(name)
  model.value = name
}

// ---- chat ----

// blocker says why a message cannot be sent yet; shown under the message box.
const blocker = computed(() => {
  if (!key.value) return 'Paste an API key (sk-aif-…) in the key field above. Create one on the API keys page.'
  if (!key.value.startsWith('sk-')) return 'The key field does not hold an API key: it must start with sk-aif-. (Did the browser fill in a saved password?)'
  if (keyError.value) return `This key cannot be used: ${keyError.value}`
  if (!model.value) return 'Pick a model, or type a model name and press Enter.'
  return ''
})
const canSend = computed(() => !busy.value && input.value.trim() !== '' && !blocker.value)

function newChat() {
  stop()
  messages.value = []
}

function stop() {
  ctrl?.abort()
}

async function send() {
  if (blocker.value && input.value.trim()) toast.add({ title: 'Cannot send yet', description: blocker.value, color: 'warning' })
  if (!canSend.value) return
  const text = input.value.trim()
  input.value = ''
  const history = messages.value.filter(m => !m.error).map(m => ({ role: m.role, content: m.content }))
  messages.value.push({ role: 'user', content: text })
  messages.value.push({ role: 'assistant', content: '', meta: { model: model.value } })
  const reply = messages.value[messages.value.length - 1]!
  const meta = reply.meta!
  scrollDown()

  const body = {
    model: model.value,
    stream: true,
    messages: [
      ...(system.value.trim() ? [{ role: 'system', content: system.value.trim() }] : []),
      ...history,
      { role: 'user', content: text },
    ],
  }
  const headers: Record<string, string> = { 'Content-Type': 'application/json', 'X-AIF-Key': key.value }
  if (endUser.value.trim()) headers['X-AIF-End-User'] = endUser.value.trim()

  busy.value = true
  ctrl = new AbortController()
  const t0 = performance.now()
  try {
    const res = await fetch('/admin/v1/playground/chat/completions', {
      method: 'POST', headers, body: JSON.stringify(body), signal: ctrl.signal,
    })
    meta.status = res.status
    meta.requestId = res.headers.get('X-Request-Id') ?? undefined
    if (!res.ok || !res.body) {
      const j = await res.json().catch(() => null) as { error?: { message?: string, code?: string } } | null
      if (res.status === 401 && j?.error?.code === 'unauthenticated') {
        await navigateTo('/login') // admin session ended
        return
      }
      reply.error = j?.error?.message ?? `HTTP ${res.status}`
      return
    }
    await readStream(res.body, reply, meta, t0)
  }
  catch (err) {
    if ((err as Error).name === 'AbortError') meta.stopped = true
    else reply.error = (err as Error).message
  }
  finally {
    meta.totalMs = performance.now() - t0
    busy.value = false
    ctrl = null
    if (meta.requestId) loadRecord(meta)
  }
}

// readStream parses OpenAI server-sent events: "data: {chunk}" lines, then "data: [DONE]".
async function readStream(stream: ReadableStream<Uint8Array>, reply: Msg, meta: Meta, t0: number) {
  const reader = stream.getReader()
  const decoder = new TextDecoder()
  let buf = ''
  for (;;) {
    const { done, value } = await reader.read()
    if (done) break
    buf += decoder.decode(value, { stream: true })
    let nl: number
    while ((nl = buf.indexOf('\n')) >= 0) {
      const line = buf.slice(0, nl).trim()
      buf = buf.slice(nl + 1)
      if (!line.startsWith('data:')) continue
      const data = line.slice(5).trim()
      if (data === '[DONE]') continue
      let chunk: { error?: { message?: string }, usage?: Usage, choices?: { delta?: { content?: string, reasoning?: string, reasoning_content?: string } }[] }
      try {
        chunk = JSON.parse(data)
      }
      catch {
        continue
      }
      if (chunk.error) reply.error = chunk.error.message ?? 'stream error'
      const delta = chunk.choices?.[0]?.delta
      const thought = delta?.reasoning_content ?? delta?.reasoning
      if (thought) reply.reasoning = (reply.reasoning ?? '') + thought
      if (delta?.content) {
        meta.ttftMs ??= performance.now() - t0
        reply.content += delta.content
        scrollDown()
      }
      if (chunk.usage) meta.usage = chunk.usage
    }
  }
}

// loadRecord fetches the metered usage record. It is written async, so retry a few times.
async function loadRecord(meta: Meta) {
  for (let i = 0; i < 6; i++) {
    await new Promise(r => setTimeout(r, 400 + i * 300))
    try {
      const res = await api<ListResponse<UsageRecord>>('/usage', { query: { id: meta.requestId } })
      if (res.data[0]) {
        meta.record = res.data[0]
        return
      }
    }
    catch {
      return
    }
  }
}

const scroller = ref<HTMLElement | null>(null)
function scrollDown() {
  nextTick(() => scroller.value?.scrollTo({ top: scroller.value.scrollHeight }))
}

function onKeydown(e: KeyboardEvent) {
  if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
    e.preventDefault()
    send()
  }
}

async function copy(text: string) {
  await navigator.clipboard.writeText(text)
  toast.add({ title: 'Copied', color: 'success' })
}

function ms(n?: number) {
  return n === undefined ? '—' : n < 1000 ? `${Math.round(n)} ms` : `${(n / 1000).toFixed(1)} s`
}
</script>

<template>
  <UDashboardPanel id="playground">
    <template #header>
      <UDashboardNavbar title="Playground">
        <template #leading>
          <UDashboardSidebarCollapse />
        </template>
        <template #right>
          <UButton icon="i-lucide-square-pen" label="New chat" color="neutral" variant="outline" :disabled="!messages.length" @click="newChat" />
        </template>
      </UDashboardNavbar>
      <UDashboardToolbar>
        <div class="flex flex-wrap items-center gap-2 py-1 w-full">
          <UInput
            v-model="apiKey"
            :type="showKey ? 'text' : 'password'"
            name="aif-playground-key"
            placeholder="sk-aif-… (API key)"
            icon="i-lucide-key-round"
            size="sm"
            class="w-64"
            autocomplete="off"
            data-1p-ignore
            data-lpignore="true"
            :color="keyError || (key && !key.startsWith('sk-')) ? 'error' : undefined"
            :highlight="!!keyError || (!!key && !key.startsWith('sk-'))"
            :ui="{ trailing: 'pe-1' }"
          >
            <template #trailing>
              <UButton
                :icon="showKey ? 'i-lucide-eye-off' : 'i-lucide-eye'"
                :aria-label="showKey ? 'Hide key' : 'Show key'"
                color="neutral"
                variant="link"
                size="xs"
                @click="showKey = !showKey"
              />
            </template>
          </UInput>
          <UInputMenu
            v-model="model"
            v-model:search-term="modelSearch"
            :items="modelItems"
            ignore-filter
            open-on-click
            open-on-focus
            create-item="always"
            placeholder="Model"
            icon="i-lucide-boxes"
            size="sm"
            class="w-72"
            @create="addModel"
          />
          <UInput v-model="endUser" placeholder="End user (optional)" icon="i-lucide-user" size="sm" class="w-48" />
          <UButton
            :icon="showSystem ? 'i-lucide-chevron-up' : 'i-lucide-chevron-down'"
            label="System prompt"
            color="neutral"
            variant="ghost"
            size="sm"
            @click="showSystem = !showSystem"
          />
        </div>
      </UDashboardToolbar>
    </template>

    <template #body>
      <div class="flex h-full flex-col gap-3">
        <UTextarea v-if="showSystem" v-model="system" :rows="2" autoresize placeholder="You are a helpful assistant." class="w-full" />

        <div ref="scroller" class="flex-1 overflow-y-auto space-y-4 pr-1">
          <div v-if="!messages.length" class="h-full flex flex-col items-center justify-center text-center text-muted gap-2 py-16">
            <UIcon name="i-lucide-messages-square" class="size-10" />
            <p class="font-medium text-default">
              Test the gateway as a client
            </p>
            <p class="text-sm max-w-md">
              Paste an API key (create one on the API keys page), pick a model, and chat. Each message goes
              through the real <code>/v1</code> path: key check, model allow-list, hooks, upstream, streaming and metering.
            </p>
            <p v-if="me?.method === 'master_key'" class="text-xs">
              Signed in with the master key.
            </p>
          </div>

          <div v-for="(m, i) in messages" :key="i" class="flex" :class="m.role === 'user' ? 'justify-end' : 'justify-start'">
            <div
              class="max-w-[80%] rounded-lg px-3 py-2 text-sm"
              :class="m.role === 'user' ? 'bg-primary/10' : 'bg-elevated border border-default'"
            >
              <details v-if="m.reasoning" class="mb-2 text-xs text-muted">
                <summary class="cursor-pointer">
                  Reasoning
                </summary>
                <p class="whitespace-pre-wrap mt-1">
                  {{ m.reasoning }}
                </p>
              </details>
              <p v-if="m.content" class="whitespace-pre-wrap break-words">
                {{ m.content }}
              </p>
              <p v-else-if="!m.error && busy && i === messages.length - 1" class="text-muted">
                <UIcon name="i-lucide-loader-circle" class="animate-spin size-4 align-middle" />
              </p>
              <UAlert v-if="m.error" color="error" variant="subtle" icon="i-lucide-circle-alert" :title="m.meta?.status ? `HTTP ${m.meta.status}` : 'Error'" :description="m.error" class="mt-1" />

              <div v-if="m.role === 'assistant' && m.meta && !(busy && i === messages.length - 1)" class="mt-2 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted">
                <span>{{ m.meta.model }}</span>
                <span v-if="m.meta.stopped">stopped</span>
                <span v-if="m.meta.usage" :title="`prompt ${m.meta.usage.prompt_tokens} / completion ${m.meta.usage.completion_tokens}`">
                  {{ fmtInt(m.meta.usage.prompt_tokens) }} → {{ fmtInt(m.meta.usage.completion_tokens) }} tokens
                </span>
                <span v-if="m.meta.ttftMs !== undefined" title="time to first token">first token {{ ms(m.meta.ttftMs) }}</span>
                <span title="total time in the browser">total {{ ms(m.meta.totalMs) }}</span>
                <template v-if="m.meta.record">
                  <span>cost {{ fmtUSD(m.meta.record.cost_usd) }}</span>
                  <UBadge v-if="m.meta.record.estimated" label="estimated" size="sm" color="warning" variant="subtle" />
                  <span>{{ m.meta.record.provider }} · {{ m.meta.record.upstream_model }}</span>
                </template>
                <UButton
                  v-if="m.meta.requestId"
                  :label="m.meta.requestId.slice(0, 12) + '…'"
                  icon="i-lucide-copy"
                  size="xs"
                  color="neutral"
                  variant="link"
                  title="Copy request ID (= usage record ID)"
                  @click="copy(m.meta.requestId)"
                />
              </div>
            </div>
          </div>
        </div>

        <form class="flex items-end gap-2" @submit.prevent="send">
          <UTextarea
            v-model="input"
            :rows="1"
            :maxrows="8"
            autoresize
            placeholder="Message (Enter to send, Shift+Enter for a new line)"
            class="flex-1"
            @keydown="onKeydown"
          />
          <UButton v-if="busy" icon="i-lucide-square" label="Stop" color="neutral" @click="stop" />
          <UButton v-else type="submit" icon="i-lucide-send" label="Send" :disabled="!input.trim()" />
        </form>
        <p v-if="blocker" class="-mt-1 flex items-center gap-1.5 text-xs text-warning">
          <UIcon name="i-lucide-info" class="size-3.5 shrink-0" />
          {{ blocker }}
        </p>
      </div>
    </template>
  </UDashboardPanel>
</template>
