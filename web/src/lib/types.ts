// Mirrors the JSON the Go API emits. Field names are the wire names, so these
// types can be compared against internal/model directly.

export type Role = "admin" | "user"
export type ProviderID = "codex" | "antigravity" | "openai"
export type UserStatus = "active" | "suspended" | "revoked"
export type ConnectionStatus = "active" | "expired" | "disabled" | "error"
export type Scope = "private" | "shared"
export type OAuthStatus =
  | "pending"
  | "completed"
  | "failed"
  | "cancelled"
  | "expired"

export type User = {
  id: string
  username: string
  display_name: string
  role: Role
  status: UserStatus
  created_at: string
  updated_at: string
  last_login_at?: string
}

export type Quota = {
  user_id: string
  requests_per_day: number
  tokens_per_day: number
  requests_per_month: number
  tokens_per_month: number
  max_connections: number
  max_api_keys: number
  allowed_providers: string[] | null
  allow_shared_pool: boolean
  concurrent_limit: number
  updated_at: string
}

export type QuotaWindow = {
  name: string
  used_percent: number
  window_minutes?: number
  resets_in_seconds?: number
  resets_at?: string
}

export type CreditBalance = {
  credit_type: string
  amount: number
  minimum: number
  tier_id?: string
  available: boolean
}

export type UpstreamQuota = {
  plan?: string
  windows?: QuotaWindow[]
  credits?: CreditBalance
  note?: string
  updated_at?: string
}

export type Connection = {
  id: string
  owner_id: string
  owner_username?: string
  provider: ProviderID
  label: string
  account_email: string
  account_id?: string
  project_id?: string
  plan?: string
  status: ConnectionStatus
  scope: Scope
  weight: number
  disabled_until?: string
  last_error?: string
  last_used_at?: string
  token_expires_at?: string
  quota?: UpstreamQuota
  quota_updated_at?: string
  metadata?: Record<string, unknown>
  created_at: string
  updated_at: string
  request_count_24h?: number
}

export type APIKey = {
  id: string
  user_id: string
  name: string
  prefix: string
  status: "active" | "revoked"
  allowed_models?: string[]
  expires_at?: string
  last_used_at?: string
  created_at: string
  request_count?: number
  /** Only present in the response that created the key. */
  secret?: string
}

export type OAuthSession = {
  id: string
  user_id: string
  owner_username?: string
  provider: ProviderID
  state: string
  redirect_uri: string
  auth_url: string
  label: string
  target_scope: Scope
  status: OAuthStatus
  error?: string
  connection_id?: string
  created_at: string
  expires_at: string
  completed_at?: string
}

export type WebSessionInfo = {
  id: string
  user_agent: string
  ip: string
  created_at: string
  expires_at: string
  current: boolean
}

export type UsageTotals = {
  requests: number
  errors: number
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
}

export type UsageBucket = UsageTotals & { bucket: string }
export type UsageRow = UsageTotals & { key: string }

export type UsageRecord = {
  id: number
  created_at: string
  user_id: string
  api_key_id?: string
  connection_id?: string
  provider: string
  model: string
  client_format: string
  status_code: number
  stream: boolean
  prompt_tokens: number
  completion_tokens: number
  reasoning_tokens: number
  cached_tokens: number
  total_tokens: number
  latency_ms: number
  error?: string
}

export type ModelInfo = {
  id: string
  object?: string
  created?: number
  owned_by?: string
  type?: string
  display_name?: string
  description?: string
  context_length?: number
  max_completion_tokens?: number
  supportedInputModalities?: string[]
  supportedOutputModalities?: string[]
  provider?: ProviderID
  plans?: string[]
}

// ---------------------------------------------------------------------------
// Response envelopes
// ---------------------------------------------------------------------------

export type Meta = {
  version: string
  providers: { id: ProviderID; display_name: string; loopback_port: number }[]
  public_url: string
  api_key_prefix: string
  local_oauth: boolean
  min_password_len: number
  /** True while the deployment has no accounts at all, which is what sends a
   * fresh install to the setup screen instead of the sign-in form. */
  needs_setup: boolean
  /** The server's own wording for the username rule, so the forms and the API
   * can never disagree about it. */
  username_rule: string
}

export type AuthResponse = {
  access_token: string
  token_type: string
  expires_at: string
  expires_in: number
  user: User
  quota?: Quota
  refresh_token?: string
}

export type MeResponse = {
  user: User
  quota: Quota
  counts: {
    connections: number
    api_keys: number
    pending_connections: number
  }
}

export type ProviderInfo = {
  id: ProviderID
  display_name: string
  loopback_port: number
  callback_path: string
  redirect_uri: string
  allowed: boolean
  connections: number
  usable: number
  models: number
  auto_callback: boolean
  /** True when the provider authenticates via an OAuth flow (Codex,
   *  Antigravity). False for API-key providers (OpenAI-compatible
   *  endpoint) — the UI uses this flag to decide between the consent
   *  flow and the API-key form. */
  oauth: boolean
}

