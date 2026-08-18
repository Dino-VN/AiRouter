import { cn } from "@/lib/utils"

/**
 * A plain determinate bar. `value` is a fraction in [0, 1]; null means there is
 * no limit to show, so the track renders empty.
 */
function Progress({
  value,
  className,
  barClassName,
}: {
  value: number | null
  className?: string
  barClassName?: string
}) {
  const pct = value === null ? 0 : Math.min(100, Math.max(0, value * 100))
  const tone =
    value === null
      ? "bg-muted-foreground/30"
      : pct >= 90
        ? "bg-destructive"
        : pct >= 70
          ? "bg-amber-500"
          : "bg-primary"

  return (
    <div
      data-slot="progress"
      role="progressbar"
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={value === null ? undefined : Math.round(pct)}
      className={cn(
        "h-1.5 w-full overflow-hidden rounded-full bg-muted",
        className
      )}
    >
      <div
        data-slot="progress-indicator"
        className={cn("h-full rounded-full transition-all", tone, barClassName)}
        style={{ width: `${pct}%` }}
      />
    </div>
  )
}

export { Progress }
