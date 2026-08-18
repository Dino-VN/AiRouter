import * as React from "react"
import { Navigate, useLocation } from "react-router-dom"
import { LoaderCircleIcon, LogInIcon } from "lucide-react"

import { Booting } from "@/components/page"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import { ErrorAlert } from "@/components/ui/alert"
import { Field, Input } from "@/components/ui/field"
import { useAuth } from "@/context/auth"
import { useAsync } from "@/hooks/use-async"
import { api, errorMessage } from "@/lib/api"

type LocationState = { from?: string } | null

export default function LoginPage() {
  const { user, booting, login } = useAuth()
  const location = useLocation()
  const [username, setUsername] = React.useState("")
  const [password, setPassword] = React.useState("")
  const [error, setError] = React.useState<string | null>(null)
  const [pending, setPending] = React.useState(false)

  // Public endpoint: it supplies the version line and, on a fresh install, the
  // flag that sends this page to the setup screen instead.
  const meta = useAsync(() => api.meta(), [])

  // Nothing here can be drawn until the boot-time session refresh and /api/meta
  // have both answered. Rendering the form first and then replacing it a beat
  // later — with the app, for somebody who is already signed in, or with the
  // setup screen, on an empty database — is what made this page flicker. useAsync
  // starts out loading, so the spinner shows from the very first render; if
  // /api/meta fails outright it settles with no data and the form appears anyway.
  if (booting || (meta.loading && !meta.data)) {
    return <Booting />
  }
  if (user) {
    const state = location.state as LocationState
    return <Navigate to={state?.from ?? "/"} replace />
  }
  if (meta.data?.needs_setup) {
    return <Navigate to="/setup" replace />
  }

  const onSubmit = (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    setPending(true)
    setError(null)
    login(username.trim(), password)
      .catch((err: unknown) => {
        setError(errorMessage(err))
      })
      .finally(() => {
        setPending(false)
      })
  }

  return (
    <div className="flex min-h-svh flex-col items-center justify-center gap-4 bg-muted/30 p-4">
      <div className="flex items-center gap-2">
        <div className="flex size-7 items-center justify-center rounded-md bg-primary text-xs font-bold text-primary-foreground">
          ah
        </div>
        <span className="text-base font-semibold tracking-tight">aihub</span>
      </div>

      <Card className="w-full max-w-sm">
        <CardContent>
          <form className="grid gap-3" onSubmit={onSubmit}>
            <div className="grid gap-1">
              <h1 className="text-sm font-medium">Sign in</h1>
              <p className="text-sm text-muted-foreground">
                Use the account your administrator created for you.
              </p>
            </div>

            <ErrorAlert error={error} />

            <Field label="Username" htmlFor="username">
              <Input
                id="username"
                name="username"
                autoComplete="username"
                autoCapitalize="none"
                spellCheck={false}
                required
                autoFocus
                placeholder="admin"
                value={username}
                onChange={(event) => setUsername(event.target.value)}
              />
            </Field>

            <Field label="Password" htmlFor="password">
              <Input
                id="password"
                name="password"
                type="password"
                autoComplete="current-password"
                required
                value={password}
                onChange={(event) => setPassword(event.target.value)}
              />
            </Field>

            <Button type="submit" disabled={pending}>
              {pending ? (
                <LoaderCircleIcon className="animate-spin" />
              ) : (
                <LogInIcon />
              )}
              Sign in
            </Button>
          </form>
        </CardContent>
      </Card>

      <p className="text-xs text-muted-foreground">
        {meta.data
          ? `aihub ${meta.data.version} · ${meta.data.providers
              .map((provider) => provider.display_name)
              .join(" · ")}`
          : "aihub"}
      </p>
    </div>
  )
}