/** Body of POST /api/connections, used to register an OpenAI-compatible
 * API-key connection. The server only honours this for providers where
 * model.Provider.IsOAuth() is false. */
export type CreateAPIKeyConnectionRequest = {
  provider: ProviderID
  label: string
  api_key: string
  base_url?: string
  plan?: string
  account_email?: string
  scope?: Scope
  weight?: number
  models?: string[]
  extra_headers?: Record<string, string>
  quota_note?: string
}

export type CreateConnectionResponse = {
  connection: Connection
}

// ---------------------------------------------------------------------------
// OpenAI-compatible endpoints (grouped by base URL)
// ---------------------------------------------------------------------------

export type OpenAIEndpointKey = {
  id: string
  label: string
  account_email: string
  plan?: string
  status: ConnectionStatus
  scope: Scope
  weight: number
  disabled_until?: string
  last_error?: string
  last_used_at?: string
  request_count_24h?: number
  has_api_key: boolean
  quota_note?: string
  extra_headers_keys?: string[]
}

export type OpenAIEndpoint = {
  base_url: string
  label: string
  models?: string[]
  keys: OpenAIEndpointKey[]
  created_at: string
  usable_count: number
  key_count: number
}

export type CreateOpenAIEndpointRequest = {
  label: string
  base_url: string
  api_key?: string
  models?: string[]
  extra_headers?: Record<string, string>
  quota_note?: string
  scope?: Scope
  weight?: number
}

export type AddOpenAIKeyRequest = {
  label: string
  api_key: string
  scope?: Scope
  weight?: number
}

export type ScanOpenAIModelsResponse = {
  base_url: string
  models: string[]
  count: number
}

export type CatalogResponse = {
  // Nullable like every other list in this file: an older server, or one whose
  // catalog has not been fetched yet, sends null rather than [].
  models: ModelInfo[] | null
  catalog: ModelInfo[] | null
  refreshed_at: string
}

export type ConnectionsResponse = {
  connections: Connection[] | null
  user_id: string
}

export type ConnectionDetail = {
  connection: Connection
  token_expired: boolean
  has_refresh: boolean
  last_refreshed?: string
}

export type OAuthSessionsResponse = {
  sessions: OAuthSession[] | null
  max_pending: number
}

export type StartOAuthResponse = {
  session: OAuthSession
  instructions: string
  expires_in: number
  auto_callback: boolean
}

export type CompleteOAuthResponse = {
  connection: Connection
  session_id: string
  status: string
}

export type KeysResponse = {
  keys: APIKey[] | null
  max_keys: number
  prefix: string
}

export type CreateKeyResponse = {
  key: APIKey
  note: string
}

export type UpstreamQuotaRow = {
  connection_id: string
  provider: ProviderID
  label: string
  account_email: string
  plan?: string
  status: ConnectionStatus
  usable: boolean
  quota?: UpstreamQuota
  quota_updated_at?: string
  requests_24h: number
}

export type QuotaResponse = {
  quota: Quota
  used: {
    day: UsageTotals
    month: UsageTotals
    connections: number
    api_keys: number
  }
  remaining: {
    requests_today: number | null
    tokens_today: number | null
    requests_month: number | null
    tokens_month: number | null
    connection_slots: number | null
    api_key_slots: number | null
  }
  windows: {
    day_started_at: string
    month_started_at: string
  }
  upstream: UpstreamQuotaRow[] | null
}

export type UsageSummary = {
  totals: {
    today: UsageTotals
    last_24h: UsageTotals
    last_7d: UsageTotals
    last_30d: UsageTotals
    month: UsageTotals
  }
  by_model: UsageRow[] | null
  by_provider: UsageRow[] | null
  scope: string
}

export type UsageSeries = {
  bucket: "hour" | "day"
  since: string
  series: UsageBucket[] | null
}

export type UsageBreakdownResponse = {
  dimension: string
  since: string
  rows: UsageRow[] | null
}

export type UsageRecordsResponse = {
  records: UsageRecord[] | null
  totals: UsageTotals
  limit: number
  offset: number
}

export type AdminOverview = {
  version: string
  started_at: string
  uptime_seconds: number
  users: { total: number; active: number; admins: number }
  connections: {
    total: number
    usable: number
    shared: number
    by_provider: Record<string, number>
    by_status: Record<string, number>
  }
  pending_connections: OAuthSession[] | null
  api_keys: { total: number; active: number }
  usage: { last_24h: UsageTotals; last_30d: UsageTotals }
  top_models: UsageRow[] | null
  top_users: UsageRow[] | null
  catalog_refreshed: string
  providers_enabled: string[]
  database_reachable: boolean
}

export type AdminUserRow = {
  user: User
  quota: Quota
  counts: { connections: number; api_keys: number }
  usage_30d: UsageTotals
}

export type AdminUserDetail = {
  user: User
  quota: Quota
  connections: Connection[] | null
  api_keys: APIKey[] | null
  pending_connections: OAuthSession[] | null
  usage: { last_24h: UsageTotals; last_30d: UsageTotals }
  web_sessions: number
}

export type CreateUserResponse = {
  user: User
  password?: string
  password_note?: string
}
