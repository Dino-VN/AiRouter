import type * as React from "react"
import { cva, type VariantProps } from "class-variance-authority"
import {
  CircleAlertIcon,
  CircleCheckIcon,
  InfoIcon,
  TriangleAlertIcon,
} from "lucide-react"

import { cn } from "@/lib/utils"

const alertVariants = cva(
  "flex items-start gap-2.5 rounded-lg border px-3 py-2.5 text-sm [&>svg]:mt-0.5 [&>svg]:size-4 [&>svg]:shrink-0",
  {
    variants: {
      variant: {
        info: "border-border bg-muted/40 text-foreground [&>svg]:text-muted-foreground",
        success:
          "border-emerald-500/25 bg-emerald-500/8 text-emerald-800 dark:text-emerald-300 [&>svg]:text-emerald-600 dark:[&>svg]:text-emerald-400",
        warning:
          "border-amber-500/25 bg-amber-500/8 text-amber-800 dark:text-amber-300 [&>svg]:text-amber-600 dark:[&>svg]:text-amber-400",
        destructive:
          "border-destructive/30 bg-destructive/8 text-destructive [&>svg]:text-destructive",
      },
    },
    defaultVariants: {
      variant: "info",
    },
  }
)

type AlertVariant = NonNullable<VariantProps<typeof alertVariants>["variant"]>

const icons = {
  info: InfoIcon,
  success: CircleCheckIcon,
  warning: TriangleAlertIcon,
  destructive: CircleAlertIcon,
}

function Alert({
  className,
  variant,
  title,
  children,
  ...props
}: React.ComponentProps<"div"> & { variant?: AlertVariant }) {
  const Icon = icons[variant ?? "info"]
  return (
    <div
      data-slot="alert"
      role="alert"
      className={cn(alertVariants({ variant }), className)}
      {...props}
    >
      <Icon aria-hidden="true" />
      <div className="min-w-0 flex-1">
        {title ? <p className="font-medium">{title}</p> : null}
        {children ? (
          <div className={cn("break-words", title && "mt-0.5 opacity-90")}>
            {children}
          </div>
        ) : null}
      </div>
    </div>
  )
}

/** Renders nothing until there is a message, which keeps call sites terse. */
function ErrorAlert({
  error,
  className,
}: {
  error?: string | null
  className?: string
}) {
  if (!error) {
    return null
  }
  return (
    <Alert variant="destructive" className={className}>
      {error}
    </Alert>
  )
}

export { Alert, alertVariants, ErrorAlert }
