// The single place that talks to the Go server.
//
// The access token lives in memory only: a page reload gets a new one from the
// httpOnly refresh cookie, so nothing long-lived is ever written to storage. A
// 401 triggers exactly one refresh-and-retry before the caller is signed out.

import type {
  AdminOverview,
  AdminUserDetail,
  AdminUserRow,
  AuthResponse,
  CatalogResponse,
  CompleteOAuthResponse,
  Connection,
  ConnectionDetail,
  ConnectionsResponse,
  CreateAPIKeyConnectionRequest,
  CreateConnectionResponse,
  CreateKeyResponse,
  CreateUserResponse,
  CreateOpenAIEndpointRequest,
  AddOpenAIKeyRequest,
  KeysResponse,
  MeResponse,
  Meta,
  OAuthSessionsResponse,
  OpenAIEndpoint,
  ProviderInfo,
  Quota,
  QuotaResponse,
  ScanOpenAIModelsResponse,
  Scope,
  StartOAuthResponse,
  UpstreamQuota,
  UsageBreakdownResponse,
  UsageRecordsResponse,
  UsageSeries,
  UsageSummary,
  User,
  WebSessionInfo,
} from "./types"

export class ApiError extends Error {
  readonly status: number
  readonly code: string
  readonly fields: Record<string, string>

  constructor(
    status: number,
    code: string,
    message: string,
    fields?: Record<string, string>
  ) {
    super(message)
    this.name = "ApiError"
    this.status = status
    this.code = code
    this.fields = fields ?? {}
  }
}

/** Turns anything thrown into something safe to render. */
export function errorMessage(error: unknown): string {
  if (error instanceof ApiError) {
    const detail = Object.entries(error.fields)
      .map(([field, problem]) => `${field}: ${problem}`)
      .join(", ")
    return detail ? `${error.message} (${detail})` : error.message
  }
  if (error instanceof Error) {
    return error.message
  }
  return String(error)
}

type QueryValue = string | number | boolean | undefined | null
type Query = Record<string, QueryValue>

type RequestInit_ = {
  body?: unknown
  query?: Query
  anonymous?: boolean
  allowRetry?: boolean
}

let accessToken: string | null = null
let signedOutHandler: (() => void) | null = null
let refreshInFlight: Promise<AuthResponse | null> | null = null

export function setAccessToken(token: string | null): void {
  accessToken = token
}

export function getAccessToken(): string | null {
  return accessToken
}

/** Called when a refresh fails, i.e. the session is really gone. */
export function setSignedOutHandler(handler: (() => void) | null): void {
  signedOutHandler = handler
}

function queryString(query?: Query): string {
  if (!query) {
    return ""
  }
  const params = new URLSearchParams()
  for (const [key, value] of Object.entries(query)) {
    if (value === undefined || value === null || value === "") {
      continue
    }
    params.set(key, String(value))
  }
  const encoded = params.toString()
  return encoded ? `?${encoded}` : ""
}

/**
 * Exchanges the refresh cookie for a new access token. Concurrent callers share
 * one request so a burst of 401s does not rotate the session several times.
 */
export function refreshSession(): Promise<AuthResponse | null> {
  if (refreshInFlight) {
    return refreshInFlight
  }
  const attempt = (async (): Promise<AuthResponse | null> => {
    try {
      const response = await fetch("/api/auth/refresh", {
        method: "POST",
        credentials: "same-origin",
      })
      if (!response.ok) {
        return null
      }
      const payload = (await response.json()) as AuthResponse
      accessToken = payload.access_token
      return payload
    } catch {
      return null
    }
  })()

  refreshInFlight = attempt
  void attempt.then(
    () => {
      refreshInFlight = null
    },
    () => {
      refreshInFlight = null
    }
  )
  return attempt
}

