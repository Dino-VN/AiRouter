import * as React from "react"
import { GaugeIcon, PlugZapIcon, RefreshCwIcon } from "lucide-react"

import { Empty } from "@/components/empty"
import { LinkButton } from "@/components/link-button"
import { PageHeader } from "@/components/page"
import { QuotaMeter, UpstreamQuotaList } from "@/components/quota-meter"
import { Alert, ErrorAlert } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import {
  Card,
  CardAction,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"
import { Skeleton } from "@/components/ui/skeleton"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { useAction, useAsync } from "@/hooks/use-async"
import { api } from "@/lib/api"
import {
  formatDateTime,
  formatLimit,
  formatNumber,
  formatRelative,
  providerLabel,
} from "@/lib/format"

/** "unlimited" reads better than a null remaining count. */
function remainingLabel(value: number | null): string {
  return value === null ? "unlimited" : formatNumber(value)
}

export default function QuotaPage() {
  const view = useAsync(() => api.quota(), [])
  const action = useAction()
  const data = view.data
  const reload = view.reload

  const upstream = React.useMemo(
    () => data?.upstream ?? [],
    [data]
  )

  const readAllUpstream = () => {
    void action.run("upstream", async () => {
      for (const row of upstream) {
        try {
          await api.connections.fetchQuota(row.connection_id)
        } catch {
          // One unreachable account should not stop the others.
        }
      }
      reload()
    })
  }

  const quota = data?.quota
  const rows: {
    label: string
    used: number
    limit: number
    remaining: number | null
  }[] = data
    ? [
        {
          label: "Requests today",
          used: data.used.day.requests,
          limit: data.quota.requests_per_day,
          remaining: data.remaining.requests_today,
        },
        {
          label: "Tokens today",
          used: data.used.day.total_tokens,
          limit: data.quota.tokens_per_day,
          remaining: data.remaining.tokens_today,
        },
        {
          label: "Requests this month",
          used: data.used.month.requests,
          limit: data.quota.requests_per_month,
          remaining: data.remaining.requests_month,
        },
        {
          label: "Tokens this month",
          used: data.used.month.total_tokens,
          limit: data.quota.tokens_per_month,
          remaining: data.remaining.tokens_month,
        },
        {
          label: "Connections",
          used: data.used.connections,
          limit: data.quota.max_connections,
          remaining: data.remaining.connection_slots,
        },
        {
          label: "API keys",
          used: data.used.api_keys,
          limit: data.quota.max_api_keys,
          remaining: data.remaining.api_key_slots,
        },
      ]
    : []

  return (
    <div className="grid gap-5">
      <PageHeader
        title="Quota"
        description="What this account is allowed to spend here, and what the upstream providers say they have left."
      >
        <Button
          type="button"
          variant="outline"
          size="sm"
          onClick={reload}
          disabled={view.loading}
        >
          <RefreshCwIcon className={view.loading ? "animate-spin" : undefined} />
          Refresh
        </Button>
      </PageHeader>

      <ErrorAlert error={view.error ?? action.error} />

      {!data ? (
        <div className="grid gap-3">
          <Skeleton className="h-56 w-full" />
          <Skeleton className="h-40 w-full" />
        </div>
      ) : (
        <>
          <div className="grid gap-4 lg:grid-cols-2">
            <Card>
              <CardHeader>
                <CardTitle>This account</CardTitle>
                <CardDescription>
                  A limit of zero means unlimited. Day and month windows are
                  anchored to UTC, not to your local midnight.
                </CardDescription>
              </CardHeader>
              <CardContent className="grid gap-3">
                {rows.map((row) => (
                  <QuotaMeter
                    key={row.label}
                    label={row.label}
                    used={row.used}
                    limit={row.limit}
                  />
                ))}
              </CardContent>
            </Card>

            <Card>
              <CardHeader>
                <CardTitle>Windows and rules</CardTitle>
                <CardDescription>
                  Set by an administrator on your account.
                </CardDescription>
              </CardHeader>
              <CardContent className="grid gap-3 text-sm">
                <div className="grid gap-1">
                  <span className="text-muted-foreground">Day window opened</span>
                  <span className="tabular-nums">
                    {formatDateTime(data.windows.day_started_at)}{" "}
                    <span className="text-muted-foreground">
                      ({formatRelative(data.windows.day_started_at)})
                    </span>
                  </span>
                </div>
                <div className="grid gap-1">
                  <span className="text-muted-foreground">
                    Month window opened
                  </span>
                  <span className="tabular-nums">
                    {formatDateTime(data.windows.month_started_at)}{" "}
                    <span className="text-muted-foreground">
                      ({formatRelative(data.windows.month_started_at)})
                    </span>
                  </span>
                </div>
                <div className="grid gap-1">
                  <span className="text-muted-foreground">Concurrent requests</span>
                  <span>{formatLimit(quota?.concurrent_limit)}</span>
                </div>
                <div className="grid gap-1">
                  <span className="text-muted-foreground">Shared pool</span>
                  <span>
                    {quota?.allow_shared_pool ? (
                      <Badge variant="success">allowed</Badge>
                    ) : (
                      <Badge variant="muted">own connections only</Badge>
                    )}
                  </span>
                </div>
                <div className="grid gap-1">
                  <span className="text-muted-foreground">Providers</span>
                  <span className="flex flex-wrap gap-1">
                    {quota?.allowed_providers &&
                    quota.allowed_providers.length > 0 ? (
                      quota.allowed_providers.map((provider) => (
                        <Badge key={provider} variant="outline">
                          {providerLabel(provider)}
                        </Badge>
                      ))
                    ) : (
                      <Badge variant="muted">every enabled provider</Badge>
                    )}
                  </span>
                </div>
              </CardContent>
            </Card>
          </div>

          <Card>
            <CardHeader>
              <CardTitle>Counters</CardTitle>
              <CardDescription>
                The same numbers the router checks before it forwards a request.
              </CardDescription>
            </CardHeader>
            <CardContent className="px-0">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Limit</TableHead>
                    <TableHead>Used</TableHead>
                    <TableHead>Allowed</TableHead>
                    <TableHead>Remaining</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {rows.map((row) => (
                    <TableRow key={row.label}>
                      <TableCell className="font-medium">{row.label}</TableCell>
                      <TableCell className="tabular-nums">
                        {formatNumber(row.used)}
                      </TableCell>
                      <TableCell className="tabular-nums">
                        {formatLimit(row.limit)}
                      </TableCell>
                      <TableCell className="tabular-nums">
                        {remainingLabel(row.remaining)}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle>Upstream accounts</CardTitle>
              <CardDescription>
                Quota reported by the provider itself. Codex publishes rolling
                windows; Antigravity reports credits.
              </CardDescription>
              <CardAction>
                <Button
                  type="button"
                  size="sm"
                  variant="outline"
                  onClick={readAllUpstream}
                  disabled={upstream.length === 0 || action.isBusy("upstream")}
                >
                  <GaugeIcon
                    className={action.isBusy("upstream") ? "animate-pulse" : undefined}
                  />
                  Read quota
                </Button>
              </CardAction>
            </CardHeader>
            <CardContent className="grid gap-3">
              {upstream.length === 0 ? (
                <Empty
                  icon={<PlugZapIcon />}
                  title="No connections yet"
                  description="Sign in to Codex or Antigravity and their quota shows up here."
                >
                  <LinkButton to="/connections" size="sm">
                    Add a connection
                  </LinkButton>
                </Empty>
              ) : (
                <>
                  <UpstreamQuotaList rows={upstream} />
                  <Alert variant="info">
                    Some providers only report quota while they are serving
                    traffic, so a fresh connection can show nothing until it has
                    handled a request.
                  </Alert>
                </>
              )}
            </CardContent>
          </Card>
        </>
      )}
    </div>
  )
}
