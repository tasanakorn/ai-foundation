// Shapes of the gateway admin API (/admin/v1). Keep in sync with gateway/internal/store.

export interface ApiKey {
  id: string
  name: string
  prefix: string
  tenant_id: string
  app_id: string
  allowed_models: string[]
  metadata: Record<string, string>
  created_at: string
  expires_at?: string
  revoked_at?: string
}

export interface CreatedKey extends ApiKey {
  secret: string
}

export interface CreateKeyInput {
  name: string
  tenant_id: string
  app_id: string
  allowed_models: string[]
  metadata: Record<string, string>
  expires_at?: string
}

export interface UsageRecord {
  id: string
  created_at: string
  key_id: string
  tenant_id: string
  app_id: string
  end_user: string
  endpoint: string
  model: string
  provider: string
  upstream_model: string
  stream: boolean
  status_code: number
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  cached_tokens: number
  reasoning_tokens: number
  cost_usd: number
  estimated: boolean
  latency_ms: number
  error?: string
}

export interface UsageSummary {
  group: string
  requests: number
  errors: number
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  cost_usd: number
}

export type GroupBy = 'none' | 'day' | 'model' | 'provider' | 'key' | 'tenant' | 'app' | 'end_user'

export interface ModelInfo {
  id: string
  provider: string
  upstream: string
  /** USD per 1M tokens */
  price?: { input: number, output: number, cached_input: number }
}

export interface ModelsResponse {
  data: ModelInfo[]
  providers: string[]
  passthrough: boolean
}

export interface ListResponse<T> {
  data: T[]
}
