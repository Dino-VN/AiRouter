import * as React from "react"
import { createPortal } from "react-dom"
import { XIcon } from "lucide-react"

import { Button } from "@/components/ui/button"
import { cn } from "@/lib/utils"

/**
 * A minimal modal: portalled to the body, closes on Escape or a backdrop click,
 * and locks page scroll while open. Enough for the handful of forms in the app
 * without pulling in a headless dialog dependency.
 */
function Dialog({
  open,
  onClose,
  title,
  description,
  footer,
  children,
  className,
  wide,
}: {
  open: boolean
  onClose: () => void
  title: React.ReactNode
  description?: React.ReactNode
  footer?: React.ReactNode
  children?: React.ReactNode
  className?: string
  wide?: boolean
}) {
  const panel = React.useRef<HTMLDivElement | null>(null)

  React.useEffect(() => {
    if (!open) {
      return
    }
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        event.stopPropagation()
        onClose()
      }
    }
    document.addEventListener("keydown", onKeyDown)
    const previous = document.body.style.overflow
    document.body.style.overflow = "hidden"
    panel.current?.focus()
    return () => {
      document.removeEventListener("keydown", onKeyDown)
      document.body.style.overflow = previous
    }
  }, [open, onClose])

  if (!open) {
    return null
  }

  return createPortal(
    <div
      data-slot="dialog-overlay"
      className="fixed inset-0 z-50 flex items-start justify-center overflow-y-auto bg-black/45 p-4 backdrop-blur-[1px] sm:items-center"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget) {
          onClose()
        }
      }}
    >
      <div
        ref={panel}
        data-slot="dialog"
        role="dialog"
        aria-modal="true"
        tabIndex={-1}
        className={cn(
          "relative my-auto w-full rounded-xl border border-border bg-background p-4 shadow-lg outline-none",
          wide ? "max-w-2xl" : "max-w-md",
          className
        )}
      >
        <div className="grid gap-1 pr-8">
          <h2 className="text-sm font-medium">{title}</h2>
          {description ? (
            <p className="text-sm text-muted-foreground">{description}</p>
          ) : null}
        </div>
        <Button
          type="button"
          variant="ghost"
          size="icon-sm"
          aria-label="Close"
          onClick={onClose}
          className="absolute top-3 right-3"
        >
          <XIcon />
        </Button>
        {children ? <div className="mt-4 grid gap-3">{children}</div> : null}
        {footer ? (
          <div className="mt-5 flex items-center justify-end gap-2">{footer}</div>
        ) : null}
      </div>
    </div>,
    document.body
  )
}

export { Dialog }
