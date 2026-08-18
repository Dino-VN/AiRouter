import type * as React from "react"

import { cn } from "@/lib/utils"

function Skeleton({ className, ...props }: React.ComponentProps<"div">) {
  return (
    <div
      data-slot="skeleton"
      className={cn("animate-pulse rounded-md bg-muted", className)}
      {...props}
    />
  )
}

/** Placeholder for a table while its first page loads. */
function SkeletonRows({ rows = 4, cols = 4 }: { rows?: number; cols?: number }) {
  return (
    <div className="grid gap-2 p-3">
      {Array.from({ length: rows }, (_, row) => (
        <div key={row} className="flex gap-3">
          {Array.from({ length: cols }, (_, col) => (
            <Skeleton
              key={col}
              className={cn("h-4 flex-1", col === 0 && "max-w-40")}
            />
          ))}
        </div>
      ))}
    </div>
  )
}

export { Skeleton, SkeletonRows }
