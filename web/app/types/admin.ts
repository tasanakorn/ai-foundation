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
  /** Provider account that answered (spec-0004); '' before any upstream call */
  account: string
  /** Upstream calls made, failovers included */
  attempts: number
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

export type GroupBy = 'none' | 'day' | 'model' | 'provider' | 'account' | 'key' | 'tenant' | 'app' | 'end_user'

/** USD per 1M tokens. cache_write defaults to 1.25 x input when absent. */
export interface Price { input: number, output: number, cached_input: number, cache_write?: number }

/** One backend of an alias (spec-0005) */
export interface ModelTarget {
  provider: string
  upstream: string
  price?: Price
  /** round_robin weight; 1 when not set */
  weight: number
  formats: ApiFormat[]
}

export interface ModelInfo {
  id: string
  /** first target */
  provider: string
  /** first target */
  upstream: string
  /** first target's price */
  price?: Price
  /** API formats that may use this alias */
  formats: ApiFormat[]
  policy: AccountPolicy
  targets: ModelTarget[]
}

export interface ModelsResponse {
  data: ModelInfo[]
  providers: string[]
  passthrough: boolean
}

// ---- providers (/admin/v1/providers, spec-0004 section 8) ----

export type AccountPolicy = 'priority' | 'round_robin'
export type StickyMode = 'none' | 'key' | 'end_user' | 'session'

export interface AccountState {
  name: string
  weight: number
  state: 'ok' | 'cooldown'
  cooldown_until?: string
  last_error?: string
  last_error_at?: string
  /** How the account is paid for, and how much is used (task-0010); absent when not tracked */
  credit?: CreditState
}

/** monthly: a plan credit that resets each cycle; prepaid: a balance raised by top-ups; payg: billed later, usd is a budget */
export type CreditType = 'monthly' | 'prepaid' | 'payg'

/** Spend is the cost the gateway metered on all nodes. */
export interface CreditState {
  type: CreditType
  /** monthly and payg: credit or budget of each cycle */
  usd?: number
  cycle_day?: number
  /** IANA name, e.g. Asia/Bangkok */
  timezone?: string
  /** monthly and payg; cycle_end is exclusive */
  cycle_start?: string
  cycle_end?: string
  /** prepaid: sum of the ledger, and what is left */
  topups_usd?: number
  balance_usd?: number
  /** prepaid: first ledger entry; spend counts from here. Absent: no entry yet */
  since?: string
  spent_usd: number
  used_percent: number
  /** warning from 80 percent */
  level: 'ok' | 'warning' | 'exhausted'
}

/** One line of a prepaid account's ledger (/admin/v1/credit-entries). Append-only. */
export interface CreditEntry {
  id: string
  provider: string
  account: string
  /** top-up above 0; a correction may be either sign */
  usd: number
  note: string
  /** when the money arrived */
  at: string
  created_at: string
  /** admin user ID, "master_key" or "cli" */
  actor: string
}

export interface CreditEntryInput {
  provider: string
  account: string
  usd: number
  note?: string
  at?: string
}

export interface ProviderState {
  name: string
  type: 'openai' | 'openrouter' | 'anthropic'
  policy: AccountPolicy
  sticky: StickyMode
  max_attempts: number
  /** Go duration, e.g. "30s" */
  cooldown: string
  accounts: AccountState[]
}

/** Account state is shared by all nodes (spec-0004 D40); node is the node that answered. */
export interface ProvidersResponse {
  node: string
  data: ProviderState[]
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
  /** Until then, the next OIDC sign-in with this email (verified) links the account. */
  link_allowed_until?: string
  /** The local account (admin.bootstrap): the only one with a password, set in config. */
  local: boolean
}

/** GET /auth/methods (no auth): the sign-in methods that are on. */
export interface AuthMethods {
  password: boolean
  oidc: { name: string, title: string }[]
  /** Provider the sign-in page goes to without a click; empty when off (spec-0001 section 6.4). */
  auto_redirect: string
}

/** An OIDC login linked to an account (GET /auth/identities, /users/{id}/identities). */
export interface AdminIdentity {
  id: string
  user_id: string
  provider: string
  issuer: string
  subject: string
  email: string
  created_at: string
  last_login_at?: string
}

/** GET /auth/me and POST /auth/login. user is null for the master key. */
export interface Me {
  user: AdminUser | null
  role: Role
  method: 'session' | 'master_key'
}

/** POST /users: an account that signs in through OIDC (no password). */
export interface CreateUserInput {
  email: string
  name: string
  role: Role
}

/** PATCH /users/{id}: only the fields that are set change. */
export interface UpdateUserInput {
  name?: string
  role?: Role
  disabled?: boolean
}
