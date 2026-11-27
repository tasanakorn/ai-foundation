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
  /** Anthropic prompt-cache writes; part of prompt_tokens (spec-0002 D20) */
  cache_write_tokens: number
  /** API format the client used */
  format: ApiFormat
  /** Claude Code session (x-claude-code-session-id), when sent */
  session_id?: string
}

export type ApiFormat = 'openai' | 'anthropic'

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
  /** USD per 1M tokens. cache_write defaults to 1.25 x input when absent. */
  price?: { input: number, output: number, cached_input: number, cache_write?: number }
  /** API formats that may use this alias */
  formats: ApiFormat[]
}

export interface ModelsResponse {
  data: ModelInfo[]
  providers: string[]
  passthrough: boolean
}

export interface ListResponse<T> {
  data: T[]
}

// ---- admin accounts (/admin/v1/auth, /admin/v1/users) ----

export type Role = 'admin' | 'viewer'

export interface AdminUser {
  id: string
  email: string
  name: string
  role: Role
  created_at: string
  updated_at: string
  last_login_at?: string
  disabled_at?: string
}

/** GET /auth/me and POST /auth/login. user is null for the master key. */
export interface Me {
  user: AdminUser | null
  role: Role
  method: 'session' | 'master_key'
}

export interface CreateUserInput {
  email: string
  name: string
  role: Role
  password: string
}

/** PATCH /users/{id}: only the fields that are set change. */
export interface UpdateUserInput {
  name?: string
  role?: Role
  disabled?: boolean
  password?: string
}
