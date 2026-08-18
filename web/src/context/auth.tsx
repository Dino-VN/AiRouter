import * as React from "react"

import {
  api,
  refreshSession,
  setAccessToken,
  setSignedOutHandler,
} from "@/lib/api"
import type { AuthResponse, MeResponse, Quota, User } from "@/lib/types"

type Counts = MeResponse["counts"]

type AuthValue = {
  /** True until the boot-time refresh has settled, so guards don't flash. */
  booting: boolean
  user: User | null
  quota: Quota | null
  counts: Counts | null
  isAdmin: boolean
  login: (username: string, password: string) => Promise<void>
  logout: () => Promise<void>
  reloadMe: () => Promise<void>
  apply: (payload: AuthResponse) => void
  /** Finishes signing in from a response that already carries a session, which
   * is how the first-run setup screen gets straight into the app. */
  adopt: (payload: AuthResponse) => Promise<void>
}

const AuthContext = React.createContext<AuthValue | null>(null)

export function AuthProvider({ children }: { children: React.ReactNode }) {
  const [booting, setBooting] = React.useState(true)
  const [user, setUser] = React.useState<User | null>(null)
  const [quota, setQuota] = React.useState<Quota | null>(null)
  const [counts, setCounts] = React.useState<Counts | null>(null)
  const timer = React.useRef<number | null>(null)

  // Adopts a fresh token pair and arms the next refresh a little before the
  // access token expires, so an idle tab stays signed in.
  const apply = React.useCallback((payload: AuthResponse) => {
    setUser(payload.user)
    if (payload.quota) {
      setQuota(payload.quota)
    }
    if (timer.current !== null) {
      window.clearTimeout(timer.current)
    }
    const seconds = payload.expires_in > 0 ? payload.expires_in : 900
    timer.current = window.setTimeout(
      () => {
        void refreshSession().then((renewed) => {
          if (renewed) {
            applyRef.current(renewed)
          }
        })
      },
      Math.max(20, seconds - 90) * 1000
    )
  }, [])

  const applyRef = React.useRef(apply)
  applyRef.current = apply

  const loadMe = React.useCallback(async () => {
    const me = await api.auth.me()
    setUser(me.user)
    setQuota(me.quota)
    setCounts(me.counts)
  }, [])

  React.useEffect(() => {
    let live = true

    setSignedOutHandler(() => {
      setUser(null)
      setQuota(null)
      setCounts(null)
    })

    void (async () => {
      const payload = await refreshSession()
      if (live && payload) {
        apply(payload)
        try {
          await loadMe()
        } catch {
          // The token is good but /me failed; the page can retry on its own.
        }
      }
      if (live) {
        setBooting(false)
      }
    })()

    return () => {
      live = false
      setSignedOutHandler(null)
      if (timer.current !== null) {
        window.clearTimeout(timer.current)
      }
    }
  }, [apply, loadMe])

  const adopt = React.useCallback(
    async (payload: AuthResponse) => {
      setAccessToken(payload.access_token)
      apply(payload)
      await loadMe()
    },
    [apply, loadMe]
  )

  const login = React.useCallback(
    async (username: string, password: string) => {
      await adopt(await api.auth.login(username, password))
    },
    [adopt]
  )

  const logout = React.useCallback(async () => {
    try {
      await api.auth.logout()
    } catch {
      // Signing out locally matters more than the server round trip.
    }
    setAccessToken(null)
    if (timer.current !== null) {
      window.clearTimeout(timer.current)
    }
    setUser(null)
    setQuota(null)
    setCounts(null)
  }, [])

  const value = React.useMemo<AuthValue>(
    () => ({
      booting,
      user,
      quota,
      counts,
      isAdmin: user?.role === "admin",
      login,
      logout,
      reloadMe: loadMe,
      apply,
      adopt,
    }),
    [booting, user, quota, counts, login, logout, loadMe, apply, adopt]
  )

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>
}

export function useAuth(): AuthValue {
  const value = React.useContext(AuthContext)
  if (!value) {
    throw new Error("useAuth must be used inside <AuthProvider>")
  }
  return value
}
