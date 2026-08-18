import type * as React from "react"
import { cva, type VariantProps } from "class-variance-authority"

import { cn } from "@/lib/utils"

const badgeVariants = cva(
  "inline-flex w-fit shrink-0 items-center justify-center gap-1 rounded-md border px-1.5 py-0.5 text-xs font-medium whitespace-nowrap [&_svg]:pointer-events-none [&_svg:not([class*='size-'])]:size-3",
  {
    variants: {
      variant: {
        default: "border-transparent bg-primary text-primary-foreground",
        secondary: "border-transparent bg-secondary text-secondary-foreground",
        outline: "border-border text-foreground",
        muted: "border-transparent bg-muted text-muted-foreground",
        success:
          "border-transparent bg-emerald-500/12 text-emerald-700 dark:text-emerald-400",
        warning:
          "border-transparent bg-amber-500/12 text-amber-700 dark:text-amber-400",
        destructive:
          "border-transparent bg-destructive/12 text-destructive dark:bg-destructive/20",
      },
    },
    defaultVariants: {
      variant: "default",
    },
  }
)

function Badge({
  className,
  variant,
  ...props
}: React.ComponentProps<"span"> & VariantProps<typeof badgeVariants>) {
  return (
    <span
      data-slot="badge"
      className={cn(badgeVariants({ variant }), className)}
      {...props}
    />
  )
}

/** Maps a lifecycle status onto a badge colour so tables stay scannable. */
function StatusBadge({
  status,
  className,
}: {
  status: string
  className?: string
}) {
  const variant =
    status === "active"
      ? "success"
      : status === "pending"
        ? "warning"
        : status === "disabled" || status === "suspended"
          ? "muted"
          : status === "completed"
            ? "secondary"
            : status === "error" ||
                status === "failed" ||
                status === "revoked" ||
                status === "expired"
              ? "destructive"
              : "outline"

  return (
    <Badge variant={variant} className={className}>
      {status}
    </Badge>
  )
}

export { Badge, badgeVariants, StatusBadge }
