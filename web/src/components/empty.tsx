import type * as React from "react"

import { cn } from "@/lib/utils"

/** Centred placeholder for a list with nothing in it yet. */
function Empty({
  icon,
  title,
  description,
  children,
  className,
}: {
  icon?: React.ReactNode
  title: React.ReactNode
  description?: React.ReactNode
  children?: React.ReactNode
  className?: string
}) {
  return (
    <div
      className={cn(
        "flex flex-col items-center justify-center gap-2 px-4 py-10 text-center",
        className
      )}
    >
      {icon ? (
        <div className="flex size-9 items-center justify-center rounded-lg bg-muted text-muted-foreground [&_svg]:size-4">
          {icon}
        </div>
      ) : null}
      <p className="text-sm font-medium">{title}</p>
      {description ? (
        <p className="max-w-sm text-sm text-muted-foreground">{description}</p>
      ) : null}
      {children ? <div className="mt-1 flex gap-2">{children}</div> : null}
    </div>
  )
}

export { Empty }
