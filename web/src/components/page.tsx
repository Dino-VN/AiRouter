import type * as React from "react"
import { LoaderCircleIcon } from "lucide-react"

import { cn } from "@/lib/utils"

/** A full-height spinner for the moment before a page knows what to render:
 * the boot-time session refresh, or the check that decides between the sign-in
 * form and the first-run setup screen. Rendering either one first and correcting
 * it a beat later makes the app look broken. */
function Booting() {
  return (
    <div className="flex min-h-svh items-center justify-center bg-muted/30">
      <LoaderCircleIcon className="size-5 animate-spin text-muted-foreground" />
    </div>
  )
}

/** Title row shared by every page: heading, blurb, and right-aligned actions. */
function PageHeader({
  title,
  description,
  children,
  className,
}: {
  title: React.ReactNode
  description?: React.ReactNode
  children?: React.ReactNode
  className?: string
}) {
  return (
    <div
      className={cn(
        "flex flex-wrap items-start justify-between gap-3",
        className
      )}
    >
      <div className="grid gap-1">
        <h1 className="text-lg leading-tight font-semibold tracking-tight">
          {title}
        </h1>
        {description ? (
          <p className="max-w-2xl text-sm text-muted-foreground">
            {description}
          </p>
        ) : null}
      </div>
      {children ? (
        <div className="flex flex-wrap items-center gap-2">{children}</div>
      ) : null}
    </div>
  )
}

function Toolbar({ className, ...props }: React.ComponentProps<"div">) {
  return (
    <div
      className={cn("flex flex-wrap items-end gap-2", className)}
      {...props}
    />
  )
}

export { Booting, PageHeader, Toolbar }
