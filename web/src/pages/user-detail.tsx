import * as React from "react"
import { Link, useParams } from "react-router-dom"
import {
  ArrowLeftIcon,
  KeyRoundIcon,
  RefreshCwIcon,
  SaveIcon,
  ShieldIcon,
} from "lucide-react"

import { Copyable, SecretValue } from "@/components/copyable"
import { Empty } from "@/components/empty"
import { PageHeader } from "@/components/page"
import { Stat, StatGrid } from "@/components/stat"
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
import { Dialog } from "@/components/ui/dialog"
import { Field, Input, Label, Select, Switch } from "@/components/ui/field"
import { Skeleton } from "@/components/ui/skeleton"
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
import { api, errorMessage } from "@/lib/api"
import {
  formatCompact,
  formatDateTime,
  formatNumber,
  formatRelative,
  providerLabel,
  titleCase,
} from "@/lib/format"
import type { Meta, Quota } from "@/lib/types"

export default function UserDetailPage() {
  const { id = "" } = useParams()
  const { user: me } = useAuth()
  const [resetting, setResetting] = React.useState(false)

  const view = useAsync(async () => {
    const [detail, meta] = await Promise.all([api.admin.user(id), api.meta()])
    return { detail, meta }
  }, [id])

  const detail = view.data?.detail
  const meta = view.data?.meta
  const reload = view.reload
  const self = detail?.user.id === me?.id

  return (
    <div className="grid gap-5">
      <div>
        <Link
          to="/admin/users"
          className="inline-flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground"
        >
          <ArrowLeftIcon className="size-3.5" />
          All users
        </Link>
      </div>

      <PageHeader
        title={detail?.user.display_name || detail?.user.username || "Account"}
        description={detail ? detail.user.username : "Loading the account…"}
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
        <Button
          type="button"
          size="sm"
          variant="outline"
          onClick={() => setResetting(true)}
          disabled={!detail}
        >
          <KeyRoundIcon />
          Reset password
        </Button>
      </PageHeader>

      <ErrorAlert error={view.error} />

      {!detail || !meta ? (
        <div className="grid gap-3">
          <Skeleton className="h-40 w-full" />
          <Skeleton className="h-64 w-full" />
        </div>
      ) : (
        <>
          <StatGrid>
            <Stat
              label="Requests, 24h"
              value={formatNumber(detail.usage.last_24h.requests)}
              hint={`${formatCompact(detail.usage.last_24h.total_tokens)} tokens`}
            />
            <Stat
              label="Requests, 30d"
              value={formatNumber(detail.usage.last_30d.requests)}
              hint={`${formatCompact(detail.usage.last_30d.total_tokens)} tokens`}
            />
            <Stat
              label="Connections"
              value={formatNumber(detail.connections?.length ?? 0)}
              hint={`${detail.pending_connections?.length ?? 0} waiting to finish`}
            />
            <Stat
              label="Signed-in browsers"
              value={formatNumber(detail.web_sessions)}
              hint={`${detail.api_keys?.length ?? 0} API key${
                (detail.api_keys?.length ?? 0) === 1 ? "" : "s"
              }`}
            />
          </StatGrid>

          <div className="grid gap-4 lg:grid-cols-2">
            <ProfileCard
              userId={detail.user.id}
              displayName={detail.user.display_name}
              role={detail.user.role}
              status={detail.user.status}
              self={self}
              createdAt={detail.user.created_at}
              lastLoginAt={detail.user.last_login_at}
              onSaved={reload}
            />

            <QuotaCard
              userId={detail.user.id}
              quota={detail.quota}
              meta={meta}
              onSaved={reload}
            />
          </div>

          <Card>
            <CardHeader>
              <CardTitle>
                Connections{" "}
                <span className="text-muted-foreground">
                  ({detail.connections?.length ?? 0})
                </span>
              </CardTitle>
              <CardDescription>
                Upstream accounts this user owns. Shared ones are usable by
                everybody.
              </CardDescription>
            </CardHeader>
            <CardContent className="px-0">
              {!detail.connections || detail.connections.length === 0 ? (
                <Empty title="No connections" />
              ) : (
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Label</TableHead>
                      <TableHead>Provider</TableHead>
                      <TableHead>Account</TableHead>
                      <TableHead>Scope</TableHead>
                      <TableHead>Status</TableHead>
                      <TableHead>Requests 24h</TableHead>
                      <TableHead>Last used</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {detail.connections.map((conn) => (
                      <TableRow key={conn.id}>
                        <TableCell className="font-medium">
                          {conn.label || "—"}
                        </TableCell>
                        <TableCell>{providerLabel(conn.provider)}</TableCell>
                        <TableCell className="text-muted-foreground">
                          {conn.account_email || "—"}
                        </TableCell>
                        <TableCell>
                          <Badge
                            variant={
                              conn.scope === "shared" ? "secondary" : "muted"
                            }
                          >
                            {conn.scope}
                          </Badge>
                        </TableCell>
                        <TableCell>
                          <StatusBadge status={conn.status} />
                        </TableCell>
                        <TableCell className="tabular-nums">
                          {formatNumber(conn.request_count_24h ?? 0)}
                        </TableCell>
                        <TableCell className="text-muted-foreground">
                          {formatRelative(conn.last_used_at)}
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
                <CardTitle>
                  API keys{" "}
                  <span className="text-muted-foreground">
                    ({detail.api_keys?.length ?? 0})
                  </span>
                </CardTitle>
                <CardDescription>
                  Secrets are hashed, so only the prefix is visible.
                </CardDescription>
              </CardHeader>
              <CardContent className="px-0">
                {!detail.api_keys || detail.api_keys.length === 0 ? (
                  <Empty title="No API keys" />
                ) : (
                  <Table>
                    <TableHeader>
                      <TableRow>
                        <TableHead>Name</TableHead>
                        <TableHead>Prefix</TableHead>
                        <TableHead>Status</TableHead>
                        <TableHead>Last used</TableHead>
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {detail.api_keys.map((key) => (
                        <TableRow key={key.id}>
                          <TableCell className="font-medium">
                            {key.name}
                          </TableCell>
                          <TableCell>
                            <Copyable value={key.prefix} truncate={false} />
                          </TableCell>
                          <TableCell>
                            <StatusBadge status={key.status} />
                          </TableCell>
                          <TableCell className="text-muted-foreground">
                            {formatRelative(key.last_used_at)}
                          </TableCell>
                        </TableRow>
                      ))}
                    </TableBody>
                  </Table>
                )}
              </CardContent>
            </Card>

            <Card>
              <CardHeader>
                <CardTitle>
                  Temporary connections{" "}
                  <span className="text-muted-foreground">
                    ({detail.pending_connections?.length ?? 0})
                  </span>
                </CardTitle>
                <CardDescription>
                  Sign-ins this account started and has not finished.
                </CardDescription>
              </CardHeader>
              <CardContent className="px-0">
                {!detail.pending_connections ||
                detail.pending_connections.length === 0 ? (
                  <Empty title="Nothing in flight" />
                ) : (
                  <Table>
                    <TableHeader>
                      <TableRow>
                        <TableHead>Provider</TableHead>
                        <TableHead>Scope</TableHead>
                        <TableHead>Started</TableHead>
                        <TableHead>Expires</TableHead>
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {detail.pending_connections.map((session) => (
                        <TableRow key={session.id}>
                          <TableCell>
                            {providerLabel(session.provider)}
                          </TableCell>
                          <TableCell className="text-muted-foreground">
                            {session.target_scope}
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
                        </TableRow>
                      ))}
                    </TableBody>
                  </Table>
                )}
              </CardContent>
            </Card>
          </div>
        </>
      )}

      <ResetPasswordDialog
        open={resetting}
        userId={id}
        username={detail?.user.username ?? ""}
        onClose={() => setResetting(false)}
      />
    </div>
  )
}

function ProfileCard({
  userId,
  displayName,
  role,
  status,
  self,
  createdAt,
  lastLoginAt,
  onSaved,
}: {
  userId: string
  displayName: string
  role: string
  status: string
  self: boolean
  createdAt: string
  lastLoginAt?: string
  onSaved: () => void
}) {
  const [name, setName] = React.useState(displayName)
  const [nextRole, setNextRole] = React.useState(role)
  const [nextStatus, setNextStatus] = React.useState(status)
  const [error, setError] = React.useState<string | null>(null)
  const [saved, setSaved] = React.useState(false)
  const [pending, setPending] = React.useState(false)

  React.useEffect(() => {
    setName(displayName)
    setNextRole(role)
    setNextStatus(status)
  }, [displayName, role, status])

  const dirty =
    name !== displayName || nextRole !== role || nextStatus !== status

  const save = () => {
    setPending(true)
    setError(null)
    setSaved(false)
    api.admin
      .updateUser(userId, {
        display_name: name !== displayName ? name.trim() : undefined,
        role: nextRole !== role ? nextRole : undefined,
        status: nextStatus !== status ? nextStatus : undefined,
      })
      .then(() => {
        setPending(false)
        setSaved(true)
        onSaved()
      })
      .catch((err: unknown) => {
        setPending(false)
        setError(errorMessage(err))
      })
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>Profile</CardTitle>
        <CardDescription>
          A role or status change signs this account out everywhere.
        </CardDescription>
        <CardAction>
          <Badge variant={role === "admin" ? "default" : "secondary"}>
            {titleCase(role)}
          </Badge>
        </CardAction>
      </CardHeader>
      <CardContent className="grid gap-3">
        <ErrorAlert error={error} />
        {saved && !dirty ? <Alert variant="success">Saved.</Alert> : null}
        <Field label="Display name" htmlFor="profile-name">
          <Input
            id="profile-name"
            value={name}
            onChange={(event) => setName(event.target.value)}
          />
        </Field>
        <div className="grid gap-3 sm:grid-cols-2">
          <Field
            label="Role"
            htmlFor="profile-role"
            hint={self ? "You cannot demote yourself." : undefined}
          >
            <Select
              id="profile-role"
              value={nextRole}
              disabled={self}
              onChange={(event) => setNextRole(event.target.value)}
            >
              <option value="user">User</option>
              <option value="admin">Admin</option>
            </Select>
          </Field>
          <Field
            label="Status"
            htmlFor="profile-status"
            hint={self ? "You cannot suspend yourself." : undefined}
          >
            <Select
              id="profile-status"
              value={nextStatus}
              disabled={self}
              onChange={(event) => setNextStatus(event.target.value)}
            >
              <option value="active">Active</option>
              <option value="suspended">Suspended</option>
              <option value="revoked">Revoked</option>
            </Select>
          </Field>
        </div>
        <div className="grid gap-1 text-xs text-muted-foreground">
          <span>Created {formatDateTime(createdAt)}</span>
          <span>Last sign-in {formatRelative(lastLoginAt)}</span>
        </div>
        <div className="flex justify-end">
          <Button
            type="button"
            size="sm"
            onClick={save}
            disabled={pending || !dirty}
          >
            <SaveIcon />
            Save profile
          </Button>
        </div>
      </CardContent>
    </Card>
  )
}

function QuotaCard({
  userId,
  quota,
  meta,
  onSaved,
}: {
  userId: string
  quota: Quota
  meta: Meta
  onSaved: () => void
}) {
  const [reqDay, setReqDay] = React.useState("0")
  const [tokDay, setTokDay] = React.useState("0")
  const [reqMonth, setReqMonth] = React.useState("0")
  const [tokMonth, setTokMonth] = React.useState("0")
  const [maxConns, setMaxConns] = React.useState("0")
  const [maxKeys, setMaxKeys] = React.useState("0")
  const [concurrent, setConcurrent] = React.useState("0")
  const [shared, setShared] = React.useState(true)
  const [allowed, setAllowed] = React.useState<string[]>([])
  const [error, setError] = React.useState<string | null>(null)
  const [saved, setSaved] = React.useState(false)
  const [pending, setPending] = React.useState(false)

  React.useEffect(() => {
    setReqDay(String(quota.requests_per_day))
    setTokDay(String(quota.tokens_per_day))
    setReqMonth(String(quota.requests_per_month))
    setTokMonth(String(quota.tokens_per_month))
    setMaxConns(String(quota.max_connections))
    setMaxKeys(String(quota.max_api_keys))
    setConcurrent(String(quota.concurrent_limit))
    setShared(quota.allow_shared_pool)
    setAllowed(quota.allowed_providers ?? [])
    setSaved(false)
  }, [quota])

  const numbers: { label: string; value: string; set: (v: string) => void }[] = [
    { label: "Requests per day", value: reqDay, set: setReqDay },
    { label: "Tokens per day", value: tokDay, set: setTokDay },
    { label: "Requests per month", value: reqMonth, set: setReqMonth },
    { label: "Tokens per month", value: tokMonth, set: setTokMonth },
    { label: "Max connections", value: maxConns, set: setMaxConns },
    { label: "Max API keys", value: maxKeys, set: setMaxKeys },
    { label: "Concurrent requests", value: concurrent, set: setConcurrent },
  ]

  const toggleProvider = (id: string) => {
    setAllowed((current) =>
      current.includes(id)
        ? current.filter((value) => value !== id)
        : [...current, id]
    )
  }

  const save = () => {
    setPending(true)
    setError(null)
    setSaved(false)
    const next: Quota = {
      ...quota,
      user_id: userId,
      requests_per_day: Number(reqDay) || 0,
      tokens_per_day: Number(tokDay) || 0,
      requests_per_month: Number(reqMonth) || 0,
      tokens_per_month: Number(tokMonth) || 0,
      max_connections: Number(maxConns) || 0,
      max_api_keys: Number(maxKeys) || 0,
      concurrent_limit: Number(concurrent) || 0,
      allow_shared_pool: shared,
      allowed_providers: allowed,
    }
    api.admin
      .setQuota(userId, next)
      .then(() => {
        setPending(false)
        setSaved(true)
        onSaved()
      })
      .catch((err: unknown) => {
        setPending(false)
        setError(errorMessage(err))
      })
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>Quota</CardTitle>
        <CardDescription>
          Zero means unlimited. Windows are anchored to UTC.
        </CardDescription>
        <CardAction>
          <Badge variant="outline">
            updated {formatRelative(quota.updated_at)}
          </Badge>
        </CardAction>
      </CardHeader>
      <CardContent className="grid gap-3">
        <ErrorAlert error={error} />
        {saved ? <Alert variant="success">Quota saved.</Alert> : null}
        <div className="grid gap-3 sm:grid-cols-2">
          {numbers.map((field) => (
            <Field key={field.label} label={field.label}>
              <Input
                type="number"
                min={0}
                inputMode="numeric"
                value={field.value}
                onChange={(event) => field.set(event.target.value)}
              />
            </Field>
          ))}
        </div>
        <Field
          label="Shared pool"
          hint="Whether this account may borrow connections other admins have shared."
        >
          <Label className="text-sm font-normal">
            <Switch
              checked={shared}
              onChange={(event) => setShared(event.target.checked)}
            />
            Allow shared connections
          </Label>
        </Field>
        <Field
          label="Allowed providers"
          hint="Leave everything unchecked to allow every provider this server has enabled."
        >
          <div className="flex flex-wrap gap-3">
            {meta.providers.map((provider) => (
              <Label key={provider.id} className="text-sm font-normal">
                <Switch
                  checked={allowed.includes(provider.id)}
                  onChange={() => toggleProvider(provider.id)}
                />
                {provider.display_name}
              </Label>
            ))}
          </div>
        </Field>
        <div className="flex justify-end">
          <Button type="button" size="sm" onClick={save} disabled={pending}>
            <ShieldIcon />
            Save quota
          </Button>
        </div>
      </CardContent>
    </Card>
  )
}

function ResetPasswordDialog({
  open,
  userId,
  username,
  onClose,
}: {
  open: boolean
  userId: string
  username: string
  onClose: () => void
}) {
  const [password, setPassword] = React.useState("")
  const [generated, setGenerated] = React.useState<string | null>(null)
  const [error, setError] = React.useState<string | null>(null)
  const [pending, setPending] = React.useState(false)
  const [done, setDone] = React.useState(false)

  React.useEffect(() => {
    setPassword("")
    setGenerated(null)
    setError(null)
    setPending(false)
    setDone(false)
  }, [open])

  const submit = () => {
    setPending(true)
    setError(null)
    api.admin
      .setPassword(userId, password)
      .then((result) => {
        setPending(false)
        setDone(true)
        setGenerated(result.password ?? null)
      })
      .catch((err: unknown) => {
        setPending(false)
        setError(errorMessage(err))
      })
  }

  return (
    <Dialog
      open={open}
      onClose={onClose}
      title="Reset password"
      description={`Every session belonging to ${username} is dropped.`}
      footer={
        done ? (
          <Button type="button" onClick={onClose}>
            Done
          </Button>
        ) : (
          <>
            <Button
              type="button"
              variant="ghost"
              onClick={onClose}
              disabled={pending}
            >
              Cancel
            </Button>
            <Button type="button" onClick={submit} disabled={pending}>
              <KeyRoundIcon />
              Reset
            </Button>
          </>
        )
      }
    >
      <ErrorAlert error={error} />
      {done ? (
        generated ? (
          <>
            <Field label="Generated password">
              <SecretValue value={generated} />
            </Field>
            <Alert variant="warning">
              Shown once. Pass it on out of band and have the account change it.
            </Alert>
          </>
        ) : (
          <Alert variant="success">
            The password you entered is now active.
          </Alert>
        )
      ) : (
        <Field
          label="New password"
          hint="Leave empty and the server generates one, shown to you once."
        >
          <Input
            type="password"
            autoComplete="new-password"
            value={password}
            onChange={(event) => setPassword(event.target.value)}
          />
        </Field>
      )}
    </Dialog>
  )
}
