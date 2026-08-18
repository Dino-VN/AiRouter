import * as React from "react"
import {
  ActivityIcon,
  CoinsIcon,
  RefreshCwIcon,
  TimerIcon,
  TriangleAlertIcon,
} from "lucide-react"

import { BreakdownChart, RequestsChart, TokensChart } from "@/components/charts"
import { Empty } from "@/components/empty"
import { PageHeader } from "@/components/page"
import { Stat, StatGrid } from "@/components/stat"
import { ErrorAlert } from "@/components/ui/alert"
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
import { Label, Switch } from "@/components/ui/field"
import { SkeletonRows } from "@/components/ui/skeleton"
import { Tabs } from "@/components/ui/tabs"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { useAuth } from "@/context/auth"
import { useAsync } from "@/hooks/use-async"
import { api } from "@/lib/api"
import type { UsageQuery } from "@/lib/api"
import {
  formatClock,
  formatCompact,
  formatDateTime,
  formatLatency,
  formatNumber,
  formatPercent,
  providerLabel,
} from "@/lib/format"
import type { UsageTotals } from "@/lib/types"

type Range = "24h" | "7d" | "30d"
type Dimension = "model" | "provider" | "user"

const EMPTY: UsageTotals = {
  requests: 0,
  errors: 0,
  prompt_tokens: 0,
  completion_tokens: 0,
  total_tokens: 0,
}

