import * as React from "react"
import {
  BanIcon,
  CircleCheckIcon,
  ExternalLinkIcon,
  GaugeIcon,
  KeyRoundIcon,
  PlugZapIcon,
  RefreshCwIcon,
  SettingsIcon,
  ShieldCheckIcon,
  TimerIcon,
  Trash2Icon,
} from "lucide-react"

import { ConfirmDialog, type ConfirmRequest } from "@/components/confirm"
import { Copyable } from "@/components/copyable"
import { Empty } from "@/components/empty"
import { PageHeader } from "@/components/page"
import { UpstreamWindows } from "@/components/quota-meter"
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
import { Field, Input, Label, Select, Switch, Textarea } from "@/components/ui/field"
import { Separator } from "@/components/ui/separator"
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
import { useAction, useAsync, useInterval } from "@/hooks/use-async"
import { api, errorMessage } from "@/lib/api"
import {
  connectionUsable,
  formatDateTime,
  formatNumber,
  formatRelative,
  providerLabel,
} from "@/lib/format"
import type {
  Connection,
  OAuthSession,
  ProviderInfo,
  Scope,
  StartOAuthResponse,
} from "@/lib/types"

type Wizard = {
  session: OAuthSession
  instructions: string
  autoCallback: boolean
}

type APIKeyDraft = {
  provider: ProviderInfo
  label: string
  apiKey: string
  baseURL: string
  plan: string
  accountEmail: string
  models: string // newline-delimited
  extraHeaders: string // newline-delimited "Key: Value"
  quotaNote: string
}

const EMPTY_API_KEY_DRAFT: Omit<APIKeyDraft, "provider"> = {
  label: "",
  apiKey: "",
  baseURL: "https://api.openai.com/v1",
  plan: "",
  accountEmail: "",
  models: "",
  extraHeaders: "",
  quotaNote: "",
}

