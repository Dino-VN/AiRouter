import {
  ActivityIcon,
  DatabaseIcon,
  KeyRoundIcon,
  PlugZapIcon,
  RefreshCwIcon,
  TimerIcon,
  UsersIcon,
} from "lucide-react"

import { BreakdownChart } from "@/components/charts"
import { Empty } from "@/components/empty"
import { LinkButton } from "@/components/link-button"
import { PageHeader } from "@/components/page"
import { Stat, StatGrid } from "@/components/stat"
import { Alert, ErrorAlert } from "@/components/ui/alert"
import { Badge, StatusBadge } from "@/components/ui/badge"
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
import { useAsync } from "@/hooks/use-async"
import { api } from "@/lib/api"
import {
  formatCompact,
  formatDateTime,
  formatDuration,
  formatNumber,
  formatRelative,
  providerLabel,
} from "@/lib/format"
import type { UsageRow } from "@/lib/types"

export default function AdminOverviewPage() {
  const view = useAsync(async () => {
    const [overview, users] = await Promise.all([
      api.admin.overview(),
      api.admin.users(),
    ])
    // top_users comes back keyed by user id; swap in the usernames so the chart
    // reads as names rather than UUIDs.
    const names = new Map<string, string>(
      users.users.map((row) => [row.user.id, row.user.username] as const)
    )
    const topUsers: UsageRow[] = (overview.top_users ?? []).map((row) => ({
      ...row,
      key: names.get(row.key) ?? row.key,
    }))
    return { overview, topUsers }
  }, [])

  const data = view.data?.overview
  const pending = data?.pending_connections ?? []
  const topModels = data?.top_models ?? []
  const topUsers = view.data?.topUsers ?? []

  return (
    <div className="grid gap-5">
      <PageHeader
        title="Server"
        description="Everything this deployment is doing, across every account."
      >
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
        <LinkButton to="/admin/users" size="sm">
          <UsersIcon />
          Users
        </LinkButton>
      </PageHeader>

      <ErrorAlert error={view.error} />

      {!data ? (
        <div className="grid gap-3">
          <Skeleton className="h-20 w-full" />
          <Skeleton className="h-56 w-full" />
        </div>
      ) : (
        <>
          {data.database_reachable ? null : (
            <Alert variant="destructive" title="Database unreachable">
              The server answered, but its connection pool cannot ping PostgreSQL.
              Requests will fail until it recovers.
            </Alert>
          )}

          <StatGrid>
            <Stat
              label="Users"
              value={`${data.users.active} / ${data.users.total}`}
              hint={`${data.users.admins} admin${data.users.admins === 1 ? "" : "s"}`}
              icon={<UsersIcon />}
            />
            <Stat
              label="Connections"
              value={`${data.connections.usable} / ${data.connections.total}`}
              hint={`${data.connections.shared} shared with everyone`}
              icon={<PlugZapIcon />}
            />
            <Stat
              label="API keys"
              value={`${data.api_keys.active} / ${data.api_keys.total}`}
              hint="active of all time"
              icon={<KeyRoundIcon />}
            />
            <Stat
              label="Uptime"
              value={formatDuration(data.uptime_seconds)}
              hint={`since ${formatDateTime(data.started_at)}`}
              icon={<TimerIcon />}
            />
          </StatGrid>

          <div className="grid gap-4 lg:grid-cols-2">
            <Card>
              <CardHeader>
                <CardTitle>Traffic</CardTitle>
                <CardDescription>
                  Every request served by this deployment.
                </CardDescription>
                <CardAction>
                  <LinkButton to="/usage" size="xs" variant="ghost">
                    Usage
                  </LinkButton>
                </CardAction>
              </CardHeader>
              <CardContent className="px-0">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Window</TableHead>
                      <TableHead>Requests</TableHead>
                      <TableHead>Errors</TableHead>
                      <TableHead>Tokens</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {[
                      { label: "Last 24 hours", totals: data.usage.last_24h },
                      { label: "Last 30 days", totals: data.usage.last_30d },
                    ].map((row) => (
                      <TableRow key={row.label}>
                        <TableCell className="font-medium">{row.label}</TableCell>
                        <TableCell className="tabular-nums">
                          {formatNumber(row.totals.requests)}
                        </TableCell>
                        <TableCell className="tabular-nums">
                          {formatNumber(row.totals.errors)}
                        </TableCell>
                        <TableCell className="tabular-nums">
                          {formatCompact(row.totals.total_tokens)}
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </CardContent>
            </Card>

            <Card>
              <CardHeader>
                <CardTitle>Health</CardTitle>
                <CardDescription>Build, providers, and catalog.</CardDescription>
                <CardAction>
                  <Badge variant="outline">{data.version}</Badge>
                </CardAction>
              </CardHeader>
              <CardContent className="grid gap-3 text-sm">
                <div className="flex items-center justify-between gap-2">
                  <span className="flex items-center gap-2 text-muted-foreground">
                    <DatabaseIcon className="size-3.5" />
                    PostgreSQL
                  </span>
                  {data.database_reachable ? (
                    <Badge variant="success">reachable</Badge>
                  ) : (
                    <Badge variant="destructive">unreachable</Badge>
                  )}
                </div>
                <div className="flex items-center justify-between gap-2">
                  <span className="text-muted-foreground">Providers enabled</span>
                  <span className="flex flex-wrap justify-end gap-1">
                    {data.providers_enabled.map((id) => (
                      <Badge key={id} variant="outline">
                        {providerLabel(id)}
                      </Badge>
                    ))}
                  </span>
                </div>
                <div className="flex items-center justify-between gap-2">
                  <span className="text-muted-foreground">Model catalog</span>
                  <span title={formatDateTime(data.catalog_refreshed)}>
                    refreshed {formatRelative(data.catalog_refreshed)}
                  </span>
                </div>
                <div className="flex items-center justify-between gap-2">
                  <span className="text-muted-foreground">
                    Connections by provider
                  </span>
                  <span className="flex flex-wrap justify-end gap-1">
                    {Object.entries(data.connections.by_provider).map(
                      ([id, count]) => (
                        <Badge key={id} variant="muted">
                          {providerLabel(id)} {count}
                        </Badge>
                      )
                    )}
                  </span>
                </div>
                <div className="flex items-center justify-between gap-2">
                  <span className="text-muted-foreground">
                    Connections by status
                  </span>
                  <span className="flex flex-wrap justify-end gap-1">
                    {Object.entries(data.connections.by_status).map(
                      ([status, count]) => (
                        <span
                          key={status}
                          className="flex items-center gap-1 text-xs tabular-nums"
                        >
                          <StatusBadge status={status} />
                          {count}
                        </span>
                      )
                    )}
                  </span>
                </div>
              </CardContent>
            </Card>
          </div>

          <div className="grid gap-4 lg:grid-cols-2">
            <Card>
              <CardHeader>
                <CardTitle>Top models</CardTitle>
                <CardDescription>Requests over the last 30 days.</CardDescription>
              </CardHeader>
              <CardContent>
                {topModels.length > 0 ? (
                  <BreakdownChart rows={topModels} />
                ) : (
                  <Empty icon={<ActivityIcon />} title="No traffic yet" />
                )}
              </CardContent>
            </Card>

            <Card>
              <CardHeader>
                <CardTitle>Busiest accounts</CardTitle>
                <CardDescription>Requests over the last 30 days.</CardDescription>
              </CardHeader>
              <CardContent>
                {topUsers.length > 0 ? (
                  <BreakdownChart rows={topUsers} />
                ) : (
                  <Empty icon={<UsersIcon />} title="No traffic yet" />
                )}
              </CardContent>
            </Card>
          </div>

          <Card>
            <CardHeader>
              <CardTitle>
                Temporary connections{" "}
                <span className="text-muted-foreground">({pending.length})</span>
              </CardTitle>
              <CardDescription>
                Sign-ins started anywhere on this server and not finished yet.
              </CardDescription>
            </CardHeader>
            <CardContent className="px-0">
              {pending.length === 0 ? (
                <Empty
                  icon={<PlugZapIcon />}
                  title="Nothing in flight"
                  description="Started sign-ins appear here until they complete or expire."
                />
              ) : (
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Provider</TableHead>
                      <TableHead>Owner</TableHead>
                      <TableHead>Scope</TableHead>
                      <TableHead>Started</TableHead>
                      <TableHead>Expires</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {pending.map((session) => (
                      <TableRow key={session.id}>
                        <TableCell>{providerLabel(session.provider)}</TableCell>
                        <TableCell className="text-muted-foreground">
                          {session.owner_username || session.user_id}
                        </TableCell>
                        <TableCell className="text-muted-foreground">
                          {session.target_scope}
                        </TableCell>
                        <TableCell className="text-muted-foreground">
                          {formatRelative(session.created_at)}
                        </TableCell>
                        <TableCell
                          className="text-muted-foreground"
                          title={formatDateTime(session.expires_at)}
                        >
                          {formatRelative(session.expires_at)}
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              )}
            </CardContent>
          </Card>
        </>
      )}
    </div>
  )
}