export default function UsagePage() {
  const { isAdmin } = useAuth()
  const [range, setRange] = React.useState<Range>("24h")
  const [dimension, setDimension] = React.useState<Dimension>("model")
  const [everyone, setEveryone] = React.useState(false)

  const bucket: "hour" | "day" = range === "24h" ? "hour" : "day"

  const view = useAsync(async () => {
    const scope: UsageQuery = everyone ? { all: true } : {}
    const [summary, series, breakdown, records] = await Promise.all([
      api.usage.summary(scope),
      api.usage.series({ ...scope, range, bucket }),
      api.usage.breakdown({ ...scope, range, dimension }),
      api.usage.records({ ...scope, range, limit: 50 }),
    ])
    return { summary, series, breakdown, records }
  }, [range, dimension, everyone])

  const totals =
    view.data?.summary.totals[
      range === "24h" ? "last_24h" : range === "7d" ? "last_7d" : "last_30d"
    ] ?? EMPTY
  const errorRate =
    totals.requests > 0 ? (totals.errors / totals.requests) * 100 : 0
  const records = view.data?.records.records ?? []
  const rows = view.data?.breakdown.rows ?? []
  const series = view.data?.series.series ?? []

  return (
    <div className="grid gap-5">
      <PageHeader
        title="Usage"
        description="Every proxied request is logged with its token counts, so these numbers come from the same rows quota is enforced against."
      >
        {isAdmin ? (
          <Label className="mr-1 text-xs text-muted-foreground">
            <Switch
              checked={everyone}
              onChange={(event) => setEveryone(event.target.checked)}
            />
            Everyone
          </Label>
        ) : null}
        <Tabs
          value={range}
          onValueChange={setRange}
          items={[
            { value: "24h", label: "24h" },
            { value: "7d", label: "7 days" },
            { value: "30d", label: "30 days" },
          ]}
        />
        <Button
          type="button"
          variant="outline"
          size="sm"
          onClick={view.reload}
          disabled={view.loading}
        >
          <RefreshCwIcon className={view.loading ? "animate-spin" : undefined} />
          Refresh
        </Button>
      </PageHeader>

      <ErrorAlert error={view.error} />

      <StatGrid>
        <Stat
          label="Requests"
          value={formatNumber(totals.requests)}
          hint={`${formatNumber(totals.errors)} failed`}
          icon={<ActivityIcon />}
        />
        <Stat
          label="Error rate"
          value={formatPercent(errorRate, errorRate > 0 && errorRate < 1 ? 2 : 0)}
          hint="share of requests that did not return 2xx"
          icon={<TriangleAlertIcon />}
        />
        <Stat
          label="Tokens"
          value={formatCompact(totals.total_tokens)}
          hint={`${formatCompact(totals.prompt_tokens)} in · ${formatCompact(totals.completion_tokens)} out`}
          icon={<CoinsIcon />}
        />
        <Stat
          label="Scope"
          value={view.data?.summary.scope === "deployment" ? "Everyone" : "You"}
          hint={`bucketed by ${bucket}`}
          icon={<TimerIcon />}
        />
      </StatGrid>

      <Card>
        <CardHeader>
          <CardTitle>Requests</CardTitle>
          <CardDescription>
            Successes and failures per {bucket}, over the selected window.
          </CardDescription>
        </CardHeader>
        <CardContent>
          {series.length > 0 ? (
            <RequestsChart series={series} bucket={bucket} />
          ) : (
            <Empty title="Nothing in this window" />
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Tokens</CardTitle>
          <CardDescription>Prompt and completion tokens, stacked.</CardDescription>
        </CardHeader>
        <CardContent>
          {series.length > 0 ? (
            <TokensChart series={series} bucket={bucket} />
          ) : (
            <Empty title="Nothing in this window" />
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Breakdown</CardTitle>
          <CardDescription>Requests grouped by {dimension}.</CardDescription>
          <CardAction>
            <Tabs
              value={dimension}
              onValueChange={setDimension}
              items={
                isAdmin
                  ? [
                      { value: "model", label: "Model" },
                      { value: "provider", label: "Provider" },
                      { value: "user", label: "User" },
                    ]
                  : [
                      { value: "model", label: "Model" },
                      { value: "provider", label: "Provider" },
                    ]
              }
            />
          </CardAction>
        </CardHeader>
        <CardContent className="grid gap-4">
          {rows.length === 0 ? (
            <Empty title="No usage to group yet" />
          ) : (
            <>
              <BreakdownChart rows={rows.slice(0, 8)} />
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>{dimension}</TableHead>
                    <TableHead>Requests</TableHead>
                    <TableHead>Errors</TableHead>
                    <TableHead>Prompt</TableHead>
                    <TableHead>Completion</TableHead>
                    <TableHead>Total tokens</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {rows.map((row) => (
                    <TableRow key={row.key}>
                      <TableCell className="font-mono text-xs">
                        {row.key || "—"}
                      </TableCell>
                      <TableCell className="tabular-nums">
                        {formatNumber(row.requests)}
                      </TableCell>
                      <TableCell className="tabular-nums">
                        {formatNumber(row.errors)}
                      </TableCell>
                      <TableCell className="tabular-nums">
                        {formatCompact(row.prompt_tokens)}
                      </TableCell>
                      <TableCell className="tabular-nums">
                        {formatCompact(row.completion_tokens)}
                      </TableCell>
                      <TableCell className="tabular-nums">
                        {formatCompact(row.total_tokens)}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Recent requests</CardTitle>
          <CardDescription>
            The last {records.length} of{" "}
            {formatNumber(view.data?.records.totals.requests ?? 0)} in this window.
          </CardDescription>
        </CardHeader>
        <CardContent className="px-0">
          {view.loading && records.length === 0 ? (
            <SkeletonRows rows={4} cols={6} />
          ) : records.length === 0 ? (
            <Empty title="No requests logged in this window" />
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>When</TableHead>
                  <TableHead>Model</TableHead>
                  <TableHead>Provider</TableHead>
                  <TableHead>Format</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Tokens</TableHead>
                  <TableHead>Latency</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {records.map((record) => (
                  <TableRow key={record.id}>
                    <TableCell
                      className="whitespace-nowrap text-muted-foreground"
                      title={formatDateTime(record.created_at)}
                    >
                      {formatClock(record.created_at)}
                    </TableCell>
                    <TableCell className="font-mono text-xs">
                      {record.model}
                    </TableCell>
                    <TableCell>{providerLabel(record.provider)}</TableCell>
                    <TableCell className="text-xs text-muted-foreground">
                      {record.client_format}
                      {record.stream ? " · stream" : ""}
                    </TableCell>
                    <TableCell>
                      <Badge
                        variant={
                          record.status_code >= 500
                            ? "destructive"
                            : record.status_code >= 400
                              ? "warning"
                              : "success"
                        }
                      >
                        {record.status_code}
                      </Badge>
                      {record.error ? (
                        <p
                          className="mt-0.5 max-w-56 truncate text-xs text-destructive"
                          title={record.error}
                        >
                          {record.error}
                        </p>
                      ) : null}
                    </TableCell>
                    <TableCell className="tabular-nums">
                      {formatCompact(record.total_tokens)}
                    </TableCell>
                    <TableCell className="tabular-nums">
                      {formatLatency(record.latency_ms)}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>
    </div>
  )
}