async function request<T>(
  method: string,
  path: string,
  init: RequestInit_ = {}
): Promise<T> {
  const headers: Record<string, string> = { Accept: "application/json" }
  if (init.body !== undefined) {
    headers["Content-Type"] = "application/json"
  }
  if (!init.anonymous && accessToken) {
    headers.Authorization = `Bearer ${accessToken}`
  }

  let response: Response
  try {
    response = await fetch(path + queryString(init.query), {
      method,
      headers,
      credentials: "same-origin",
      body: init.body === undefined ? undefined : JSON.stringify(init.body),
    })
  } catch {
    throw new ApiError(0, "network", "could not reach the server")
  }

  if (
    response.status === 401 &&
    !init.anonymous &&
    init.allowRetry !== false &&
    accessToken !== null
  ) {
    const renewed = await refreshSession()
    if (renewed) {
      return request<T>(method, path, { ...init, allowRetry: false })
    }
    accessToken = null
    if (signedOutHandler) {
      signedOutHandler()
    }
  }

  const text = await response.text()
  let payload: unknown = null
  if (text.length > 0) {
    try {
      payload = JSON.parse(text)
    } catch {
      payload = null
    }
  }

  if (!response.ok) {
    const shape = payload as {
      code?: string
      message?: string
      fields?: Record<string, string>
    } | null
    throw new ApiError(
      response.status,
      shape?.code ?? "error",
      shape?.message ?? `the server returned status ${response.status}`,
      shape?.fields
    )
  }
  return payload as T
}

const get = <T>(path: string, query?: Query) =>
  request<T>("GET", path, { query })
const post = <T>(path: string, body?: unknown, query?: Query) =>
  request<T>("POST", path, { body: body ?? {}, query })
const patch = <T>(path: string, body: unknown) =>
  request<T>("PATCH", path, { body })
const put = <T>(path: string, body: unknown) => request<T>("PUT", path, { body })
const del = <T>(path: string) => request<T>("DELETE", path)

// ---------------------------------------------------------------------------
// Filters
// ---------------------------------------------------------------------------

export type UsageQuery = {
  user_id?: string
  all?: boolean
  range?: string
  since?: string
  until?: string
  provider?: string
  model?: string
}

export type ConnectionQuery = {
  all?: boolean
  usable_only?: boolean
  provider?: string
}

// ---------------------------------------------------------------------------
// Endpoints
// ---------------------------------------------------------------------------