export default function ProvidersPage() {
  const { isAdmin } = useAuth()
  const [showAll, setShowAll] = React.useState(false)
  const [startFor, setStartFor] = React.useState<ProviderInfo | null>(null)
  const [wizard, setWizard] = React.useState<Wizard | null>(null)
  const [editing, setEditing] = React.useState<Connection | null>(null)
  const [addAPIKeyFor, setAddAPIKeyFor] = React.useState<ProviderInfo | null>(null)
  const [confirmRequest, setConfirmRequest] = React.useState<ConfirmRequest | null>(
    null
  )
  const [notice, setNotice] = React.useState<string | null>(null)
  const action = useAction()

  const view = useAsync(async () => {
    const scope = showAll ? { all: true } : undefined
    const [providers, connections, sessions] = await Promise.all([
      api.providers(),
      api.connections.list(scope),
      api.oauth.list(showAll ? { all: true } : undefined),
    ])
    return { providers, connections, sessions }
  }, [showAll])

  const sessions = view.data?.sessions.sessions ?? []
  const pending = sessions.filter((session) => session.status === "pending")
  const connections = view.data?.connections.connections ?? []
  const providers = view.data?.providers.providers ?? []

  // A loopback callback finishes a sign-in outside this tab, so poll while any
  // attempt is still open.
  useInterval(() => view.reload(), 6000, pending.length > 0)

  // …and when that happens the Finish dialog has to close itself. Leaving it open
  // asks for a code that has already been redeemed, and pasting one then fails
  // with "this attempt is already completed": a sign-in that worked, reported as
  // if it had not. Whatever the outcome, the row in the table below carries the
  // status and any error, so closing loses nothing.
  const attempt = wizard
    ? sessions.find((session) => session.id === wizard.session.id)
    : undefined
  const settled = attempt && attempt.status !== "pending" ? attempt : undefined
  React.useEffect(() => {
    if (!settled) {
      return
    }
    setWizard(null)
    if (settled.status === "completed") {
      setNotice(`${providerLabel(settled.provider)} is connected.`)
    }
  }, [settled])

  const reload = view.reload

  return (
    <div className="grid gap-5">
      <PageHeader
        title="Providers"
        description="Upstream providers this server can route through. Each provider manages its own accounts and models."
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
      </PageHeader>

      <ErrorAlert error={view.error ?? action.error} />
      {notice ? (
        <Alert variant="success" title="Connected">
          {notice}
        </Alert>
      ) : null}

      <div className="grid gap-3 sm:grid-cols-2">
        {providers.length === 0 && view.loading
          ? [0, 1, 2].map((key) => <Skeleton key={key} className="h-28 w-full" />)
          : providers.map((provider) => (
              <Card key={provider.id}>
                <CardHeader>
                  <CardTitle className="flex items-center gap-2">
                    {provider.display_name}
                    {provider.allowed ? null : (
                      <Badge variant="destructive">not allowed</Badge>
                    )}
                  </CardTitle>
                  <CardDescription>
                    {provider.usable} of {provider.connections} usable ·{" "}
                    {provider.models} models
                  </CardDescription>
                  <CardAction>
                    {provider.oauth ? (
                      <Button
                        type="button"
                        size="sm"
                        disabled={!provider.allowed}
                        onClick={() => setStartFor(provider)}
                      >
                        <PlugZapIcon />
                        Connect
                      </Button>
                    ) : (
                      <Button
                        type="button"
                        size="sm"
                        disabled={!provider.allowed}
                        onClick={() => setAddAPIKeyFor(provider)}
                      >
                        <KeyRoundIcon />
                        Add API key
                      </Button>
                    )}
                  </CardAction>
                </CardHeader>
                <CardContent className="grid gap-1 text-xs text-muted-foreground">
                  {provider.oauth ? (
                    <>
                      <span>
                        Callback{" "}
                        <code className="rounded bg-muted px-1 py-0.5 font-mono">
                          {provider.redirect_uri || provider.callback_path}
                        </code>
                      </span>
                      <span>
                        {provider.auto_callback
                          ? "This server is listening on the callback port, so the browser finishes the sign-in for you."
                          : "The callback port is not bound; paste the redirect URL back here to finish."}
                      </span>
                    </>
                  ) : (
                    <span>
                      Forward <code className="rounded bg-muted px-1 py-0.5 font-mono">/v1/chat/completions</code> and{" "}
                      <code className="rounded bg-muted px-1 py-0.5 font-mono">/v1/responses</code> to any
                      OpenAI-compatible endpoint using an API key.
                    </span>
                  )}
                </CardContent>
              </Card>
            ))}
      </div>

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <TimerIcon className="size-4 text-muted-foreground" />
            Temporary connections
          </CardTitle>
          <CardDescription>
            Sign-ins that have been started but not finished. Each one holds a
            state token until it is redeemed, cancelled, or expires
            {view.data ? ` (up to ${view.data.sessions.max_pending} at a time)` : ""}
            .
          </CardDescription>
        </CardHeader>
        <CardContent className="px-0">
          {sessions.length === 0 ? (
            <Empty
              icon={<TimerIcon />}
              title="No sign-ins in progress"
              description="Start one with Connect above; it will appear here until it completes."
            />
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Provider</TableHead>
                  {showAll ? <TableHead>Owner</TableHead> : null}
                  <TableHead>Status</TableHead>
                  <TableHead>Started</TableHead>
                  <TableHead>Expires</TableHead>
                  <TableHead className="text-right">Actions</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {sessions.map((session) => (
                  <TableRow key={session.id}>
                    <TableCell className="font-medium">
                      {providerLabel(session.provider)}
                      {session.target_scope === "shared" ? (
                        <Badge variant="secondary" className="ml-1.5">
                          shared
                        </Badge>
                      ) : null}
                    </TableCell>
                    {showAll ? (
                      <TableCell className="text-muted-foreground">
                        {session.owner_username ?? "—"}
                      </TableCell>
                    ) : null}
                    <TableCell>
                      <StatusBadge status={session.status} />
                      {session.error ? (
                        <p
                          className="mt-0.5 max-w-60 truncate text-xs text-destructive"
                          title={session.error}
                        >
                          {session.error}
                        </p>
                      ) : null}
                    </TableCell>
                    <TableCell className="text-muted-foreground">
                      {formatRelative(session.created_at)}
                    </TableCell>
                    <TableCell className="text-muted-foreground">
                      {formatRelative(session.expires_at)}
                    </TableCell>
                    <TableCell>
                      <div className="flex justify-end gap-1">
                        {session.status === "pending" ? (
                          <>
                            <Button
                              type="button"
                              size="xs"
                              variant="outline"
                              onClick={() =>
                                setWizard({
                                  session,
                                  instructions: "",
                                  autoCallback: false,
                                })
                              }
                            >
                              Finish
                            </Button>
                            <Button
                              type="button"
                              size="icon-xs"
                              variant="ghost"
                              aria-label="Cancel"
                              disabled={action.isBusy(session.id)}
                              onClick={() =>
                                setConfirmRequest({
                                  title: "Cancel this sign-in?",
                                  description:
                                    "The consent URL stops working. You can start a new one at any time.",
                                  confirmLabel: "Cancel sign-in",
                                  run: async () => {
                                    await api.oauth.cancel(session.id)
                                    reload()
                                  },
                                })
                              }
                            >
                              <BanIcon />
                            </Button>
                          </>
                        ) : (
                          <span className="text-xs text-muted-foreground">
                            {session.completed_at
                              ? formatRelative(session.completed_at)
                              : "—"}
                          </span>
                        )}
                      </div>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      <div className="grid gap-1">
        <h2 className="text-sm font-medium">
          Accounts{" "}
          <span className="text-muted-foreground">({connections.length})</span>
        </h2>
        <p className="text-sm text-muted-foreground">
          Requests are routed across the usable accounts of a provider by weight.
        </p>
      </div>

      {connections.length === 0 ? (
        <Card>
          <CardContent>
            <Empty
              icon={<PlugZapIcon />}
              title="No accounts connected"
              description="Connect a Codex or Antigravity account, or add an OpenAI API key."
            />
          </CardContent>
        </Card>
      ) : (
        <div className="grid gap-3 lg:grid-cols-2">
          {connections.map((conn) => (
            <ConnectionCard
              key={conn.id}
              conn={conn}
              busy={action.isBusy(conn.id)}
              onEdit={() => setEditing(conn)}
              onRefresh={() =>
                void action.run(conn.id, async () => {
                  await api.connections.refresh(conn.id)
                  reload()
                })
              }
              onQuota={() =>
                void action.run(conn.id, async () => {
                  const result = await api.connections.fetchQuota(conn.id)
                  if (result.note) {
                    setNotice(result.note)
                  }
                  reload()
                })
              }
              onDelete={() =>
                setConfirmRequest({
                  title: `Remove ${conn.label || conn.account_email}?`,
                  description:
                    "The stored credential is deleted. Requests routed to this account will fail over to the others.",
                  confirmLabel: "Remove",
                  run: async () => {
                    await api.connections.remove(conn.id)
                    reload()
                  },
                })
              }
            />
          ))}
        </div>
      )}

      <StartDialog
        provider={startFor}
        canShare={isAdmin}
        onClose={() => setStartFor(null)}
        onStarted={(result) => {
          setStartFor(null)
          setWizard({
            session: result.session,
            instructions: result.instructions,
            autoCallback: result.auto_callback,
          })
          reload()
        }}
      />

      <AddAPIKeyDialog
        provider={addAPIKeyFor}
        canShare={isAdmin}
        onClose={() => setAddAPIKeyFor(null)}
        onSaved={(conn) => {
          setAddAPIKeyFor(null)
          setNotice(
            `${providerLabel(conn.provider)} connection added as ${conn.label}.`
          )
          reload()
        }}
      />

      <FinishDialog
        wizard={wizard}
        onClose={() => {
          setWizard(null)
          reload()
        }}
        onDone={(conn) => {
          setWizard(null)
          setNotice(
            `${providerLabel(conn.provider)} is connected as ${conn.account_email || conn.label}.`
          )
          reload()
        }}
      />

      <EditDialog
        conn={editing}
        canShare={isAdmin}
        onClose={() => setEditing(null)}
        onSaved={() => {
          setEditing(null)
          reload()
        }}
      />

      <ConfirmDialog
        request={confirmRequest}
        onClose={() => setConfirmRequest(null)}
      />
    </div>
  )
}

// ---------------------------------------------------------------------------
// One account
// ---------------------------------------------------------------------------

function ConnectionCard({
  conn,
  busy,
  onEdit,
  onRefresh,
  onQuota,
  onDelete,
}: {
  conn: Connection
  busy: boolean
  onEdit: () => void
  onRefresh: () => void
  onQuota: () => void
  onDelete: () => void
}) {
  const usable = connectionUsable(conn)
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex flex-wrap items-center gap-1.5">
          <span className="truncate">{conn.label || conn.account_email}</span>
          <Badge variant="outline">{providerLabel(conn.provider)}</Badge>
          <StatusBadge status={conn.status} />
          {conn.scope === "shared" ? (
            <Badge variant="secondary">
              <ShieldCheckIcon />
              shared
            </Badge>
          ) : null}
        </CardTitle>
        <CardDescription className="truncate" title={conn.account_email}>
          {conn.account_email || "unknown account"}
          {conn.plan ? ` · ${conn.plan}` : ""}
          {conn.owner_username ? ` · ${conn.owner_username}` : ""}
        </CardDescription>
      </CardHeader>

      <CardContent className="grid gap-2">
        <dl className="grid grid-cols-2 gap-x-3 gap-y-1 text-xs">
          <div className="flex justify-between gap-2">
            <dt className="text-muted-foreground">Weight</dt>
            <dd className="tabular-nums">{conn.weight}</dd>
          </div>
          <div className="flex justify-between gap-2">
            <dt className="text-muted-foreground">Requests 24h</dt>
            <dd className="tabular-nums">
              {formatNumber(conn.request_count_24h ?? 0)}
            </dd>
          </div>
          <div className="flex justify-between gap-2">
            <dt className="text-muted-foreground">Last used</dt>
            <dd>{formatRelative(conn.last_used_at)}</dd>
          </div>
          <div className="flex justify-between gap-2">
            <dt className="text-muted-foreground">Token expires</dt>
            <dd title={formatDateTime(conn.token_expires_at)}>
              {formatRelative(conn.token_expires_at)}
            </dd>
          </div>
        </dl>

        {!usable && conn.disabled_until ? (
          <p className="text-xs text-amber-600 dark:text-amber-400">
            Cooling down until {formatDateTime(conn.disabled_until)}.
          </p>
        ) : null}
        {conn.last_error ? (
          <p className="text-xs break-words text-destructive">
            {conn.last_error}
          </p>
        ) : null}

        {conn.quota &&
        (conn.quota.windows?.length || conn.quota.credits || conn.quota.note) ? (
          <>
            <Separator />
            <UpstreamWindows quota={conn.quota} />
          </>
        ) : null}

        <Separator />
        <div className="flex flex-wrap items-center gap-1.5">
          <Button
            type="button"
            size="xs"
            variant="outline"
            disabled={busy}
            onClick={onRefresh}
          >
            <RefreshCwIcon className={busy ? "animate-spin" : undefined} />
            Refresh token
          </Button>
          <Button
            type="button"
            size="xs"
            variant="outline"
            disabled={busy}
            onClick={onQuota}
          >
            <GaugeIcon />
            Read quota
          </Button>
          <Button type="button" size="xs" variant="ghost" onClick={onEdit}>
            <SettingsIcon />
            Settings
          </Button>
          <div className="flex-1" />
          <Copyable value={conn.id} display={conn.id.slice(0, 8)} />
          <Button
            type="button"
            size="icon-xs"
            variant="ghost"
            aria-label="Remove"
            onClick={onDelete}
          >
            <Trash2Icon />
          </Button>
        </div>
      </CardContent>
    </Card>
  )
}

// ---------------------------------------------------------------------------
// Dialogs
// ---------------------------------------------------------------------------

function AddAPIKeyDialog({
  provider,
  canShare,
  onClose,
  onSaved,
}: {
  provider: ProviderInfo | null
  canShare: boolean
  onClose: () => void
  onSaved: (conn: Connection) => void
}) {
  const [draft, setDraft] = React.useState<APIKeyDraft>(() => ({
    ...EMPTY_API_KEY_DRAFT,
    provider: provider!,
  }))
  const [scope, setScope] = React.useState<Scope>("private")
  const [weight, setWeight] = React.useState(1)
  const [error, setError] = React.useState<string | null>(null)
  const [pending, setPending] = React.useState(false)

  React.useEffect(() => {
    if (provider) {
      setDraft({ ...EMPTY_API_KEY_DRAFT, provider })
      setScope("private")
      setWeight(1)
      setError(null)
      setPending(false)
    }
  }, [provider])

  if (!provider) {
    return null
  }

  const update = (patch: Partial<APIKeyDraft>) =>
    setDraft((current) => ({ ...current, ...patch }))

  const submit = () => {
    const apiKey = draft.apiKey.trim()
    const label = draft.label.trim()
    if (!apiKey) {
      setError("API key is required.")
      return
    }
    if (!label) {
      setError("Label is required.")
      return
    }
    // Parse newline-delimited "Key: Value" lines into a header map. Empty
    // lines and lines without a colon are dropped with a warning.
    const extraHeaders: Record<string, string> = {}
    for (const line of draft.extraHeaders.split("\\n")) {
      const trimmed = line.trim()
      if (!trimmed) continue
      const [key, ...rest] = trimmed.split(":")
      if (!key || rest.length === 0) {
        setError(`Could not parse header line: ${trimmed}`)
        return
      }
      extraHeaders[key.trim()] = rest.join(":").trim()
    }
    const models = draft.models
      .split("\\n")
      .map((m) => m.trim())
      .filter(Boolean)

    setPending(true)
    setError(null)
    api.connections
      .create({
        provider: provider.id as "openai",
        label,
        api_key: apiKey,
        base_url: draft.baseURL.trim() || undefined,
        plan: draft.plan.trim() || undefined,
        account_email: draft.accountEmail.trim() || undefined,
        scope,
        weight,
        models: models.length > 0 ? models : undefined,
        extra_headers: Object.keys(extraHeaders).length > 0 ? extraHeaders : undefined,
        quota_note: draft.quotaNote.trim() || undefined,
      })
      .then((response) => {
        setPending(false)
        onSaved(response.connection)
      })
      .catch((err: unknown) => {
        setPending(false)
        setError(errorMessage(err))
      })
  }

  return (
    <Dialog
      open={provider !== null}
      onClose={onClose}
      title={`Add ${provider.display_name} API key`}
      description="Register an OpenAI-compatible endpoint with an API key. Works against api.openai.com, Azure OpenAI, OpenRouter, vLLM, LocalAI, etc."
      footer={
        <>
          <Button type="button" variant="ghost" onClick={onClose} disabled={pending}>
            Cancel
          </Button>
          <Button type="button" onClick={submit} disabled={pending}>
            <KeyRoundIcon />
            Save connection
          </Button>
        </>
      }
    >
      <ErrorAlert error={error} />
      <div className="grid gap-3">
        <Field label="Label" hint="A name for this connection." htmlFor="label">
          <Input
            id="label"
            value={draft.label}
            placeholder="My OpenAI key"
            onChange={(e) => update({ label: e.target.value })}
          />
        </Field>
        <Field label="API key" hint="The bearer token sent as Authorization." htmlFor="apiKey">
          <Input
            id="apiKey"
            type="password"
            value={draft.apiKey}
            placeholder="sk-..."
            onChange={(e) => update({ apiKey: e.target.value })}
          />
        </Field>
        <Field
          label="Base URL"
          hint="Defaults to https://api.openai.com/v1. Override for Azure, OpenRouter, vLLM, etc."
          htmlFor="baseURL"
        >
          <Input
            id="baseURL"
            value={draft.baseURL}
            placeholder="https://api.openai.com/v1"
            onChange={(e) => update({ baseURL: e.target.value })}
          />
        </Field>
        <div className="grid gap-3 sm:grid-cols-2">
          <Field label="Plan (optional)" htmlFor="plan">
            <Input
              id="plan"
              value={draft.plan}
              placeholder="paygo, scale, ..."
              onChange={(e) => update({ plan: e.target.value })}
            />
          </Field>
          <Field label="Account email (optional)" htmlFor="accountEmail">
            <Input
              id="accountEmail"
              value={draft.accountEmail}
              onChange={(e) => update({ accountEmail: e.target.value })}
            />
          </Field>
        </div>
        {canShare ? (
          <Field label="Scope" htmlFor="scope">
            <Select
              id="scope"
              value={scope}
              onChange={(e) => setScope(e.target.value as Scope)}
            >
              <option value="private">Private to me</option>
              <option value="shared">Shared pool</option>
            </Select>
          </Field>
        ) : null}
        <Field
          label="Models (optional)"
          hint="One per line. When set, only these models are advertised for this connection."
          htmlFor="models"
        >
          <Textarea
            id="models"
            rows={3}
            value={draft.models}
            placeholder={"gpt-4o\\ngpt-4o-mini"}
            onChange={(e) => update({ models: e.target.value })}
          />
        </Field>
        <Field
          label="Extra headers (optional)"
          hint='One per line as "Header: Value". Useful for OpenAI-Beta, Helicone-Auth, etc.'
          htmlFor="extraHeaders"
        >
          <Textarea
            id="extraHeaders"
            rows={3}
            value={draft.extraHeaders}
            placeholder={'OpenAI-Beta: assistants=v2'}
            onChange={(e) => update({ extraHeaders: e.target.value })}
          />
        </Field>
        <Field
          label="Quota note (optional)"
          hint="Overrides the default message shown when no usage windows are populated yet."
          htmlFor="quotaNote"
        >
          <Input
            id="quotaNote"
            value={draft.quotaNote}
            onChange={(e) => update({ quotaNote: e.target.value })}
          />
        </Field>
      </div>
    </Dialog>
  )
}

function StartDialog({
  provider,
  canShare,
  onClose,
  onStarted,
}: {
  provider: ProviderInfo | null
  canShare: boolean
  onClose: () => void
  onStarted: (result: StartOAuthResponse) => void
}) {
  const [scope, setScope] = React.useState<Scope>("private")
  const [error, setError] = React.useState<string | null>(null)
  const [pending, setPending] = React.useState(false)

  React.useEffect(() => {
    setScope("private")
    setError(null)
    setPending(false)
  }, [provider])

  const submit = () => {
    if (!provider) {
      return
    }
    setPending(true)
    setError(null)
    // No label is sent: the server names the connection after the account that
    // signs in, which is the name the user would have typed anyway. It stays
    // editable afterwards from the connection's own dialog.
    api.oauth
      .start({ provider: provider.id, scope })
      .then((result) => {
        setPending(false)
        // Opening the consent page straight away is what the user expects; if the
        // popup is blocked the URL is still shown in the next dialog.
        window.open(result.session.auth_url, "_blank", "noopener,noreferrer")
        onStarted(result)
      })
      .catch((err: unknown) => {
        setPending(false)
        setError(errorMessage(err))
      })
  }

  return (
    <Dialog
      open={provider !== null}
      onClose={onClose}
      title={`Connect ${provider?.display_name ?? ""}`}
      description="A consent page opens in a new tab. Sign in there, then come back here."
      footer={
        <>
          <Button type="button" variant="ghost" onClick={onClose} disabled={pending}>
            Cancel
          </Button>
          <Button type="button" onClick={submit} disabled={pending}>
            <ExternalLinkIcon />
            Open consent page
          </Button>
        </>
      }
    >
      <ErrorAlert error={error} />
      <p className="text-sm text-muted-foreground">
        {provider?.display_name ?? "The provider"} will be added under the email
        of the account you sign in with. You can rename it later.
      </p>
      {canShare ? (
        <Field
          label="Scope"
          hint="A shared account can be used by every user whose quota allows the shared pool."
          htmlFor="scope"
        >
          <Select
            id="scope"
            value={scope}
            onChange={(event) => setScope(event.target.value as Scope)}
          >
            <option value="private">Private to me</option>
            <option value="shared">Shared pool</option>
          </Select>
        </Field>
      ) : null}
    </Dialog>
  )
}

function FinishDialog({
  wizard,
  onClose,
  onDone,
}: {
  wizard: Wizard | null
  onClose: () => void
  onDone: (conn: Connection) => void
}) {
  const [code, setCode] = React.useState("")
  const [error, setError] = React.useState<string | null>(null)
  const [pending, setPending] = React.useState(false)

  React.useEffect(() => {
    setCode("")
    setError(null)
    setPending(false)
  }, [wizard])

  const submit = () => {
    if (!wizard) {
      return
    }
    const value = code.trim()
    if (!value) {
      setError("Paste the authorization code or the whole redirect URL.")
      return
    }
    setPending(true)
    setError(null)
    api.oauth
      .complete(wizard.session.id, { code: value })
      .then((result) => {
        setPending(false)
        onDone(result.connection)
      })
      .catch((err: unknown) => {
        setPending(false)
        setError(errorMessage(err))
      })
  }

  return (
    <Dialog
      open={wizard !== null}
      onClose={onClose}
      wide
      title={`Finish signing in to ${providerLabel(wizard?.session.provider ?? "")}`}
      description="If the callback page said “Connected”, this is already done — close this and refresh."
      footer={
        <>
          <Button type="button" variant="ghost" onClick={onClose} disabled={pending}>
            Close
          </Button>
          <Button type="button" onClick={submit} disabled={pending}>
            <CircleCheckIcon />
            Finish
          </Button>
        </>
      }
    >
      <ErrorAlert error={error} />
      {wizard?.instructions ? (
        <Alert variant="info">{wizard.instructions}</Alert>
      ) : null}

      <Field label="Consent URL">
        <div className="flex items-center gap-2">
          <Copyable
            value={wizard?.session.auth_url ?? ""}
            className="min-w-0 flex-1"
          />
          <a
            href={wizard?.session.auth_url ?? "#"}
            target="_blank"
            rel="noreferrer"
            className="shrink-0 text-xs text-primary hover:underline"
          >
            Open
          </a>
        </div>
      </Field>

      <Field
        label="Authorization code or redirect URL"
        hint="Paste either the code the provider showed, or the whole http://localhost/... URL your browser landed on."
        htmlFor="code"
      >
        <Textarea
          id="code"
          rows={3}
          value={code}
          spellCheck={false}
          placeholder="http://localhost:1455/auth/callback?code=..."
          onChange={(event) => setCode(event.target.value)}
        />
      </Field>

      {wizard?.autoCallback ? (
        <p className="text-xs text-muted-foreground">
          This server is listening on the provider's callback port, so the sign-in
          usually completes on its own. This form is the fallback.
        </p>
      ) : null}
    </Dialog>
  )
}

function EditDialog({
  conn,
  canShare,
  onClose,
  onSaved,
}: {
  conn: Connection | null
  canShare: boolean
  onClose: () => void
  onSaved: () => void
}) {
  const [label, setLabel] = React.useState("")
  const [scope, setScope] = React.useState<Scope>("private")
  const [weight, setWeight] = React.useState("1")
  const [status, setStatus] = React.useState("active")
  const [error, setError] = React.useState<string | null>(null)
  const [pending, setPending] = React.useState(false)

  React.useEffect(() => {
    setLabel(conn?.label ?? "")
    setScope(conn?.scope ?? "private")
    setWeight(String(conn?.weight ?? 1))
    setStatus(conn?.status === "disabled" ? "disabled" : "active")
    setError(null)
    setPending(false)
  }, [conn])

  const submit = () => {
    if (!conn) {
      return
    }
    const parsed = Number(weight)
    if (!Number.isFinite(parsed) || parsed < 1 || parsed > 100) {
      setError("Weight must be between 1 and 100.")
      return
    }
    setPending(true)
    setError(null)
    api.connections
      .update(conn.id, {
        label: label.trim() || conn.account_email || conn.provider,
        scope,
        weight: Math.round(parsed),
        status: status === "disabled" ? "disabled" : "active",
      })
      .then(() => {
        setPending(false)
        onSaved()
      })
      .catch((err: unknown) => {
        setPending(false)
        setError(errorMessage(err))
      })
  }

  return (
    <Dialog
      open={conn !== null}
      onClose={onClose}
      title="Connection settings"
      description="Weight controls how often this account is picked; disabling keeps it out of rotation without deleting the credential."
      footer={
        <>
          <Button type="button" variant="ghost" onClick={onClose} disabled={pending}>
            Cancel
          </Button>
          <Button type="button" onClick={submit} disabled={pending}>
            Save
          </Button>
        </>
      }
    >
      <ErrorAlert error={error} />
      <Field
        label="Label"
        hint="Clear it to go back to the account email."
        htmlFor="edit-label"
      >
        <Input
          id="edit-label"
          value={label}
          onChange={(event) => setLabel(event.target.value)}
        />
      </Field>
      <div className="grid gap-3 sm:grid-cols-2">
        <Field label="Weight" hint="1 to 100" htmlFor="edit-weight">
          <Input
            id="edit-weight"
            type="number"
            min={1}
            max={100}
            value={weight}
            onChange={(event) => setWeight(event.target.value)}
          />
        </Field>
        <Field label="Status" htmlFor="edit-status">
          <Select
            id="edit-status"
            value={status}
            onChange={(event) => setStatus(event.target.value)}
          >
            <option value="active">Active</option>
            <option value="disabled">Disabled</option>
          </Select>
        </Field>
      </div>
      {canShare ? (
        <Field label="Scope" htmlFor="edit-scope">
          <Select
            id="edit-scope"
            value={scope}
            onChange={(event) => setScope(event.target.value as Scope)}
          >
            <option value="private">Private</option>
            <option value="shared">Shared pool</option>
          </Select>
        </Field>
      ) : null}
    </Dialog>
  )
}
