import type * as React from "react"

import { Card, CardContent } from "@/components/ui/card"
import { cn } from "@/lib/utils"

/** One headline number with an optional trailing detail line. */
function Stat({
  label,
  value,
  hint,
  icon,
  footer,
  className,
}: {
  label: React.ReactNode
  value: React.ReactNode
  hint?: React.ReactNode
  icon?: React.ReactNode
  footer?: React.ReactNode
  className?: string
}) {
  return (
    <Card className={cn("gap-0 py-3", className)}>
      <CardContent className="grid gap-1">
        <div className="flex items-center justify-between gap-2">
          <span className="text-xs font-medium text-muted-foreground">
            {label}
          </span>
          {icon ? (
            <span className="text-muted-foreground [&_svg]:size-3.5">
              {icon}
            </span>
          ) : null}
        </div>
        <span className="text-xl leading-none font-semibold tabular-nums">
          {value}
        </span>
        {hint ? (
          <span className="text-xs text-muted-foreground">{hint}</span>
        ) : null}
        {footer}
      </CardContent>
    </Card>
  )
}

function StatGrid({ className, ...props }: React.ComponentProps<"div">) {
  return (
    <div
      className={cn(
        "grid gap-3 sm:grid-cols-2 lg:grid-cols-4",
        className
      )}
      {...props}
    />
  )
}

export { Stat, StatGrid }
