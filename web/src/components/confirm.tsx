import * as React from "react"

import { Button } from "@/components/ui/button"
import { Dialog } from "@/components/ui/dialog"
import { ErrorAlert } from "@/components/ui/alert"

export type ConfirmRequest = {
  title: string
  description?: React.ReactNode
  confirmLabel?: string
  destructive?: boolean
  run: () => Promise<unknown>
}

/**
 * One dialog per page, driven by a request object. Pages set the request when a
 * destructive button is pressed and clear it when the work is done.
 */
function ConfirmDialog({
  request,
  onClose,
  onDone,
}: {
  request: ConfirmRequest | null
  onClose: () => void
  onDone?: () => void
}) {
  const [pending, setPending] = React.useState(false)
  const [error, setError] = React.useState<string | null>(null)

  React.useEffect(() => {
    setError(null)
    setPending(false)
  }, [request])

  const confirm = () => {
    if (!request) {
      return
    }
    setPending(true)
    request
      .run()
      .then(() => {
        setPending(false)
        onClose()
        if (onDone) {
          onDone()
        }
      })
      .catch((err: unknown) => {
        setPending(false)
        setError(err instanceof Error ? err.message : String(err))
      })
  }

  return (
    <Dialog
      open={request !== null}
      onClose={onClose}
      title={request?.title ?? ""}
      description={request?.description}
      footer={
        <>
          <Button type="button" variant="ghost" onClick={onClose} disabled={pending}>
            Cancel
          </Button>
          <Button
            type="button"
            variant={request?.destructive === false ? "default" : "destructive"}
            onClick={confirm}
            disabled={pending}
          >
            {request?.confirmLabel ?? "Confirm"}
          </Button>
        </>
      }
    >
      {error ? <ErrorAlert error={error} /> : undefined}
    </Dialog>
  )
}

export { ConfirmDialog }
