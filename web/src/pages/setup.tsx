import * as React from "react"
import { Navigate } from "react-router-dom"
import { LoaderCircleIcon, ShieldCheckIcon } from "lucide-react"

import { Booting } from "@/components/page"
import { Alert, ErrorAlert } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import { Field, Input } from "@/components/ui/field"
import { useAuth } from "@/context/auth"
import { useAsync } from "@/hooks/use-async"
import { api, ApiError, errorMessage } from "@/lib/api"
import { validUsername } from "@/lib/utils"

/**
 * First-run screen. It is what a brand-new deployment shows instead of the
 * sign-in form: the account created here becomes the administrator, and the
 * server refuses this endpoint from the moment it exists, so the page is only
 * ever reachable once.
 */
export default function SetupPage() {
  const { user, booting, adopt } = useAuth()
  const meta = useAsync(() => api.meta(), [])
  const [username, setUsername] = React.useState("")
  const [displayName, setDisplayName] = React.useState("")
  const [password, setPassword] = React.useState("")
  const [confirm, setConfirm] = React.useState("")
  const [error, setError] = React.useState<string | null>(null)
  const [pending, setPending] = React.useState(false)

  const minLength = meta.data?.min_password_len ?? 8
  const handle = username.trim()

  // Each problem is only reported once its field has something in it, so the
  // form does not open already covered in complaints.
  const usernameProblem =
    handle !== "" && !validUsername(handle)
      ? (meta.data?.username_rule ?? "that username is not allowed")
      : null
  const passwordProblem =
    password !== "" && password.length < minLength
      ? `at least ${minLength} characters`
      : null
  const confirmProblem =
    confirm !== "" && confirm !== password ? "the passwords do not match" : null
  const ready =
    handle !== "" &&
    password !== "" &&
    confirm !== "" &&
    !usernameProblem &&
    !passwordProblem &&
    !confirmProblem

  // Nothing here can be drawn until the boot-time session refresh and /api/meta
  // have both answered — and on a cold database that first meta call is the one
  // that has to wake it up. Drawing the form meanwhile only to redirect a moment
  // later looks like a bug. A meta call that fails outright settles with no data
  // and falls through to the form, which then says so.
  if (booting || (meta.loading && !meta.data)) {
    return <Booting />
  }
  // Submitting signs the new account in, which lands here and moves on.
  if (user) {
    return <Navigate to="/" replace />
  }
  // Somebody already finished setup, or the operator was bootstrapped from the
  // environment: there is nothing to do here but sign in.
  if (meta.data && !meta.data.needs_setup) {
    return <Navigate to="/login" replace />
  }

  const onSubmit = (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    setPending(true)
    setError(null)
    api
      .setup({
        username: handle,
        display_name: displayName.trim() || undefined,
        password,
      })
      .then((payload) => adopt(payload))
      .catch((err: unknown) => {
        setError(errorMessage(err))
        if (err instanceof ApiError && err.code === "setup_complete") {
          // Somebody finished setup while this form was open. Re-reading meta
          // flips needs_setup, and the guard above then hands over to sign-in.
          meta.reload()
        }
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
              <h1 className="text-sm font-medium">Create the first account</h1>
              <p className="text-sm text-muted-foreground">
                This database is empty. The account you create here is the
                administrator; it can add everyone else afterwards.
              </p>
            </div>

            <ErrorAlert error={error} />
            {meta.error ? (
              <Alert variant="warning">
                Could not reach the server ({meta.error}). Setup may still work,
                but check that the database is up first.
              </Alert>
            ) : null}

            <Field
              label="Username"
              htmlFor="username"
              hint={meta.data?.username_rule}
              error={usernameProblem}
            >
              <Input
                id="username"
                name="username"
                autoComplete="username"
                autoCapitalize="none"
                spellCheck={false}
                required
                autoFocus
                placeholder="admin"
                aria-invalid={usernameProblem ? true : undefined}
                value={username}
                onChange={(event) => setUsername(event.target.value)}
              />
            </Field>

            <Field
              label="Display name"
              htmlFor="display_name"
              hint="Optional. Defaults to the username."
            >
              <Input
                id="display_name"
                name="display_name"
                autoComplete="name"
                placeholder="Administrator"
                value={displayName}
                onChange={(event) => setDisplayName(event.target.value)}
              />
            </Field>

            <Field
              label="Password"
              htmlFor="password"
              hint={`At least ${minLength} characters.`}
              error={passwordProblem}
            >
              <Input
                id="password"
                name="password"
                type="password"
                autoComplete="new-password"
                required
                aria-invalid={passwordProblem ? true : undefined}
                value={password}
                onChange={(event) => setPassword(event.target.value)}
              />
            </Field>

            <Field
              label="Confirm password"
              htmlFor="confirm"
              error={confirmProblem}
            >
              <Input
                id="confirm"
                name="confirm"
                type="password"
                autoComplete="new-password"
                required
                aria-invalid={confirmProblem ? true : undefined}
                value={confirm}
                onChange={(event) => setConfirm(event.target.value)}
              />
            </Field>

            <Button type="submit" disabled={pending || !ready}>
              {pending ? (
                <LoaderCircleIcon className="animate-spin" />
              ) : (
                <ShieldCheckIcon />
              )}
              Create account and sign in
            </Button>
          </form>
        </CardContent>
      </Card>

      <p className="max-w-sm text-center text-xs text-muted-foreground">
        Anyone who can reach this page right now can claim the deployment, so
        finish setup before exposing the port. Unattended installs can create the
        account up front with AIHUB_ADMIN_USERNAME and AIHUB_ADMIN_PASSWORD
        instead.
      </p>
    </div>
  )
}
