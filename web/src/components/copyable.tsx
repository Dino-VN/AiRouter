import * as React from "react"
import { CheckIcon, CopyIcon, EyeIcon, EyeOffIcon } from "lucide-react"

import { Button } from "@/components/ui/button"
import { cn } from "@/lib/utils"

/**
 * Copies text without assuming a secure context: this server is often reached
 * over plain http on a LAN address, where navigator.clipboard is unavailable.
 */
async function copyText(text: string): Promise<boolean> {
  try {
    if (navigator.clipboard) {
      await navigator.clipboard.writeText(text)
      return true
    }
  } catch {
    // Fall through to the textarea trick.
  }
  try {
    const area = document.createElement("textarea")
    area.value = text
    area.setAttribute("readonly", "")
    area.style.position = "fixed"
    area.style.opacity = "0"
    document.body.appendChild(area)
    area.select()
    const ok = document.execCommand("copy")
    document.body.removeChild(area)
    return ok
  } catch {
    return false
  }
}

function useCopy(): [boolean, (text: string) => void] {
  const [copied, setCopied] = React.useState(false)
  const timer = React.useRef<number | null>(null)

  React.useEffect(
    () => () => {
      if (timer.current !== null) {
        window.clearTimeout(timer.current)
      }
    },
    []
  )

  const copy = React.useCallback((text: string) => {
    void copyText(text).then((ok) => {
      if (!ok) {
        return
      }
      setCopied(true)
      if (timer.current !== null) {
        window.clearTimeout(timer.current)
      }
      timer.current = window.setTimeout(() => setCopied(false), 1600)
    })
  }, [])

  return [copied, copy]
}

/** Monospaced value with a copy button, used for ids, keys and URLs. */
function Copyable({
  value,
  display,
  className,
  truncate = true,
}: {
  value: string
  display?: string
  className?: string
  truncate?: boolean
}) {
  const [copied, copy] = useCopy()
  return (
    <span className={cn("inline-flex min-w-0 items-center gap-1", className)}>
      <code
        title={value}
        className={cn(
          "min-w-0 rounded bg-muted px-1.5 py-0.5 font-mono text-xs",
          truncate && "truncate"
        )}
      >
        {display ?? value}
      </code>
      <Button
        type="button"
        variant="ghost"
        size="icon-xs"
        aria-label="Copy"
        onClick={() => copy(value)}
      >
        {copied ? <CheckIcon /> : <CopyIcon />}
      </Button>
    </span>
  )
}

/** A one-time secret: hidden by default, revealable, copyable. */
function SecretValue({ value }: { value: string }) {
  const [shown, setShown] = React.useState(false)
  const [copied, copy] = useCopy()

  return (
    <div className="flex items-center gap-1.5 rounded-lg border border-border bg-muted/40 p-1.5">
      <code className="min-w-0 flex-1 truncate font-mono text-xs">
        {shown ? value : "•".repeat(Math.min(48, value.length))}
      </code>
      <Button
        type="button"
        variant="ghost"
        size="icon-xs"
        aria-label={shown ? "Hide" : "Reveal"}
        onClick={() => setShown((prev) => !prev)}
      >
        {shown ? <EyeOffIcon /> : <EyeIcon />}
      </Button>
      <Button
        type="button"
        variant="outline"
        size="xs"
        onClick={() => copy(value)}
      >
        {copied ? <CheckIcon /> : <CopyIcon />}
        {copied ? "Copied" : "Copy"}
      </Button>
    </div>
  )
}

export { Copyable, SecretValue, useCopy }
