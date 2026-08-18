import * as React from "react"
import {
  BanIcon,
  KeyRoundIcon,
  PlusIcon,
  RefreshCwIcon,
  Trash2Icon,
} from "lucide-react"

import { ConfirmDialog, type ConfirmRequest } from "@/components/confirm"
import { Copyable, SecretValue } from "@/components/copyable"
import { Empty } from "@/components/empty"
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
import { Field, Input, Label, Select, Switch } from "@/components/ui/field"
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
import { useAction, useAsync } from "@/hooks/use-async"
import { api, errorMessage } from "@/lib/api"
import { formatDateTime, formatNumber, formatRelative } from "@/lib/format"
import type { APIKey, ModelInfo } from "@/lib/types"

export default function ApiKeysPage() {
  const { isAdmin } = useAuth()
  const [showAll, setShowAll] = React.useState(false)
  const [creating, setCreating] = React.useState(false)
  const [created, setCreated] = React.useState<APIKey | null>(null)
  const [confirmRequest, setConfirmRequest] =
    React.useState<ConfirmRequest | null>(null)
  const action = useAction()

  const view = useAsync(async () => {
    const [keys, catalog] = await Promise.all([
      api.keys.list(showAll ? { all: true } : undefined),
      api.models(),
    ])
    return { keys, catalog }
  }, [showAll])

  const keys = view.data?.keys.keys ?? []
  // The models the caller can actually reach, falling back to the whole catalog
  // for an account that has no connection yet. Both arrive as null on a fresh
  // install, so neither is dereferenced without a default.
  const reachable = view.data?.catalog.models ?? []
  const models: ModelInfo[] =
    reachable.length > 0 ? reachable : (view.data?.catalog.catalog ?? [])
  const maxKeys = view.data?.keys.max_keys ?? 0
  const reload = view.reload

  return (
    <div className="grid gap-5">
      <PageHeader
        title="API keys"
        description="Keys clients use to reach this server. The secret is shown once, when the key is created."
      >
        {isAdmin ? (
          <Label className="mr-1 text-xs text-muted-foreground">
            <Switch
              checked={showAll}
              onChange={(event) => setShowAll(event.target.checked)}
            />
            Every user
          </Label>
        ) : null}
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
          <PlusIcon />
          New key
        </Button>
      </PageHeader>

      <ErrorAlert error={view.error ?? action.error} />

      <Card>
        <CardHeader>
          <CardTitle>
            Keys{" "}
            <span className="text-muted-foreground">
              ({keys.length}
              {maxKeys > 0 ? ` of ${maxKeys}` : ""})
            </span>
          </CardTitle>
          <CardDescription>
            Send one as <code className="font-mono">Authorization: Bearer …</code>
            , <code className="font-mono">x-api-key</code>, or{" "}
            <code className="font-mono">?key=</code> for the Gemini routes.
          </CardDescription>
        </CardHeader>
        <CardContent className="px-0">
          {view.loading && keys.length === 0 ? (
            <SkeletonRows rows={3} cols={5} />
          ) : keys.length === 0 ? (
            <Empty
              icon={<KeyRoundIcon />}
              title="No keys yet"
              description="Create one to point a client at this server."
            >
              <Button type="button" size="sm" onClick={() => setCreating(true)}>
                <PlusIcon />
                New key
              </Button>
            </Empty>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Name</TableHead>
                  <TableHead>Prefix</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Models</TableHead>
                  <TableHead>Requests</TableHead>
                  <TableHead>Last used</TableHead>
                  <TableHead>Expires</TableHead>
                  <TableHead className="text-right">Actions</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {keys.map((key) => (
                  <TableRow key={key.id}>
                    <TableCell className="font-medium">{key.name}</TableCell>
                    <TableCell>
                      <Copyable value={key.prefix} truncate={false} />
                    </TableCell>
                    <TableCell>
                      <StatusBadge status={key.status} />
                    </TableCell>
                    <TableCell>
                      {key.allowed_models && key.allowed_models.length > 0 ? (
                        <span
                          className="text-xs"
                          title={key.allowed_models.join(", ")}
                        >
                          {key.allowed_models.length} allowed
                        </span>
                      ) : (
                        <Badge variant="muted">all</Badge>
                      )}
                    </TableCell>
                    <TableCell className="tabular-nums">
                      {formatNumber(key.request_count ?? 0)}
                    </TableCell>
                    <TableCell className="text-muted-foreground">
                      {formatRelative(key.last_used_at)}
                    </TableCell>
                    <TableCell
                      className="text-muted-foreground"
                      title={formatDateTime(key.expires_at)}
                    >
                      {key.expires_at ? formatRelative(key.expires_at) : "never"}
                    </TableCell>
                    <TableCell>
                      <div className="flex justify-end gap-1">
                        {key.status === "active" ? (
                          <Button
                            type="button"
                            size="xs"
                            variant="outline"
                            disabled={action.isBusy(key.id)}
                            onClick={() =>
                              setConfirmRequest({
                                title: `Revoke “${key.name}”?`,
                                description:
                                  "Clients using this key start failing immediately. Its usage history is kept.",
                                confirmLabel: "Revoke",
                                run: async () => {
                                  await api.keys.revoke(key.id)
                                  reload()
                                },
                              })
                            }
                          >
                            <BanIcon />
                            Revoke
                          </Button>
                        ) : null}
                        <Button
                          type="button"
                          size="icon-xs"
                          variant="ghost"
                          aria-label="Delete"
                          onClick={() =>
                            setConfirmRequest({
                              title: `Delete “${key.name}”?`,
                              description:
                                "The key row is removed. Usage records keep pointing at it by id.",
                              confirmLabel: "Delete",
                              run: async () => {
                                await api.keys.remove(key.id)
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
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      <CreateKeyDialog
        open={creating}
        models={models}
        onClose={() => setCreating(false)}
        onCreated={(key) => {
          setCreating(false)
          setCreated(key)
          reload()
        }}
      />

      <NewKeyDialog keyValue={created} onClose={() => setCreated(null)} />

      <ConfirmDialog
        request={confirmRequest}
        onClose={() => setConfirmRequest(null)}
      />
    </div>
  )
}

function CreateKeyDialog({
  open,
  models,
  onClose,
  onCreated,
}: {
  open: boolean
  models: ModelInfo[]
  onClose: () => void
  onCreated: (key: APIKey) => void
}) {
  const [name, setName] = React.useState("")
  const [days, setDays] = React.useState("0")
  const [allowed, setAllowed] = React.useState<string[]>([])
  const [error, setError] = React.useState<string | null>(null)
  const [pending, setPending] = React.useState(false)

  React.useEffect(() => {
    setName("")
    setDays("0")
    setAllowed([])
    setError(null)
    setPending(false)
  }, [open])

  const toggle = (id: string) => {
    setAllowed((current) =>
      current.includes(id)
        ? current.filter((value) => value !== id)
        : [...current, id]
    )
  }

  const submit = () => {
    setPending(true)
    setError(null)
    const expires = Number(days)
    api.keys
      .create({
        name: name.trim(),
        allowed_models: allowed.length > 0 ? allowed : undefined,
        expires_in_days: expires > 0 ? expires : undefined,
      })
      .then((result) => {
        setPending(false)
        onCreated(result.key)
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
      wide
      title="New API key"
      description="The secret is returned once and stored only as a hash."
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
          <Button type="button" onClick={submit} disabled={pending}>
            Create key
          </Button>
        </>
      }
    >
      <ErrorAlert error={error} />
      <Field
        label="Name"
        hint="Something you will recognise later, like “laptop cli”."
        htmlFor="key-name"
      >
        <Input
          id="key-name"
          value={name}
          placeholder="laptop cli"
          onChange={(event) => setName(event.target.value)}
        />
      </Field>
      <Field label="Expires" htmlFor="key-days">
        <Select
          id="key-days"
          value={days}
          onChange={(event) => setDays(event.target.value)}
        >
          <option value="0">Never</option>
          <option value="7">In 7 days</option>
          <option value="30">In 30 days</option>
          <option value="90">In 90 days</option>
          <option value="365">In a year</option>
        </Select>
      </Field>
      <Field
        label="Allowed models"
        hint="Leave everything unchecked to allow every model this account can reach."
      >
        <div className="grid max-h-52 gap-1 overflow-y-auto rounded-lg border border-border p-2">
          {models.length === 0 ? (
            <p className="p-1 text-xs text-muted-foreground">
              No models in the catalog yet.
            </p>
          ) : (
            models.map((model) => (
              <Label
                key={model.id}
                className="rounded px-1 py-0.5 text-xs font-normal hover:bg-muted"
              >
                <Switch
                  checked={allowed.includes(model.id)}
                  onChange={() => toggle(model.id)}
                />
                <span className="truncate font-mono">{model.id}</span>
                {model.provider ? (
                  <Badge variant="muted">{model.provider}</Badge>
                ) : null}
              </Label>
            ))
          )}
        </div>
      </Field>
    </Dialog>
  )
}

function NewKeyDialog({
  keyValue,
  onClose,
}: {
  keyValue: APIKey | null
  onClose: () => void
}) {
  const secret = keyValue?.secret ?? ""
  const origin = typeof window === "undefined" ? "" : window.location.origin

  return (
    <Dialog
      open={keyValue !== null}
      onClose={onClose}
      wide
      title="Key created"
      description="Copy it now. It is stored only as a hash and cannot be shown again."
      footer={
        <Button type="button" onClick={onClose}>
          Done
        </Button>
      }
    >
      <SecretValue value={secret} />
      <Alert variant="warning">
        This is the only time this secret is visible.
      </Alert>
      <Field label="Try it" hint="An OpenAI-compatible request against this server.">
        <pre className="overflow-x-auto rounded-lg border border-border bg-muted/40 p-2.5 font-mono text-xs">
          {`curl ${origin}/v1/chat/completions \\
  -H "Authorization: Bearer ${secret}" \\
  -H "Content-Type: application/json" \\
  -d '{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}]}'`}
        </pre>
      </Field>
    </Dialog>
  )
}
