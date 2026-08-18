import { Badge } from "@/components/ui/badge"
import { Progress } from "@/components/ui/progress"
import {
  formatDuration,
  formatLimit,
  formatNumber,
  formatPercent,
  formatRelative,
  providerLabel,
  usedFraction,
} from "@/lib/format"
import type { UpstreamQuota, UpstreamQuotaRow } from "@/lib/types"
import { cn } from "@/lib/utils"

/** One local quota line: used against a limit, where 0 means unlimited. */
function QuotaMeter({
  label,
  used,
  limit,
  suffix,
  className,
}: {
  label: string
  used: number
  limit: number
  suffix?: string
  className?: string
}) {
  const fraction = usedFraction(used, limit)
  return (
    <div className={cn("grid gap-1.5", className)}>
      <div className="flex items-baseline justify-between gap-2 text-sm">
        <span className="text-muted-foreground">{label}</span>
        <span className="tabular-nums">
          {formatNumber(used)}
          <span className="text-muted-foreground">
            {" / "}
            {formatLimit(limit)}
            {suffix ? ` ${suffix}` : ""}
          </span>
        </span>
      </div>
      <Progress value={fraction} />
    </div>
  )
}

/** The upstream account's own limits, as reported by the provider. */
function UpstreamWindows({ quota }: { quota: UpstreamQuota }) {
  const windows = quota.windows ?? []
  return (
    <div className="grid gap-2.5">
      {windows.map((window) => (
        <div key={window.name} className="grid gap-1">
          <div className="flex items-baseline justify-between gap-2 text-xs">
            <span className="font-medium">{window.name}</span>
            <span className="text-muted-foreground tabular-nums">
              {formatPercent(window.used_percent)} used
              {window.resets_in_seconds
                ? ` · resets in ${formatDuration(window.resets_in_seconds)}`
                : ""}
            </span>
          </div>
          <Progress value={Math.min(1, window.used_percent / 100)} />
        </div>
      ))}
      {quota.credits ? (
        <div className="flex items-center justify-between gap-2 text-xs">
          <span className="font-medium">
            {quota.credits.credit_type || "credits"}
          </span>
          <span className="text-muted-foreground tabular-nums">
            {formatNumber(quota.credits.amount)}
            {quota.credits.available ? "" : " · unavailable"}
          </span>
        </div>
      ) : null}
      {quota.note ? (
        <p className="text-xs text-muted-foreground">{quota.note}</p>
      ) : null}
    </div>
  )
}

/** Table-free list of every connection's upstream quota, for the quota page. */
function UpstreamQuotaList({ rows }: { rows: UpstreamQuotaRow[] }) {
  return (
    <div className="grid gap-3">
      {rows.map((row) => (
        <div
          key={row.connection_id}
          className="grid gap-2 rounded-lg border border-border p-3"
        >
          <div className="flex flex-wrap items-center justify-between gap-2">
            <div className="flex min-w-0 items-center gap-2">
              <span className="truncate text-sm font-medium">
                {row.label || row.account_email}
              </span>
              <Badge variant="outline">{providerLabel(row.provider)}</Badge>
              {row.plan ? <Badge variant="muted">{row.plan}</Badge> : null}
            </div>
            <span className="text-xs text-muted-foreground tabular-nums">
              {formatNumber(row.requests_24h)} req / 24h
            </span>
          </div>
          {row.quota && (row.quota.windows?.length || row.quota.credits) ? (
            <UpstreamWindows quota={row.quota} />
          ) : (
            <p className="text-xs text-muted-foreground">
              No upstream quota reported yet.
            </p>
          )}
          <p className="text-xs text-muted-foreground">
            {row.usable ? "Usable" : "Not usable"} · updated{" "}
            {formatRelative(row.quota_updated_at)}
          </p>
        </div>
      ))}
    </div>
  )
}

export { QuotaMeter, UpstreamQuotaList, UpstreamWindows }
