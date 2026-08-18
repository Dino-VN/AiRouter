import * as React from "react"
import {
  BanIcon,
  CircleCheckIcon,
  PlusIcon,
  RefreshCwIcon,
  Trash2Icon,
  UserPlusIcon,
} from "lucide-react"
import { Link } from "react-router-dom"

import { ConfirmDialog, type ConfirmRequest } from "@/components/confirm"
import { Copyable, SecretValue } from "@/components/copyable"
import { LinkButton } from "@/components/link-button"
import { PageHeader } from "@/components/page"
import { Alert, ErrorAlert } from "@/components/ui/alert"
import { Badge, StatusBadge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"
import { Dialog } from "@/components/ui/dialog"
import { Field, Input, Select } from "@/components/ui/field"
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
import { api, errorMessage } from "@/lib/api"
import { formatCompact, formatNumber, formatRelative, titleCase } from "@/lib/format"
import type { CreateUserResponse } from "@/lib/types"
import { validUsername } from "@/lib/utils"

export default function UsersPage() {
  const { user: me } = useAuth()
  const [creating, setCreating] = React.useState(false)
  const [created, setCreated] = React.useState<CreateUserResponse | null>(null)
  const [confirmRequest, setConfirmRequest] =
    React.useState<ConfirmRequest | null>(null)

  const view = useAsync(() => api.admin.users(), [])
  const rows = view.data?.users ?? []
  const reload = view.reload

  return (
    <div className="grid gap-5">
      <PageHeader
        title="Users"
        description="Every account on this server. Each one gets its own connections, keys and quota."
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
        <Button type="button" size="sm" onClick={() => setCreating(true)}>
          <UserPlusIcon />
          New user
        </Button>
      </PageHeader>

      <ErrorAlert error={view.error} />

      <Card>
        <CardHeader>
          <CardTitle>
            Accounts <span className="text-muted-foreground">({rows.length})</span>
          </CardTitle>
          <CardDescription>
            Suspending an account, or changing its role, drops its sessions
            immediately.
          </CardDescription>
        </CardHeader>
        <CardContent className="px-0">
          {view.loading && rows.length === 0 ? (
            <SkeletonRows rows={4} cols={6} />
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Account</TableHead>
                  <TableHead>Role</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Connections</TableHead>
                  <TableHead>Keys</TableHead>
                  <TableHead>Requests 30d</TableHead>
                  <TableHead>Last sign-in</TableHead>
                  <TableHead className="text-right">Actions</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {rows.map((row) => {
                  const self = row.user.id === me?.id
                  const suspended = row.user.status !== "active"
                  return (
                    <TableRow key={row.user.id}>
                      <TableCell>
                        <Link
                          to={`/admin/users/${row.user.id}`}
                          className="grid gap-0.5 hover:underline"
                        >
                          <span className="font-medium">
                            {row.user.display_name || row.user.username}
                          </span>
                          <span className="text-xs text-muted-foreground">
                            {row.user.username}
                          </span>
                        </Link>
                      </TableCell>
                      <TableCell>
                        <Badge
                          variant={
                            row.user.role === "admin" ? "default" : "secondary"
                          }
                        >
                          {titleCase(row.user.role)}
                        </Badge>
                        {self ? (
                          <Badge variant="outline" className="ml-1">
                            you
                          </Badge>
                        ) : null}
                      </TableCell>
                      <TableCell>
                        <StatusBadge status={row.user.status} />
                      </TableCell>
                      <TableCell className="tabular-nums">
                        {formatNumber(row.counts.connections)}
                      </TableCell>
                      <TableCell className="tabular-nums">
                        {formatNumber(row.counts.api_keys)}
                      </TableCell>
                      <TableCell className="tabular-nums">
                        {formatNumber(row.usage_30d.requests)}
                        <span className="text-muted-foreground">
                          {" · "}
                          {formatCompact(row.usage_30d.total_tokens)} tok
                        </span>
                      </TableCell>
                      <TableCell className="text-muted-foreground">
                        {formatRelative(row.user.last_login_at)}
                      </TableCell>
                      <TableCell>
                        <div className="flex justify-end gap-1">
                          <LinkButton
                            to={`/admin/users/${row.user.id}`}
                            size="xs"
                            variant="outline"
                          >
                            Manage
                          </LinkButton>
                          <Button
                            type="button"
                            size="icon-xs"
                            variant="ghost"
                            aria-label={suspended ? "Activate" : "Suspend"}
                            disabled={self}
                            onClick={() =>
                              setConfirmRequest({
                                title: suspended
                                  ? `Activate ${row.user.username}?`
                                  : `Suspend ${row.user.username}?`,
                                description: suspended
                                  ? "The account will be able to sign in again."
                                  : "The account is signed out and its API keys stop working.",
                                confirmLabel: suspended
                                  ? "Activate"
                                  : "Suspend",
                                destructive: !suspended,
                                run: async () => {
                                  await api.admin.updateUser(row.user.id, {
                                    status: suspended ? "active" : "suspended",
                                  })
                                  reload()
                                },
                              })
                            }
                          >
                            {suspended ? <CircleCheckIcon /> : <BanIcon />}
                          </Button>
                          <Button
                            type="button"
                            size="icon-xs"
                            variant="ghost"
                            aria-label="Delete"
                            disabled={self}
                            onClick={() =>
                              setConfirmRequest({
                                title: `Delete ${row.user.username}?`,
                                description:
                                  "Their connections, API keys, sessions and usage history are removed with them. This cannot be undone.",
                                confirmLabel: "Delete account",
                                run: async () => {
                                  await api.admin.deleteUser(row.user.id)
                                  reload()
                                },
                              })
                            }
                          >
                            <Trash2Icon />
                          </Button>
                        </div>
                      </TableCell>
                    </TableRow>
                  )
                })}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      <CreateUserDialog
        open={creating}
        onClose={() => setCreating(false)}
        onCreated={(result) => {
          setCreating(false)
          setCreated(result)
          reload()
        }}
      />

      <Dialog
        open={created !== null}
        onClose={() => setCreated(null)}
        title="Account created"
        description={
          created?.password
            ? "Pass these credentials on out of band. The password is not stored in readable form."
            : "The account can sign in with the password you chose."
        }
        footer={
          <Button type="button" onClick={() => setCreated(null)}>
            Done
          </Button>
        }
      >
        <Field label="Username">
          <Copyable value={created?.user.username ?? ""} truncate={false} />
        </Field>
        {created?.password ? (
          <>
            <Field label="Generated password">
              <SecretValue value={created.password} />
            </Field>
            <Alert variant="warning">
              {created.password_note ?? "Shown once."}
            </Alert>
          </>
        ) : null}
      </Dialog>

      <ConfirmDialog
        request={confirmRequest}
        onClose={() => setConfirmRequest(null)}
      />
    </div>
  )
}

function CreateUserDialog({
  open,
  onClose,
  onCreated,
}: {
  open: boolean
  onClose: () => void
  onCreated: (result: CreateUserResponse) => void
}) {
  const [username, setUsername] = React.useState("")
  const [name, setName] = React.useState("")
  const [password, setPassword] = React.useState("")
  const [role, setRole] = React.useState("user")
  const [error, setError] = React.useState<string | null>(null)
  const [pending, setPending] = React.useState(false)

  // Only for the wording of the username rule, so this form and the server can
  // never describe it differently.
  const meta = useAsync(() => api.meta(), [])

  React.useEffect(() => {
    setUsername("")
    setName("")
    setPassword("")
    setRole("user")
    setError(null)
    setPending(false)
  }, [open])

  const handle = username.trim()
  const usernameProblem =
    handle !== "" && !validUsername(handle)
      ? (meta.data?.username_rule ?? "that username is not allowed")
      : null

  const submit = () => {
    setPending(true)
    setError(null)
    api.admin
      .createUser({
        username: handle,
        display_name: name.trim() || undefined,
        password: password || undefined,
        role,
      })
      .then((result) => {
        setPending(false)
        onCreated(result)
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
      title="New user"
      description="Admins start with unlimited quota; ordinary users start with the defaults, which you can change afterwards."
      footer={
        <>
          <Button
            type="button"
            variant="ghost"
            onClick={onClose}
            disabled={pending}
          >
            Cancel
          </Button>
          <Button
            type="button"
            onClick={submit}
            disabled={pending || !validUsername(handle)}
          >
            <PlusIcon />
            Create
          </Button>
        </>
      }
    >
      <ErrorAlert error={error} />
      <Field
        label="Username"
        htmlFor="new-user-username"
        hint={meta.data?.username_rule}
        error={usernameProblem}
      >
        <Input
          id="new-user-username"
          autoComplete="off"
          autoCapitalize="none"
          spellCheck={false}
          aria-invalid={usernameProblem ? true : undefined}
          value={username}
          placeholder="teammate"
          onChange={(event) => setUsername(event.target.value)}
        />
      </Field>
      <Field
        label="Display name"
        hint="Optional. Defaults to the username."
        htmlFor="new-user-name"
      >
        <Input
          id="new-user-name"
          value={name}
          onChange={(event) => setName(event.target.value)}
        />
      </Field>
      <Field
        label="Password"
        hint="Leave empty and the server generates one, shown to you once."
        htmlFor="new-user-password"
      >
        <Input
          id="new-user-password"
          type="password"
          autoComplete="new-password"
          value={password}
          onChange={(event) => setPassword(event.target.value)}
        />
      </Field>
      <Field label="Role" htmlFor="new-user-role">
        <Select
          id="new-user-role"
          value={role}
          onChange={(event) => setRole(event.target.value)}
        >
          <option value="user">User</option>
          <option value="admin">Admin</option>
        </Select>
      </Field>
    </Dialog>
  )
}
