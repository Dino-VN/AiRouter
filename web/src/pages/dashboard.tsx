import {
  ActivityIcon,
  CoinsIcon,
  KeyRoundIcon,
  PlugZapIcon,
  RefreshCwIcon,
  TimerIcon,
} from "lucide-react"

import { BreakdownChart, RequestsChart } from "@/components/charts"
import { Empty } from "@/components/empty"
import { LinkButton } from "@/components/link-button"
import { PageHeader } from "@/components/page"
import { QuotaMeter } from "@/components/quota-meter"
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
import { Skeleton } from "@/components/ui/skeleton"
import { useAuth } from "@/context/auth"
import { useAsync } from "@/hooks/use-async"
import { api } from "@/lib/api"
import {
  connectionUsable,
  formatCompact,
  formatLimit,
  formatNumber,
  formatRelative,
  providerLabel,
} from "@/lib/format"

export default function DashboardPage() {
  const { user } = useAuth()

  const view = useAsync(async () => {
    const [quota, summary, series, connections, pending] = await Promise.all([
      api.quota(),
      api.usage.summary(),
      api.usage.series({ range: "24h", bucket: "hour" }),
      api.connections.list(),
      api.oauth.list({ pending: true }),
    ])
    return { quota, summary, series, connections, pending }
  }, [])

  const data = view.data
  const conns = data?.connections.connections ?? []
  const usable = conns.filter(connectionUsable).length
  const pending = data?.pending.sessions ?? []
  const today = data?.summary.totals.today
  const day = data?.quota.used.day
  const quota = data?.quota.quota

  return (
    <div className="grid gap-5">
      <PageHeader
        title={`Welcome back, ${user?.display_name || user?.username || ""}`}
        description="Your upstream accounts, what they have served today, and what is left of your quota."
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
        <LinkButton to="/connections" size="sm">
          <PlugZapIcon />
          Connections
        </LinkButton>
      </PageHeader>

      <ErrorAlert error={view.error} />

      {!data ? (
        <div className="grid gap-3">
          <Skeleton className="h-20 w-full" />
          <Skeleton className="h-64 w-full" />
        </div>
      ) : (
        <>
          {pending.length > 0 ? (
            <Card className="border-amber-500/30 bg-amber-500/5">
              <CardHeader>
                <CardTitle>
                  {pending.length} sign-in
                  {pending.length === 1 ? "" : "s"} waiting to finish
                </CardTitle>
                <CardDescription>
                  A temporary connection stays here until you paste the
                  authorization code, or until it expires.
                </CardDescription>
                <CardAction>
                  <LinkButton to="/connections" size="sm" variant="outline">
                    Finish
                  </LinkButton>
                </CardAction>
              </CardHeader>
              <CardContent className="flex flex-wrap gap-2">
                {pending.map((session) => (
                  <Badge key={session.id} variant="warning">
                    <TimerIcon />
                    {providerLabel(session.provider)} · expires{" "}
                    {formatRelative(session.expires_at)}
                  </Badge>
                ))}
              </CardContent>
            </Card>
          ) : null}

          <StatGrid>
            <Stat
              label="Requests today"
              value={formatNumber(today?.requests ?? 0)}
              hint={`of ${formatLimit(quota?.requests_per_day)} allowed`}
              icon={<ActivityIcon />}
            />
            <Stat
              label="Tokens today"
              value={formatCompact(today?.total_tokens ?? 0)}
              hint={`of ${formatLimit(quota?.tokens_per_day)} allowed`}
              icon={<CoinsIcon />}
            />
            <Stat
              label="Connections"
              value={`${usable} / ${conns.length}`}
              hint="usable right now"
              icon={<PlugZapIcon />}
            />
            <Stat
              label="API keys"
              value={formatNumber(data.quota.used.api_keys)}
              hint={`of ${formatLimit(quota?.max_api_keys)} allowed`}
              icon={<KeyRoundIcon />}
            />
          </StatGrid>

          <Card>
            <CardHeader>
              <CardTitle>Requests, last 24 hours</CardTitle>
              <CardDescription>
                Hourly, across every model and provider you can reach.
              </CardDescription>
            </CardHeader>
            <CardContent>
              {data.series.series && data.series.series.length > 0 ? (
                <RequestsChart series={data.series.series} bucket="hour" />
              ) : (
                <Empty
                  icon={<ActivityIcon />}
                  title="No requests yet"
                  description="Point a client at this server with one of your API keys and traffic will show up here."
                />
              )}
            </CardContent>
          </Card>

          <div className="grid gap-4 lg:grid-cols-2">
            <Card>
              <CardHeader>
                <CardTitle>Quota</CardTitle>
                <CardDescription>
                  Windows are anchored to UTC. A limit of zero means unlimited.
                </CardDescription>
                <CardAction>
                  <LinkButton to="/quota" size="xs" variant="ghost">
                    Details
                  </LinkButton>
                </CardAction>
              </CardHeader>
              <CardContent className="grid gap-3">
                <QuotaMeter
                  label="Requests today"
                  used={day?.requests ?? 0}
                  limit={quota?.requests_per_day ?? 0}
                />
                <QuotaMeter
                  label="Tokens today"
                  used={day?.total_tokens ?? 0}
                  limit={quota?.tokens_per_day ?? 0}
                />
                <QuotaMeter
                  label="Requests this month"
                  used={data.quota.used.month.requests}
                  limit={quota?.requests_per_month ?? 0}
                />
                <QuotaMeter
                  label="Tokens this month"
                  used={data.quota.used.month.total_tokens}
                  limit={quota?.tokens_per_month ?? 0}
                />
              </CardContent>
            </Card>

            <Card>
              <CardHeader>
                <CardTitle>Top models, last 30 days</CardTitle>
                <CardDescription>Requests per model.</CardDescription>
                <CardAction>
                  <LinkButton to="/usage" size="xs" variant="ghost">
                    Usage
                  </LinkButton>
                </CardAction>
              </CardHeader>
              <CardContent>
                {data.summary.by_model && data.summary.by_model.length > 0 ? (
                  <BreakdownChart rows={data.summary.by_model} />
                ) : (
                  <Empty
                    title="Nothing to break down yet"
                    description="Model usage appears once requests have been served."
                  />
                )}
              </CardContent>
            </Card>
          </div>
        </>
      )}
    </div>
  )
}
