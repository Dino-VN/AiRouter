import { cn } from "@/lib/utils"

export type TabItem<T extends string> = {
  value: T
  label: string
  count?: number
}

/**
 * A controlled segmented control. Deliberately dumb: the page owns the active
 * value so it can also live in the URL or in a fetch dependency list.
 */
function Tabs<T extends string>({
  value,
  onValueChange,
  items,
  className,
}: {
  value: T
  onValueChange: (value: T) => void
  items: TabItem<T>[]
  className?: string
}) {
  return (
    <div
      data-slot="tabs"
      role="tablist"
      className={cn(
        "inline-flex h-8 items-center gap-0.5 rounded-lg bg-muted p-0.5 text-sm",
        className
      )}
    >
      {items.map((item) => {
        const active = item.value === value
        return (
          <button
            key={item.value}
            type="button"
            role="tab"
            aria-selected={active}
            onClick={() => onValueChange(item.value)}
            className={cn(
              "inline-flex h-7 items-center gap-1.5 rounded-md px-2.5 font-medium whitespace-nowrap transition-colors outline-none focus-visible:ring-3 focus-visible:ring-ring/50",
              active
                ? "bg-background text-foreground shadow-xs"
                : "text-muted-foreground hover:text-foreground"
            )}
          >
            {item.label}
            {item.count === undefined ? null : (
              <span
                className={cn(
                  "rounded px-1 text-xs tabular-nums",
                  active ? "bg-muted" : "bg-background/60"
                )}
              >
                {item.count}
              </span>
            )}
          </button>
        )
      })}
    </div>
  )
}

export { Tabs }
