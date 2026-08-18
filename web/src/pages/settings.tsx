import * as React from "react"
import {
  LaptopIcon,
  LockIcon,
  MoonIcon,
  RefreshCwIcon,
  SunIcon,
} from "lucide-react"

import { ConfirmDialog, type ConfirmRequest } from "@/components/confirm"
import { Copyable } from "@/components/copyable"
import { PageHeader } from "@/components/page"
import { useTheme } from "@/components/theme-provider"
import { Alert, ErrorAlert } from "@/components/ui/alert"
import { Badge, StatusBadge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import {
  Card,
  CardAction,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"
import { Field, Input } from "@/components/ui/field"
import { Separator } from "@/components/ui/separator"
import { SkeletonRows } from "@/components/ui/skeleton"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { useAuth } from "@/context/auth"
import { useAsync } from "@/hooks/use-async"
import { api, errorMessage, setAccessToken } from "@/lib/api"
import {
  formatDateTime,
  formatRelative,
  providerLabel,
  shortUserAgent,
  titleCase,
} from "@/lib/format"

export default function SettingsPage() {
  const { user, logout, apply, reloadMe } = useAuth()
  const [confirmRequest, setConfirmRequest] =
    React.useState<ConfirmRequest | null>(null)

  const view = useAsync(async () => {
    const [meta, sessions] = await Promise.all([
      api.meta(),
      api.auth.sessions(),
    ])
    return { meta, sessions }
  }, [])

  const meta = view.data?.meta
  const sessions = view.data?.sessions.sessions ?? []
  const reload = view.reload

  return (
    <div className="grid gap-5">
      <PageHeader
        title="Settings"
        description="Your account, the browsers it is signed in on, and what this server is running."
      >
        <Button
          type="button"
          variant="outline"
          size="sm"
          onClick={reload}
          disabled={view.loading}
        >
          <RefreshCwIcon className={view.loading ? "animate-spin" : undefined} />
          Refresh
        </Button>
      </PageHeader>

      <ErrorAlert error={view.error} />

      <div className="grid gap-4 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle>Account</CardTitle>
            <CardDescription>
              Only an administrator can change your name, role, or quota.
            </CardDescription>
          </CardHeader>
          <CardContent className="grid gap-3 text-sm">
            <Row label="Username">
              <Copyable value={user?.username ?? ""} truncate={false} />
            </Row>
            <Row label="Name">{user?.display_name || "—"}</Row>
            <Row label="Role">
              <Badge variant={user?.role === "admin" ? "default" : "secondary"}>
                {titleCase(user?.role ?? "")}
              </Badge>
            </Row>
            <Row label="Status">
              <StatusBadge status={user?.status ?? "active"} />
            </Row>
            <Separator />
            <Row label="Member since">{formatDateTime(user?.created_at)}</Row>
            <Row label="Last sign-in">{formatRelative(user?.last_login_at)}</Row>
          </CardContent>
        </Card>

        <PasswordCard
          minLength={meta?.min_password_len ?? 8}
          onChanged={reload}
          apply={apply}
          reloadMe={reloadMe}
        />
      </div>

      <Card>
        <CardHeader>
          <CardTitle>
            Signed-in browsers{" "}
            <span className="text-muted-foreground">({sessions.length})</span>
          </CardTitle>
          <CardDescription>
            Each row is a refresh session. Revoking one signs that browser out the
            next time it renews.
          </CardDescription>
        </CardHeader>
        <CardContent className="px-0">
          {view.loading && sessions.length === 0 ? (
            <SkeletonRows rows={2} cols={4} />
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Client</TableHead>
                  <TableHead>IP</TableHead>
                  <TableHead>Signed in</TableHead>
                  <TableHead>Expires</TableHead>
                  <TableHead className="text-right">Actions</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {sessions.map((session) => (
                  <TableRow key={session.id}>
                    <TableCell>
                      <span className="flex items-center gap-2">
                        {shortUserAgent(session.user_agent)}
                        {session.current ? (
                          <Badge variant="success">this browser</Badge>
                        ) : null}
                      </span>
                    </TableCell>
                    <TableCell className="font-mono text-xs">
                      {session.ip || "—"}
                    </TableCell>
                    <TableCell className="text-muted-foreground">
                      {formatRelative(session.created_at)}
                    </TableCell>
                    <TableCell
                      className="text-muted-foreground"
                      title={formatDateTime(session.expires_at)}
                    >
                      {formatRelative(session.expires_at)}
                    </TableCell>
                    <TableCell>
                      <div className="flex justify-end">
                        <Button
                          type="button"
                          size="xs"
                          variant="outline"
                          onClick={() =>
                            setConfirmRequest({
                              title: session.current
                                ? "Sign this browser out?"
                                : "Revoke this session?",
                              description: session.current
                                ? "You will be returned to the login page."
                                : `${shortUserAgent(session.user_agent)} will have to sign in again.`,
                              confirmLabel: session.current
                                ? "Sign out"
                                : "Revoke",
                              run: async () => {
                                await api.auth.revokeSession(session.id)
                                if (session.current) {
                                  await logout()
                                  return
                                }
                                reload()
                              },
                            })
                          }
                        >
                          {session.current ? "Sign out" : "Revoke"}
                        </Button>
                      </div>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      <div className="grid gap-4 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle>Appearance</CardTitle>
            <CardDescription>Stored in this browser only.</CardDescription>
          </CardHeader>
          <CardContent>
            <ThemePicker />
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>Server</CardTitle>
            <CardDescription>What this deployment is running.</CardDescription>
            <CardAction>
              <Badge variant="outline">{meta?.version ?? "…"}</Badge>
            </CardAction>
          </CardHeader>
          <CardContent className="grid gap-3 text-sm">
            <Row label="Base URL">
              <Copyable
                value={
                  meta?.public_url ||
                  (typeof window === "undefined" ? "" : window.location.origin)
                }
              />
            </Row>
            <Row label="Key prefix">
              <code className="font-mono text-xs">
                {meta?.api_key_prefix ?? "—"}
              </code>
            </Row>
            <Row label="Providers">
              <span className="flex flex-wrap gap-1">
                {(meta?.providers ?? []).map((provider) => (
                  <Badge key={provider.id} variant="outline">
                    {providerLabel(provider.id)}
                    <span className="text-muted-foreground">
                      :{provider.loopback_port}
                    </span>
                  </Badge>
                ))}
              </span>
            </Row>
            <Row label="Loopback OAuth">
              {meta?.local_oauth ? (
                <Badge variant="success">listening</Badge>
              ) : (
                <Badge variant="muted">paste the code manually</Badge>
              )}
            </Row>
          </CardContent>
        </Card>
      </div>

      <ConfirmDialog
        request={confirmRequest}
        onClose={() => setConfirmRequest(null)}
      />
    </div>
  )
}

function Row({
  label,
  children,
}: {
  label: string
  children: React.ReactNode
}) {
  return (
    <div className="flex flex-wrap items-center justify-between gap-2">
      <span className="text-muted-foreground">{label}</span>
      <span className="min-w-0 max-w-[65%] text-right">{children}</span>
    </div>
  )
}

function ThemePicker() {
  const { theme, setTheme } = useTheme()
  return (
    <div className="flex gap-2">
      <Button
        type="button"
        size="sm"
        variant={theme === "light" ? "default" : "outline"}
        onClick={() => setTheme("light")}
      >
        <SunIcon />
        Light
      </Button>
      <Button
        type="button"
        size="sm"
        variant={theme === "dark" ? "default" : "outline"}
        onClick={() => setTheme("dark")}
      >
        <MoonIcon />
        Dark
      </Button>
      <Button
        type="button"
        size="sm"
        variant={theme === "system" ? "default" : "outline"}
        onClick={() => setTheme("system")}
      >
        <LaptopIcon />
        System
      </Button>
    </div>
  )
}

function PasswordCard({
  minLength,
  onChanged,
  apply,
  reloadMe,
}: {
  minLength: number
  onChanged: () => void
  apply: ReturnType<typeof useAuth>["apply"]
  reloadMe: ReturnType<typeof useAuth>["reloadMe"]
}) {
  const [current, setCurrent] = React.useState("")
  const [next, setNext] = React.useState("")
  const [confirm, setConfirm] = React.useState("")
  const [error, setError] = React.useState<string | null>(null)
  const [done, setDone] = React.useState(false)
  const [pending, setPending] = React.useState(false)

  const submit = (event: React.FormEvent) => {
    event.preventDefault()
    setError(null)
    setDone(false)
    if (next.length < minLength) {
      setError(`the new password needs at least ${minLength} characters`)
      return
    }
    if (next !== confirm) {
      setError("the two new passwords do not match")
      return
    }
    setPending(true)
    api.auth
      .changePassword(current, next)
      .then(async (payload) => {
        // Changing the password revokes every session, including this one, and
        // the server hands back a replacement. Adopt it so the page keeps working.
        setAccessToken(payload.access_token)
        apply(payload)
        try {
          await reloadMe()
        } catch {
          // Not fatal: the token is fresh, the page can refetch on its own.
        }
        setPending(false)
        setCurrent("")
        setNext("")
        setConfirm("")
        setDone(true)
        onChanged()
      })
      .catch((err: unknown) => {
        setPending(false)
        setError(errorMessage(err))
      })
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>Password</CardTitle>
        <CardDescription>
          Changing it signs every other browser out.
        </CardDescription>
      </CardHeader>
      <CardContent>
        <form className="grid gap-3" onSubmit={submit}>
          <ErrorAlert error={error} />
          {done ? <Alert variant="success">Password updated.</Alert> : null}
          <Field label="Current password" htmlFor="pw-current">
            <Input
              id="pw-current"
              type="password"
              autoComplete="current-password"
              value={current}
              onChange={(event) => setCurrent(event.target.value)}
            />
          </Field>
          <Field
            label="New password"
            hint={`At least ${minLength} characters.`}
            htmlFor="pw-new"
          >
            <Input
              id="pw-new"
              type="password"
              autoComplete="new-password"
              value={next}
              onChange={(event) => setNext(event.target.value)}
            />
          </Field>
          <Field label="Repeat new password" htmlFor="pw-confirm">
            <Input
              id="pw-confirm"
              type="password"
              autoComplete="new-password"
              value={confirm}
              onChange={(event) => setConfirm(event.target.value)}
            />
          </Field>
          <div className="flex justify-end">
            <Button
              type="submit"
              size="sm"
              disabled={pending || !current || !next}
            >
              <LockIcon />
              Change password
            </Button>
          </div>
        </form>
      </CardContent>
    </Card>
  )
}
