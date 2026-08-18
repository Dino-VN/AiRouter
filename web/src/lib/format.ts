// Presentation helpers. Everything here tolerates the shapes the Go API can
// produce, including the zero time (0001-01-01) that stands in for "never".

import type { Connection } from "./types"

const ZERO_TIME_PREFIX = "0001-01-01"

const integer = new Intl.NumberFormat(undefined, { maximumFractionDigits: 0 })
const compact = new Intl.NumberFormat(undefined, {
  notation: "compact",
  maximumFractionDigits: 1,
})
const relative = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" })

export function hasTime(value: string | null | undefined): value is string {
  return !!value && !value.startsWith(ZERO_TIME_PREFIX)
}

export function parseTime(value: string | null | undefined): Date | null {
  if (!hasTime(value)) {
    return null
  }
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? null : date
}

export function formatNumber(value: number | null | undefined): string {
  if (value === null || value === undefined) {
    return "—"
  }
  return integer.format(value)
}

/** Short form for cards and axes: 12.3K, 4.1M. */
export function formatCompact(value: number | null | undefined): string {
  if (value === null || value === undefined) {
    return "—"
  }
  if (Math.abs(value) < 1000) {
    return integer.format(value)
  }
  return compact.format(value)
}

/** A limit of 0 means unlimited everywhere in this product. */
export function formatLimit(limit: number | null | undefined): string {
  if (limit === null || limit === undefined || limit === 0) {
    return "unlimited"
  }
  return integer.format(limit)
}

export function formatPercent(value: number, digits = 0): string {
  return `${value.toFixed(digits)}%`
}

export function formatDateTime(value: string | null | undefined): string {
  const date = parseTime(value)
  if (!date) {
    return "—"
  }
  return date.toLocaleString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  })
}

export function formatDate(value: string | null | undefined): string {
  const date = parseTime(value)
  if (!date) {
    return "—"
  }
  return date.toLocaleDateString(undefined, {
    year: "numeric",
    month: "short",
    day: "numeric",
  })
}

export function formatClock(value: string | null | undefined): string {
  const date = parseTime(value)
  if (!date) {
    return "—"
  }
  return date.toLocaleTimeString(undefined, {
    hour: "2-digit",
    minute: "2-digit",
  })
}

const UNITS: [Intl.RelativeTimeFormatUnit, number][] = [
  ["year", 365 * 24 * 3600],
  ["month", 30 * 24 * 3600],
  ["day", 24 * 3600],
  ["hour", 3600],
  ["minute", 60],
  ["second", 1],
]

/** "5 minutes ago", "in 3 days", or "—" when there is no timestamp. */
export function formatRelative(value: string | null | undefined): string {
  const date = parseTime(value)
  if (!date) {
    return "—"
  }
  const seconds = (date.getTime() - Date.now()) / 1000
  for (const [unit, size] of UNITS) {
    if (Math.abs(seconds) >= size || unit === "second") {
      return relative.format(Math.round(seconds / size), unit)
    }
  }
  return "just now"
}

/** Compact duration for uptimes and countdowns: 3d 4h, 12m 30s. */
export function formatDuration(seconds: number): string {
  const total = Math.max(0, Math.floor(seconds))
  const days = Math.floor(total / 86400)
  const hours = Math.floor((total % 86400) / 3600)
  const minutes = Math.floor((total % 3600) / 60)
  const rest = total % 60

  if (days > 0) {
    return `${days}d ${hours}h`
  }
  if (hours > 0) {
    return `${hours}h ${minutes}m`
  }
  if (minutes > 0) {
    return `${minutes}m ${rest}s`
  }
  return `${rest}s`
}

export function formatLatency(ms: number | null | undefined): string {
  if (ms === null || ms === undefined) {
    return "—"
  }
  if (ms < 1000) {
    return `${Math.round(ms)} ms`
  }
  return `${(ms / 1000).toFixed(ms < 10000 ? 2 : 1)} s`
}

/** Fraction used, clamped, for progress bars. A limit of 0 has no fraction. */
export function usedFraction(used: number, limit: number): number | null {
  if (!limit || limit <= 0) {
    return null
  }
  return Math.min(1, Math.max(0, used / limit))
}

/**
 * Mirrors Connection.Usable in Go: active, and past any cooldown the router set
 * after a failure.
 */
export function connectionUsable(conn: Connection): boolean {
  if (conn.status !== "active") {
    return false
  }
  const until = parseTime(conn.disabled_until)
  return !until || until.getTime() <= Date.now()
}

export function providerLabel(id: string): string {
  switch (id) {
    case "codex":
      return "Codex"
    case "antigravity":
      return "Antigravity"
    default:
      return id
  }
}

export function titleCase(value: string): string {
  if (!value) {
    return ""
  }
  return value.charAt(0).toUpperCase() + value.slice(1)
}

/** Trims a user agent down to something that fits in a table cell. */
export function shortUserAgent(agent: string): string {
  if (!agent) {
    return "unknown client"
  }
  const browser = /(Firefox|Edg|Chrome|Safari)\/[\d.]+/.exec(agent)
  const platform = /\(([^)]+)\)/.exec(agent)
  const name = browser ? browser[0].replace("Edg", "Edge") : agent.slice(0, 24)
  const where = platform ? platform[1].split(";")[0] : ""
  return where ? `${name} · ${where}` : name
}
