// Two small hooks that every page uses: one for loading data, one for running a
// mutation. Deliberately tiny — no cache, no query client, just enough state to
// render a spinner, an error, and a retry.

import { useCallback, useEffect, useRef, useState } from "react"

import { errorMessage } from "@/lib/api"

export type Async<T> = {
  data: T | null
  error: string | null
  loading: boolean
  /** Re-runs the loader, keeping whatever is already on screen. */
  reload: () => void
  setData: (value: T | null) => void
}

export function useAsync<T>(load: () => Promise<T>, deps: unknown[]): Async<T> {
  const [data, setData] = useState<T | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const [nonce, setNonce] = useState(0)

  // The loader closes over render-scoped values, so keep the latest one without
  // making it a dependency of the effect.
  const loader = useRef(load)
  loader.current = load

  useEffect(() => {
    let live = true
    setLoading(true)
    loader.current().then(
      (value) => {
        if (live) {
          setData(value)
          setError(null)
          setLoading(false)
        }
      },
      (err: unknown) => {
        if (live) {
          setError(errorMessage(err))
          setLoading(false)
        }
      }
    )
    return () => {
      live = false
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, nonce])

  const reload = useCallback(() => {
    setNonce((value) => value + 1)
  }, [])

  return { data, error, loading, reload, setData }
}

export type Action = {
  /** Key of the in-flight call, so a table can disable just one row's button. */
  busy: string | null
  isBusy: (key: string) => boolean
  error: string | null
  setError: (message: string | null) => void
  run: <T>(key: string, fn: () => Promise<T>) => Promise<T | null>
}

export function useAction(): Action {
  const [busy, setBusy] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)

  const run = useCallback(
    async <T>(key: string, fn: () => Promise<T>): Promise<T | null> => {
      setBusy(key)
      setError(null)
      try {
        return await fn()
      } catch (err) {
        setError(errorMessage(err))
        return null
      } finally {
        setBusy(null)
      }
    },
    []
  )

  return {
    busy,
    isBusy: (key: string) => busy === key,
    error,
    setError,
    run,
  }
}

/** Polls while `enabled`, firing immediately and then every `ms`. */
export function useInterval(
  callback: () => void,
  ms: number,
  enabled = true
): void {
  const latest = useRef(callback)
  latest.current = callback

  useEffect(() => {
    if (!enabled) {
      return
    }
    const id = window.setInterval(() => latest.current(), ms)
    return () => window.clearInterval(id)
  }, [ms, enabled])
}
