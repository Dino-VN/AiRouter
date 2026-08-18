import {
  Area,
  AreaChart,
  Bar,
  BarChart,
  CartesianGrid,
  Cell,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts"

import { formatClock, formatCompact, formatDate } from "@/lib/format"
import type { UsageBucket, UsageRow } from "@/lib/types"

const tooltipStyle = {
  background: "var(--popover)",
  border: "1px solid var(--border)",
  borderRadius: "0.5rem",
  color: "var(--popover-foreground)",
  fontSize: "0.75rem",
  padding: "0.375rem 0.5rem",
}

const axis = {
  stroke: "var(--muted-foreground)",
  fontSize: 11,
  tickLine: false,
  axisLine: false,
}

function label(value: string, bucket: "hour" | "day"): string {
  return bucket === "hour" ? formatClock(value) : formatDate(value)
}

/** Requests per bucket, with errors stacked underneath in a warning colour. */
function RequestsChart({
  series,
  bucket,
  height = 220,
}: {
  series: UsageBucket[]
  bucket: "hour" | "day"
  height?: number
}) {
  return (
    <ResponsiveContainer width="100%" height={height}>
      <AreaChart data={series} margin={{ top: 6, right: 6, bottom: 0, left: 0 }}>
        <defs>
          <linearGradient id="fill-requests" x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stopColor="var(--color-chart-1)" stopOpacity={0.5} />
            <stop offset="100%" stopColor="var(--color-chart-1)" stopOpacity={0.03} />
          </linearGradient>
        </defs>
        <CartesianGrid vertical={false} stroke="var(--border)" strokeDasharray="3 3" />
        <XAxis
          dataKey="bucket"
          tickFormatter={(value) => label(String(value), bucket)}
          minTickGap={24}
          {...axis}
        />
        <YAxis
          width={44}
          tickFormatter={(value) => formatCompact(Number(value))}
          {...axis}
        />
        <Tooltip
          contentStyle={tooltipStyle}
          labelFormatter={(value) => label(String(value), bucket)}
          formatter={(value, name) => [formatCompact(Number(value)), String(name)]}
        />
        <Area
          type="monotone"
          dataKey="requests"
          name="requests"
          stroke="var(--color-chart-1)"
          strokeWidth={2}
          fill="url(#fill-requests)"
        />
        <Area
          type="monotone"
          dataKey="errors"
          name="errors"
          stroke="var(--color-chart-5)"
          strokeWidth={1.5}
          fillOpacity={0.12}
          fill="var(--color-chart-5)"
        />
      </AreaChart>
    </ResponsiveContainer>
  )
}

/** Prompt and completion tokens stacked per bucket. */
function TokensChart({
  series,
  bucket,
  height = 220,
}: {
  series: UsageBucket[]
  bucket: "hour" | "day"
  height?: number
}) {
  return (
    <ResponsiveContainer width="100%" height={height}>
      <BarChart data={series} margin={{ top: 6, right: 6, bottom: 0, left: 0 }}>
        <CartesianGrid vertical={false} stroke="var(--border)" strokeDasharray="3 3" />
        <XAxis
          dataKey="bucket"
          tickFormatter={(value) => label(String(value), bucket)}
          minTickGap={24}
          {...axis}
        />
        <YAxis
          width={44}
          tickFormatter={(value) => formatCompact(Number(value))}
          {...axis}
        />
        <Tooltip
          cursor={{ fill: "var(--muted)", opacity: 0.4 }}
          contentStyle={tooltipStyle}
          labelFormatter={(value) => label(String(value), bucket)}
          formatter={(value, name) => [formatCompact(Number(value)), String(name)]}
        />
        <Bar
          dataKey="prompt_tokens"
          name="prompt"
          stackId="tokens"
          fill="var(--color-chart-2)"
          radius={[0, 0, 0, 0]}
        />
        <Bar
          dataKey="completion_tokens"
          name="completion"
          stackId="tokens"
          fill="var(--color-chart-4)"
          radius={[3, 3, 0, 0]}
        />
      </BarChart>
    </ResponsiveContainer>
  )
}

const palette = [
  "var(--color-chart-1)",
  "var(--color-chart-2)",
  "var(--color-chart-3)",
  "var(--color-chart-4)",
  "var(--color-chart-5)",
]

/** Horizontal bars for a breakdown, e.g. requests by model. */
function BreakdownChart({
  rows,
  height = 220,
}: {
  rows: UsageRow[]
  height?: number
}) {
  return (
    <ResponsiveContainer width="100%" height={height}>
      <BarChart
        data={rows}
        layout="vertical"
        margin={{ top: 4, right: 12, bottom: 0, left: 0 }}
      >
        <CartesianGrid horizontal={false} stroke="var(--border)" strokeDasharray="3 3" />
        <XAxis
          type="number"
          tickFormatter={(value) => formatCompact(Number(value))}
          {...axis}
        />
        <YAxis type="category" dataKey="key" width={150} {...axis} />
        <Tooltip
          cursor={{ fill: "var(--muted)", opacity: 0.4 }}
          contentStyle={tooltipStyle}
          formatter={(value) => [formatCompact(Number(value)), "requests"]}
        />
        <Bar dataKey="requests" radius={[0, 4, 4, 0]} barSize={14}>
          {rows.map((row, index) => (
            <Cell key={row.key} fill={palette[index % palette.length]} />
          ))}
        </Bar>
      </BarChart>
    </ResponsiveContainer>
  )
}

export { BreakdownChart, RequestsChart, TokensChart }