export const api = {
  meta: () => get<Meta>("/api/meta"),

  /**
   * First-run only: creates the owner account and signs it straight in. The
   * server answers 409 "setup_complete" once any account exists.
   */
  setup: (body: {
    username: string
    display_name?: string
    password: string
  }) =>
    request<AuthResponse>("POST", "/api/setup", {
      body,
      anonymous: true,
    }),

  auth: {
    login: (username: string, password: string) =>
      request<AuthResponse>("POST", "/api/auth/login", {
        body: { username, password },
        anonymous: true,
      }),
    refresh: refreshSession,
    logout: () => post<{ status: string }>("/api/auth/logout"),
    me: () => get<MeResponse>("/api/auth/me"),
    changePassword: (current_password: string, new_password: string) =>
      post<AuthResponse>("/api/auth/password", {
        current_password,
        new_password,
      }),
    sessions: () => get<{ sessions: WebSessionInfo[] | null }>(
      "/api/auth/sessions"
    ),
    revokeSession: (id: string) =>
      del<{ status: string }>(`/api/auth/sessions/${id}`),
  },

  providers: () => get<{ providers: ProviderInfo[] }>("/api/providers"),
  models: () => get<CatalogResponse>("/api/models"),

  /** OpenAI-compatible endpoints. Each "endpoint" is a profile identified
   *  by its base URL; a profile can hold any number of API keys (each
   *  key is its own connection row grouped by base_url). The handlers
   *  here are the REST surface the /providers page drives. */
  openai: {
    list: () => get<{ endpoints: OpenAIEndpoint[] | null }>("/api/openai/endpoints"),
    create: (body: CreateOpenAIEndpointRequest) =>
      post<CreateConnectionResponse>("/api/openai/endpoints", body),
    addKey: (baseURL: string, body: AddOpenAIKeyRequest) =>
      post<CreateConnectionResponse>(
        `/api/openai/endpoints/${encodeURIComponent(baseURL)}/keys`,
        body
      ),
    scanModels: (baseURL: string, apiKey?: string) =>
      get<ScanOpenAIModelsResponse>(
        `/api/openai/endpoints/${encodeURIComponent(baseURL)}/models`,
        apiKey ? { api_key: apiKey } : undefined
      ),
  },

  connections: {
    list: (query?: ConnectionQuery) =>
      get<ConnectionsResponse>("/api/connections", query as Query),
    get: (id: string) => get<ConnectionDetail>(`/api/connections/${id}`),
    update: (
      id: string,
      body: {
        label?: string
        scope?: Scope
        weight?: number
        status?: "active" | "disabled"
      }
    ) => patch<{ connection: Connection }>(`/api/connections/${id}`, body),
    remove: (id: string) => del<{ status: string }>(`/api/connections/${id}`),
    refresh: (id: string) =>
      post<{ connection: Connection }>(`/api/connections/${id}/refresh`),
    fetchQuota: (id: string) =>
      post<{ quota: UpstreamQuota | null; note?: string }>(
        `/api/connections/${id}/quota`
      ),
    /** Create an API-key based connection (ProviderOpenAI today; any future
     *  provider where IsOAuth() returns false lands here too). OAuth-backed
     *  providers reject this endpoint and direct the caller to /api/oauth/sessions
     *  instead, so the UI always knows which entry point to use per provider. */
    create: (body: CreateAPIKeyConnectionRequest) =>
      post<CreateConnectionResponse>("/api/connections", body),
  },

  oauth: {
    list: (query?: { all?: boolean; pending?: boolean; limit?: number }) =>
      get<OAuthSessionsResponse>("/api/oauth/sessions", query as Query),
    start: (body: {
      provider: string
      label?: string
      scope?: Scope
      redirect_uri?: string
    }) => post<StartOAuthResponse>("/api/oauth/sessions", body),
    complete: (id: string, body: { code?: string; url?: string }) =>
      post<CompleteOAuthResponse>(
        `/api/oauth/sessions/${id}/complete`,
        body
      ),
    cancel: (id: string) =>
      post<{ status: string }>(`/api/oauth/sessions/${id}/cancel`),
    remove: (id: string) => del<{ status: string }>(`/api/oauth/sessions/${id}`),
  },

  keys: {
    list: (query?: { all?: boolean }) =>
      get<KeysResponse>("/api/keys", query as Query),
    create: (body: {
      name: string
      allowed_models?: string[]
      expires_in_days?: number
      expires_at?: string
    }) => post<CreateKeyResponse>("/api/keys", body),
    revoke: (id: string) => post<{ status: string }>(`/api/keys/${id}/revoke`),
    remove: (id: string) => del<{ status: string }>(`/api/keys/${id}`),
  },

  quota: (userID?: string) =>
    get<QuotaResponse>("/api/quota", { user_id: userID }),

  usage: {
    summary: (query?: UsageQuery) =>
      get<UsageSummary>("/api/usage/summary", query as Query),
    series: (query?: UsageQuery & { bucket?: "hour" | "day" }) =>
      get<UsageSeries>("/api/usage/series", query as Query),
    breakdown: (
      query?: UsageQuery & { dimension?: "model" | "provider" | "user" }
    ) => get<UsageBreakdownResponse>("/api/usage/breakdown", query as Query),
    records: (query?: UsageQuery & { limit?: number; offset?: number }) =>
      get<UsageRecordsResponse>("/api/usage/records", query as Query),
  },

  admin: {
    overview: () => get<AdminOverview>("/api/admin/overview"),
    users: () => get<{ users: AdminUserRow[] }>("/api/admin/users"),
    createUser: (body: {
      username: string
      display_name?: string
      password?: string
      role?: string
      status?: string
      quota?: Quota | null
    }) => post<CreateUserResponse>("/api/admin/users", body),
    user: (id: string) => get<AdminUserDetail>(`/api/admin/users/${id}`),
    updateUser: (
      id: string,
      body: { display_name?: string; role?: string; status?: string }
    ) => patch<{ user: User }>(`/api/admin/users/${id}`, body),
    deleteUser: (id: string) =>
      del<{ status: string }>(`/api/admin/users/${id}`),
    setQuota: (id: string, quota: Quota) =>
      put<{ quota: Quota }>(`/api/admin/users/${id}/quota`, quota),
    setPassword: (id: string, password: string) =>
      post<{ status: string; password?: string }>(
        `/api/admin/users/${id}/password`,
        { password }
      ),
  },
}
