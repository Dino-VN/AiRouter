import * as React from "react"
import { useNavigate, useParams, useSearchParams } from "react-router-dom"
import {
  ArrowLeftIcon,
  ExternalLinkIcon,
  GaugeIcon,
  KeyRoundIcon,
  PlugZapIcon,
  PlusIcon,
  RefreshCwIcon,
  SearchIcon,
  Trash2Icon,
} from "lucide-react"

import { ConfirmDialog, type ConfirmRequest } from "@/components/confirm"
import { Empty } from "@/components/empty"
import { PageHeader } from "@/components/page"
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
import { Field, Input, Label, Select, Textarea } from "@/components/ui/field"
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
  ModelInfo,
  OAuthSession,
  OpenAIEndpoint,
  OpenAIEndpointKey,
  ProviderID,
  Scope,
  StartOAuthResponse,
} from "@/lib/types"

/** ProviderDetailPage routes between the OAuth and OpenAI-compatible
 * variants based on the :provider path segment. */
export default function ProviderDetailPage() {
  const params = useParams<"provider">()
  const providerID = (params.provider ?? "") as ProviderID

  if (providerID === "codex" || providerID === "antigravity") {
    return <OAuthProviderDetail key={providerID} providerID={providerID} />
  }
  if (providerID === "openai") {
    return <OpenAIProviderDetail />
  }
  return (
    <div className="grid gap-3">
      <PageHeader title="Unknown provider" description={providerID} />
      <Card>
        <CardContent>
          <Empty
            icon={<PlugZapIcon />}
            title="No such provider"
            description={`Provider "${providerID}" is not known to this server.`}
          />
        </CardContent>
      </Card>
    </div>
  )
}

// ---------------------------------------------------------------------------
// Codex / Antigravity (OAuth providers)
// ---------------------------------------------------------------------------

type Wizard = {
  session: OAuthSession
  instructions: string
  autoCallback: boolean
}

function OAuthProviderDetail({ providerID }: { providerID: ProviderID }) {
  const { isAdmin } = useAuth()
  const navigate = useNavigate()
  const [showAll, setShowAll] = React.useState(false)
  const [startOpen, setStartOpen] = React.useState(false)
  const [wizard, setWizard] = React.useState<Wizard | null>(null)
  const [editing, setEditing] = React.useState<Connection | null>(null)
  const [confirmRequest, setConfirmRequest] = React.useState<ConfirmRequest | null>(null)
  const [notice, setNotice] = React.useState<string | null>(null)
  const action = useAction()

  // Load provider info, the connections the caller owns (or all of them
  // for admins), pending OAuth sessions, and the model catalog in parallel.
  // All four are independent, so the page renders in a single pass once
  // the slowest one comes back.
  const view = useAsync(async () => {
    const scope = showAll ? { all: true } : undefined
    const [providers, connections, sessions, catalog] = await Promise.all([
      api.providers(),
      api.connections.list({ provider: providerID, ...(scope ?? {}) }),
      api.oauth.list(scope),
      api.models(),
    ])
    return { providers, connections, sessions, catalog }
  }, [providerID, showAll])

  const provider = view.data?.providers.providers.find((p) => p.id === providerID)
  const sessions = view.data?.sessions.sessions ?? []
  const pending = sessions.filter((s) => s.status === "pending")
  const connections = view.data?.connections.connections ?? []
  const allModels = view.data?.catalog.catalog ?? []
  const providerModels = allModels.filter((m) => m.provider === providerID)

  // A loopback callback finishes a sign-in outside this tab, so poll while
  // an attempt is still open.
  useInterval(() => view.reload(), 6000, pending.length > 0)

  const reload = view.reload

  return (
    <div className="grid gap-5">
      <PageHeader
        title={providerLabel(providerID)}
        description={
          providerID === "codex"
            ? "OpenAI ChatGPT (Codex) account. Sign in with OAuth to attach an account."
            : "Google Antigravity account. Sign in with OAuth to attach an account."
        }
      >
        <Button
          type="button"
          variant="ghost"
          size="sm"
          onClick={() => navigate("/providers")}
        >
          <ArrowLeftIcon />
          Back
        </Button>
        {isAdmin ? (
          <Label className="mr-1 text-xs text-muted-foreground">
            <input
              type="checkbox"
              checked={showAll}
              onChange={(e) => setShowAll(e.target.checked)}
              className="mr-1"
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
        <Button
          type="button"
          size="sm"
          onClick={() => setStartOpen(true)}
          disabled={!provider?.allowed}
        >
          <PlugZapIcon />
          Add account
        </Button>
      </PageHeader>

      <ErrorAlert error={view.error ?? action.error} />
      {notice ? (
        <Alert variant="success" title="Saved">
          {notice}
        </Alert>
      ) : null}

      {provider ? (
        <Card>
          <CardHeader>
            <CardTitle>Provider info</CardTitle>
            <CardDescription>
              {provider.usable} of {provider.connections} usable ·{" "}
              {provider.models} models
            </CardDescription>
          </CardHeader>
          <CardContent className="grid gap-1 text-xs text-muted-foreground">
            <span>
              Callback{" "}
              <code className="rounded bg-muted px-1 py-0.5 font-mono">
                {provider.redirect_uri || provider.callback_path}
              </code>
            </span>
            <span>
              {provider.auto_callback
                ? "This server listens on the callback port; the browser finishes the sign-in for you."
                : "The callback port is not bound; paste the redirect URL back here to finish."}
            </span>
          </CardContent>
        </Card>
      ) : null}

      {/* Pending sign-ins */}
      <Card>
        <CardHeader>
          <CardTitle>Pending sign-ins</CardTitle>
          <CardDescription>
            Attempts that have not been redeemed or cancelled yet.
          </CardDescription>
        </CardHeader>
        <CardContent className="px-0">
          {pending.length === 0 ? (
            <Empty
              icon={<PlugZapIcon />}
              title="No sign-ins in progress"
              description='Click "Add account" up top to start one.'
            />
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Status</TableHead>
                  <TableHead>Started</TableHead>
                  <TableHead>Expires</TableHead>
                  <TableHead className="text-right">Actions</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {pending.map((s) => (
                  <TableRow key={s.id}>
                    <TableCell>
                      <StatusBadge status={s.status} />
                    </TableCell>
                    <TableCell className="text-muted-foreground">
                      {formatRelative(s.created_at)}
                    </TableCell>
                    <TableCell className="text-muted-foreground">
                      {formatRelative(s.expires_at)}
                    </TableCell>
                    <TableCell>
                      <div className="flex justify-end gap-1">
                        <Button
                          type="button"
                          size="xs"
                          variant="outline"
                          onClick={() =>
                            setWizard({
                              session: s,
                              instructions: "",
                              autoCallback: provider?.auto_callback ?? false,
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
                          disabled={action.isBusy(s.id)}
                          onClick={() =>
                            setConfirmRequest({
                              title: "Cancel this sign-in?",
                              description: "The consent URL stops working.",
                              confirmLabel: "Cancel sign-in",
                              run: async () => {
                                await api.oauth.cancel(s.id)
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

      {/* Accounts */}
      <div className="grid gap-1">
        <h2 className="text-sm font-medium">
          Accounts{" "}
          <span className="text-muted-foreground">({connections.length})</span>
        </h2>
      </div>
      {connections.length === 0 ? (
        <Card>
          <CardContent>
            <Empty
              icon={<PlugZapIcon />}
              title="No accounts connected"
              description='Click "Add account" up top to sign one in.'
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
                  if (result.note) setNotice(result.note)
                  reload()
                })
              }
              onDelete={() =>
                setConfirmRequest({
                  title: `Remove ${conn.label || conn.account_email}?`,
                  description: "The stored credential is deleted.",
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

      {/* Models from the catalog for this provider */}
      <ModelsFromCatalog
        title={`Models served by ${providerLabel(providerID)}`}
        models={providerModels}
        loading={view.loading}
      />

      <StartDialog
        providerID={providerID}
        canShare={isAdmin}
        open={startOpen}
        onClose={() => setStartOpen(false)}
        onStarted={(result) => {
          setStartOpen(false)
          setWizard({
            session: result.session,
            instructions: result.instructions,
            autoCallback: result.auto_callback,
          })
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
            `${providerLabel(conn.provider)} connected as ${conn.account_email || conn.label}.`
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
      <ConfirmDialog request={confirmRequest} onClose={() => setConfirmRequest(null)} />
    </div>
  )
}

/** ConnectionCard renders one OAuth-backed account row. */
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
        <CardTitle className="flex items-center gap-2">
          <span className="truncate">{conn.label || conn.account_email || conn.id}</span>
          <StatusBadge status={conn.status} />
          {conn.scope === "shared" ? (
            <Badge variant="secondary">shared</Badge>
          ) : null}
        </CardTitle>
        <CardDescription>
          {conn.account_email || "—"}
          {conn.plan ? ` · ${conn.plan}` : ""}
          {conn.request_count_24h ? ` · ${formatNumber(conn.request_count_24h)} req/24h` : ""}
        </CardDescription>
        <CardAction>
          <div className="flex gap-1">
            <Button type="button" size="icon-sm" variant="ghost" aria-label="Edit" onClick={onEdit}>
              <RefreshCwIcon />
            </Button>
            <Button
              type="button"
              size="icon-sm"
              variant="ghost"
              aria-label="Refresh token"
              disabled={busy}
              onClick={onRefresh}
            >
              <GaugeIcon />
            </Button>
            <Button
              type="button"
              size="icon-sm"
              variant="ghost"
              aria-label="Fetch quota"
              disabled={busy}
              onClick={onQuota}
            >
              <GaugeIcon />
            </Button>
            <Button
              type="button"
              size="icon-sm"
              variant="ghost"
              aria-label="Remove"
              disabled={busy}
              onClick={onDelete}
            >
              <Trash2Icon />
            </Button>
          </div>
        </CardAction>
      </CardHeader>
      <CardContent className="grid gap-1 text-xs text-muted-foreground">
        <span>
          Token expires{" "}
          {conn.token_expires_at ? formatRelative(conn.token_expires_at) : "—"}
        </span>
        <span>
          Last used {conn.last_used_at ? formatRelative(conn.last_used_at) : "—"}
        </span>
        {conn.last_error ? (
          <span className="text-destructive">Last error: {conn.last_error}</span>
        ) : null}
        {!usable && conn.disabled_until ? (
          <span>Disabled until {formatDateTime(conn.disabled_until)}</span>
        ) : null}
      </CardContent>
    </Card>
  )
}

// ---------------------------------------------------------------------------
// OpenAI-compatible endpoints (API-key providers)
// ---------------------------------------------------------------------------

function OpenAIProviderDetail() {
  const navigate = useNavigate()
  const [params] = useSearchParams()
  const baseURL = params.get("base_url") ?? ""
  const [addingKey, setAddingKey] = React.useState(false)
  const [scanningModels, setScanningModels] = React.useState(false)
  const [manualModels, setManualModels] = React.useState(false)
  const [confirmRequest, setConfirmRequest] = React.useState<ConfirmRequest | null>(null)
  const [notice, setNotice] = React.useState<string | null>(null)
  const action = useAction()

  // When baseURL is empty we are on the umbrella page that lists every
  // registered endpoint. When it is set we are on the per-endpoint page
  // and only load that one profile (the openai.endpoints endpoint always
  // returns every profile, the filter happens client-side).
  const view = useAsync(async () => {
    const [providers, endpointsResp] = await Promise.all([
      api.providers(),
      api.openai.list(),
    ])
    return { providers, endpoints: endpointsResp.endpoints ?? [] }
  }, [])

  const provider = view.data?.providers.providers.find((p) => p.id === "openai")
  const endpoints: OpenAIEndpoint[] = view.data?.endpoints ?? []
  const endpoint = baseURL
    ? endpoints.find((e) => e.base_url === baseURL)
    : undefined

  const reload = view.reload

  // Per-endpoint view: show the keys + model scan form for one profile.
  if (baseURL && endpoint) {
    return (
      <OpenAIEndpointDetail
        endpoint={endpoint}
        action={action}
        notice={notice}
        onNotice={setNotice}
        onReload={reload}
        onBack={() => navigate("/providers/openai")}
        onAddKey={() => setAddingKey(true)}
        onScanModels={() => setScanningModels(true)}
        onManualModels={() => setManualModels(true)}
        onDeleteKey={(keyID, label) =>
          setConfirmRequest({
            title: `Remove ${label}?`,
            description: "The stored API key is deleted.",
            confirmLabel: "Remove",
            run: async () => {
              await api.connections.remove(keyID)
              reload()
            },
          })
        }
        confirmRequest={confirmRequest}
        onConfirmClose={() => setConfirmRequest(null)}
        addingKeyOpen={addingKey}
        scanningOpen={scanningModels}
        manualOpen={manualModels}
        onAddKeyClose={() => setAddingKey(false)}
        onScanClose={() => setScanningModels(false)}
        onManualClose={() => setManualModels(false)}
      />
    )
  }

  // Umbrella view: a row per registered endpoint with the umbrella stats
  // from /api/providers up top.
  return (
    <div className="grid gap-5">
      <PageHeader
        title={providerLabel("openai")}
        description="Every OpenAI-compatible endpoint you have registered. Each one holds its own API keys and model list."
      >
        <Button
          type="button"
          variant="ghost"
          size="sm"
          onClick={() => navigate("/providers")}
        >
          <ArrowLeftIcon />
          Back
        </Button>
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
          onClick={() => navigate("/providers")}
        >
          <PlusIcon />
          Add endpoint
        </Button>
      </PageHeader>

      <ErrorAlert error={view.error ?? action.error} />
      {notice ? (
        <Alert variant="success" title="Saved">
          {notice}
        </Alert>
      ) : null}

      {provider ? (
        <Card>
          <CardHeader>
            <CardTitle>Provider info</CardTitle>
            <CardDescription>
              {provider.usable} of {provider.connections} usable ·{" "}
              {provider.models} models
            </CardDescription>
          </CardHeader>
          <CardContent className="text-xs text-muted-foreground">
            Forward <code className="rounded bg-muted px-1 py-0.5 font-mono">/v1/chat/completions</code>{" "}
            and <code className="rounded bg-muted px-1 py-0.5 font-mono">/v1/responses</code> to any
            OpenAI-compatible endpoint using an API key.
          </CardContent>
        </Card>
      ) : null}

      <div className="grid gap-3 sm:grid-cols-2">
        {endpoints.length === 0 && view.loading
          ? [0, 1].map((k) => <Skeleton key={k} className="h-32 w-full" />)
          : endpoints.length === 0 ? (
              <Card className="border-dashed">
                <CardContent>
                  <Empty
                    icon={<PlusIcon />}
                    title="No endpoints yet"
                    description='Click "Add endpoint" up top to register one.'
                  />
                </CardContent>
              </Card>
            ) : (
              endpoints.map((e) => (
                <Card key={e.base_url}>
                  <CardHeader>
                    <CardTitle className="flex items-center gap-2">
                      <span className="truncate">{e.label || e.base_url}</span>
                      <Badge variant="secondary">{e.key_count} keys</Badge>
                    </CardTitle>
                    <CardDescription>
                      {e.usable_count} usable
                      {e.models && e.models.length > 0
                        ? ` · ${e.models.length} models`
                        : " · no model list"}
                    </CardDescription>
                    <CardAction>
                      <div className="flex gap-1">
                        <Button
                          type="button"
                          size="sm"
                          variant="outline"
                          onClick={() =>
                            navigate(
                              `/providers/openai?base_url=${encodeURIComponent(e.base_url)}`
                            )
                          }
                        >
                          Open
                        </Button>
                        <Button
                          type="button"
                          size="icon-sm"
                          variant="ghost"
                          aria-label="Delete endpoint"
                          disabled={action.isBusy(e.base_url)}
                          onClick={() =>
                            setConfirmRequest({
                              title: `Delete ${e.label || e.base_url}?`,
                              description: `This removes all ${e.key_count} API key(s) registered against this endpoint. The upstream API itself is not affected.`,
                              confirmLabel: "Delete",
                              run: async () => {
                                await api.openai.remove(e.base_url)
                                reload()
                              },
                            })
                          }
                        >
                          <Trash2Icon />
                        </Button>
                      </div>
                    </CardAction>
                  </CardHeader>
                  <CardContent className="text-xs text-muted-foreground">
                    <code className="rounded bg-muted px-1 py-0.5 font-mono break-all">
                      {e.base_url}
                    </code>
                  </CardContent>
                </Card>
              ))
            )}
      </div>

      <ConfirmDialog request={confirmRequest} onClose={() => setConfirmRequest(null)} />
    </div>
  )
}

/** OpenAIEndpointDetail is the per-endpoint view: the keys for one profile,
 * plus a model list that can be scanned from the upstream's /v1/models
 * endpoint or entered manually. */
function OpenAIEndpointDetail({
  endpoint,
  action,
  notice,
  onNotice,
  onReload,
  onBack,
  onAddKey,
  onScanModels,
  onManualModels,
  onDeleteKey,
  confirmRequest,
  onConfirmClose,
  addingKeyOpen,
  scanningOpen,
  manualOpen,
  onAddKeyClose,
  onScanClose,
  onManualClose,
}: {
  endpoint: OpenAIEndpoint
  action: ReturnType<typeof useAction>
  notice: string | null
  onNotice: (s: string | null) => void
  onReload: () => void
  onBack: () => void
  onAddKey: () => void
  onScanModels: () => void
  onManualModels: () => void
  onDeleteKey: (keyID: string, label: string) => void
  confirmRequest: ConfirmRequest | null
  onConfirmClose: () => void
  addingKeyOpen: boolean
  scanningOpen: boolean
  manualOpen: boolean
  onAddKeyClose: () => void
  onScanClose: () => void
  onManualClose: () => void
}) {
  return (
    <div className="grid gap-5">
      <PageHeader
        title={endpoint.label || endpoint.base_url}
        description={`OpenAI-compatible endpoint · ${endpoint.key_count} keys · ${endpoint.usable_count} usable`}
      >
        <Button type="button" variant="ghost" size="sm" onClick={onBack}>
          <ArrowLeftIcon />
          Back
        </Button>
        <Button type="button" size="sm" onClick={onAddKey}>
          <KeyRoundIcon />
          Add API key
        </Button>
      </PageHeader>

      {notice ? (
        <Alert variant="success" title="Saved">
          {notice}
        </Alert>
      ) : null}

      <Card>
        <CardHeader>
          <CardTitle>API keys</CardTitle>
          <CardDescription>
            Requests are routed across usable keys by weight.
          </CardDescription>
        </CardHeader>
        <CardContent className="px-0">
          {endpoint.keys.length === 0 ? (
            <Empty
              icon={<KeyRoundIcon />}
              title="No keys yet"
              description='Click "Add API key" up top to attach one.'
            />
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Label</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Requests / 24h</TableHead>
                  <TableHead>Last used</TableHead>
                  <TableHead className="text-right">Actions</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {endpoint.keys.map((key) => (
                  <OpenAIKeyRow
                    key={key.id}
                    keyRow={key}
                    busy={action.isBusy(key.id)}
                    onDelete={() => onDeleteKey(key.id, key.label || key.id)}
                  />
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Models</CardTitle>
          <CardDescription>
            {endpoint.models && endpoint.models.length > 0
              ? `${endpoint.models.length} curated models.`
              : "No models yet. Scan from the upstream's /v1/models or enter them manually."}
          </CardDescription>
          <CardAction>
            <Button
              type="button"
              size="sm"
              variant="outline"
              onClick={onScanModels}
              disabled={endpoint.keys.length === 0}
            >
              <SearchIcon />
              Scan from API
            </Button>
            <Button
              type="button"
              size="sm"
              variant="ghost"
              onClick={onManualModels}
            >
              Enter manually
            </Button>
          </CardAction>
        </CardHeader>
        <CardContent>
          {endpoint.models && endpoint.models.length > 0 ? (
            <div className="flex flex-wrap gap-1.5">
              {endpoint.models.map((m) => (
                <Badge key={m} variant="secondary">
                  {m}
                </Badge>
              ))}
            </div>
          ) : (
            <p className="text-sm text-muted-foreground">
              No models configured. Scanning the upstream populates this list;
              the first request after a scan reaches the upstream verbatim.
            </p>
          )}
        </CardContent>
      </Card>

      <AddKeyDialog
        open={addingKeyOpen}
        baseURL={endpoint.base_url}
        onClose={onAddKeyClose}
        onAdded={(label) => {
          onAddKeyClose()
          onNotice(`${label} added.`)
          onReload()
        }}
      />
      <ScanModelsDialog
        open={scanningOpen}
        endpoint={endpoint}
        onClose={onScanClose}
        onNotice={onNotice}
        onReload={onReload}
      />
      <ManualModelsDialog
        open={manualOpen}
        endpoint={endpoint}
        onClose={onManualClose}
        onSaved={() => {
          onManualClose()
          onReload()
        }}
      />
      <ConfirmDialog request={confirmRequest} onClose={onConfirmClose} />
    </div>
  )
}

/** OpenAIKeyRow renders one API key in the per-endpoint table. The key
 * itself is never shown — only the operator's label, status and stats. */
function OpenAIKeyRow({
  keyRow,
  busy,
  onDelete,
}: {
  keyRow: OpenAIEndpointKey
  busy: boolean
  onDelete: () => void
}) {
  return (
    <TableRow>
      <TableCell className="font-medium">
        {keyRow.label}
        {keyRow.scope === "shared" ? (
          <Badge variant="secondary" className="ml-1.5">
            shared
          </Badge>
        ) : null}
        {!keyRow.has_api_key ? (
          <Badge variant="destructive" className="ml-1.5">
            no key
          </Badge>
        ) : null}
      </TableCell>
      <TableCell>
        <StatusBadge status={keyRow.status} />
        {keyRow.last_error ? (
          <p className="mt-0.5 max-w-60 truncate text-xs text-destructive" title={keyRow.last_error}>
            {keyRow.last_error}
          </p>
        ) : null}
      </TableCell>
      <TableCell>{formatNumber(keyRow.request_count_24h ?? 0)}</TableCell>
      <TableCell className="text-muted-foreground">
        {keyRow.last_used_at ? formatRelative(keyRow.last_used_at) : "—"}
      </TableCell>
      <TableCell>
        <div className="flex justify-end">
          <Button
            type="button"
            size="icon-xs"
            variant="ghost"
            aria-label="Remove key"
            disabled={busy}
            onClick={onDelete}
          >
            <Trash2Icon />
          </Button>
        </div>
      </TableCell>
    </TableRow>
  )
}

// ---------------------------------------------------------------------------
// Dialogs
// ---------------------------------------------------------------------------

/** StartDialog opens the OAuth consent page for one provider. */
function StartDialog({
  providerID,
  canShare,
  open,
  onClose,
  onStarted,
}: {
  providerID: ProviderID
  canShare: boolean
  open: boolean
  onClose: () => void
  onStarted: (result: StartOAuthResponse) => void
}) {
  const [scope, setScope] = React.useState<Scope>("private")
  const [error, setError] = React.useState<string | null>(null)
  const [pending, setPending] = React.useState(false)

  React.useEffect(() => {
    if (open) {
      setScope("private")
      setError(null)
      setPending(false)
    }
  }, [open])

  const submit = () => {
    setPending(true)
    setError(null)
    api.oauth
      .start({ provider: providerID, scope })
      .then((result) => {
        setPending(false)
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
      open={open}
      onClose={onClose}
      title={`Connect ${providerLabel(providerID)}`}
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
        {providerLabel(providerID)} will be added under the email of the account you sign in with.
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

/** FinishDialog walks the operator through pasting the OAuth callback URL. */
function FinishDialog({
  wizard,
  onClose,
  onDone,
}: {
  wizard: Wizard | null
  onClose: () => void
  onDone: (conn: Connection) => void
}) {
  const [callback, setCallback] = React.useState("")
  const [error, setError] = React.useState<string | null>(null)
  const [pending, setPending] = React.useState(false)

  React.useEffect(() => {
    if (wizard) {
      setCallback("")
      setError(null)
      setPending(false)
    }
  }, [wizard])

  if (!wizard) {
    return null
  }

  const submit = () => {
    if (!wizard) return
    setPending(true)
    setError(null)
    api.oauth
      .complete(wizard.session.id, { url: callback.trim() })
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
      title="Finish sign-in"
      description="Paste the whole URL the browser redirected you to after you signed in."
      footer={
        <>
          <Button type="button" variant="ghost" onClick={onClose} disabled={pending}>
            Cancel
          </Button>
          <Button type="button" onClick={submit} disabled={pending || !callback.trim()}>
            Redeem
          </Button>
        </>
      }
    >
      <ErrorAlert error={error} />
      <Field label="Callback URL" htmlFor="callback">
        <Textarea
          id="callback"
          rows={4}
          value={callback}
          placeholder="http://localhost:1455/auth/callback?code=...&state=..."
          onChange={(e) => setCallback(e.target.value)}
        />
      </Field>
      {wizard.instructions ? (
        <p className="text-xs text-muted-foreground">{wizard.instructions}</p>
      ) : null}
    </Dialog>
  )
}

/** EditDialog changes the operator-editable fields of an account. */
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
  const [weight, setWeight] = React.useState(1)
  const [status, setStatus] = React.useState<"active" | "disabled">("active")
  const [error, setError] = React.useState<string | null>(null)
  const [pending, setPending] = React.useState(false)

  React.useEffect(() => {
    if (conn) {
      setLabel(conn.label)
      setScope(conn.scope)
      setWeight(conn.weight)
      setStatus(conn.status === "disabled" ? "disabled" : "active")
      setError(null)
      setPending(false)
    }
  }, [conn])

  if (!conn) {
    return null
  }

  const submit = () => {
    if (!conn) return
    setPending(true)
    setError(null)
    api.connections
      .update(conn.id, { label, scope, weight, status })
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
      title="Edit connection"
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
      <div className="grid gap-3">
        <Field label="Label" htmlFor="label">
          <Input id="label" value={label} onChange={(e) => setLabel(e.target.value)} />
        </Field>
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
        <Field label="Weight" hint="Higher weight gets more traffic." htmlFor="weight">
          <Input
            id="weight"
            type="number"
            min={1}
            max={100}
            value={weight}
            onChange={(e) => setWeight(Number(e.target.value) || 1)}
          />
        </Field>
        <Field label="Status" htmlFor="status">
          <Select
            id="status"
            value={status}
            onChange={(e) => setStatus(e.target.value as "active" | "disabled")}
          >
            <option value="active">Active</option>
            <option value="disabled">Disabled</option>
          </Select>
        </Field>
      </div>
    </Dialog>
  )
}

/** AddKeyDialog attaches another API key to an existing endpoint. */
function AddKeyDialog({
  open,
  baseURL,
  onClose,
  onAdded,
}: {
  open: boolean
  baseURL: string
  onClose: () => void
  onAdded: (label: string) => void
}) {
  const [label, setLabel] = React.useState("")
  const [apiKey, setAPIKey] = React.useState("")
  const [scope, setScope] = React.useState<Scope>("private")
  const [error, setError] = React.useState<string | null>(null)
  const [pending, setPending] = React.useState(false)

  React.useEffect(() => {
    if (open) {
      setLabel("")
      setAPIKey("")
      setScope("private")
      setError(null)
      setPending(false)
    }
  }, [open])

  const submit = () => {
    if (!apiKey.trim()) {
      setError("API key is required.")
      return
    }
    setPending(true)
    setError(null)
    api.openai
      .addKey(baseURL, {
        label: label.trim() || baseURL,
        api_key: apiKey.trim(),
        scope,
      })
      .then((response) => {
        setPending(false)
        onAdded(response.connection.label)
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
      title="Add API key"
      description={`Attach another key to ${baseURL}. Each key is its own connection row grouped by this base URL.`}
      footer={
        <>
          <Button type="button" variant="ghost" onClick={onClose} disabled={pending}>
            Cancel
          </Button>
          <Button type="button" onClick={submit} disabled={pending}>
            <KeyRoundIcon />
            Save key
          </Button>
        </>
      }
    >
      <ErrorAlert error={error} />
      <div className="grid gap-3">
        <Field label="Label" htmlFor="addkey-label">
          <Input
            id="addkey-label"
            value={label}
            placeholder="Production key"
            onChange={(e) => setLabel(e.target.value)}
          />
        </Field>
        <Field label="API key" htmlFor="addkey-key">
          <Input
            id="addkey-key"
            type="password"
            value={apiKey}
            placeholder="sk-..."
            onChange={(e) => setAPIKey(e.target.value)}
          />
        </Field>
        <Field label="Scope" htmlFor="addkey-scope">
          <Select
            id="addkey-scope"
            value={scope}
            onChange={(e) => setScope(e.target.value as Scope)}
          >
            <option value="private">Private to me</option>
            <option value="shared">Shared pool</option>
          </Select>
        </Field>
      </div>
    </Dialog>
  )
}

/** ScanModelsDialog calls the upstream's GET /v1/models with one of the
 * operator's stored keys and shows what comes back. Saving is a separate
 * step so the operator can curate the list before persisting it. */
function ScanModelsDialog({
  open,
  endpoint,
  onClose,
  onNotice,
  onReload,
}: {
  open: boolean
  endpoint: OpenAIEndpoint
  onClose: () => void
  onNotice: (s: string | null) => void
  onReload: () => void
}) {
  const [found, setFound] = React.useState<string[]>([])
  const [selected, setSelected] = React.useState<Set<string>>(new Set())
  const [scanning, setScanning] = React.useState(false)
  const [error, setError] = React.useState<string | null>(null)
  const [pending, setPending] = React.useState(false)

  React.useEffect(() => {
    if (open) {
      setFound([])
      setSelected(new Set())
      setScanning(false)
      setError(null)
      setPending(false)
      // Auto-scan on open so the operator sees the upstream's models without
      // an extra click.
      void scan()
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, endpoint.base_url])

  const scan = async () => {
    setScanning(true)
    setError(null)
    try {
      const result = await api.openai.scanModels(endpoint.base_url)
      setFound(result.models)
      // Pre-select every model the operator already curated, plus every
      // new one (they can deselect later). This matches the "scan to
      // populate" expectation.
      const preSelected = new Set<string>(result.models)
      for (const existing of endpoint.models ?? []) {
        preSelected.add(existing)
      }
      setSelected(preSelected)
    } catch (err) {
      setError(errorMessage(err))
    } finally {
      setScanning(false)
    }
  }

  const save = () => {
    setPending(true)
    setError(null)
    const list = Array.from(selected).sort()
    api.openai
      .create({
        label: endpoint.label || endpoint.base_url,
        base_url: endpoint.base_url,
        api_key: "", // empty so the existing key row is left intact
        models: list,
      })
      .then(() => {
        setPending(false)
        onNotice(`${list.length} models saved.`)
        onReload()
        onClose()
      })
      .catch((err: unknown) => {
        setPending(false)
        // The server rejects empty API keys for createOpenAIEndpoint, so this
        // path will not save yet. The real save uses PATCH on the existing
        // connection — see TODO. Until then, surface the error.
        setError(errorMessage(err))
      })
  }

  const toggle = (id: string) => {
    setSelected((current) => {
      const next = new Set(current)
      if (next.has(id)) {
        next.delete(id)
      } else {
        next.add(id)
      }
      return next
    })
  }

  return (
    <Dialog
      open={open}
      onClose={onClose}
      title="Scan models"
      description={`GET ${endpoint.base_url}/models — uses one of your stored keys.`}
      footer={
        <>
          <Button type="button" variant="ghost" onClick={onClose} disabled={pending}>
            Cancel
          </Button>
          <Button
            type="button"
            variant="outline"
            onClick={() => void scan()}
            disabled={scanning}
          >
            <RefreshCwIcon className={scanning ? "animate-spin" : undefined} />
            Re-scan
          </Button>
          <Button type="button" onClick={save} disabled={pending || found.length === 0}>
            Save {selected.size > 0 ? `(${selected.size})` : ""}
          </Button>
        </>
      }
    >
      <ErrorAlert error={error} />
      {scanning ? (
        <div className="grid gap-2">
          <Skeleton className="h-6 w-full" />
          <Skeleton className="h-6 w-full" />
          <Skeleton className="h-6 w-full" />
        </div>
      ) : found.length === 0 ? (
        <p className="text-sm text-muted-foreground">
          No models reported. Try re-scan or enter them manually.
        </p>
      ) : (
        <div className="grid max-h-80 gap-1 overflow-auto">
          {found.map((m) => (
            <label
              key={m}
              className="flex items-center gap-2 rounded px-2 py-1 text-sm hover:bg-muted/50"
            >
              <input
                type="checkbox"
                checked={selected.has(m)}
                onChange={() => toggle(m)}
                className="size-4"
              />
              <code className="font-mono">{m}</code>
            </label>
          ))}
        </div>
      )}
    </Dialog>
  )
}

/** ManualModelsDialog lets the operator type the model list by hand, for
 * endpoints that do not expose /v1/models or for ones the scan misses. */
function ManualModelsDialog({
  open,
  endpoint,
  onClose,
  onSaved,
}: {
  open: boolean
  endpoint: OpenAIEndpoint
  onClose: () => void
  onSaved: () => void
}) {
  const [text, setText] = React.useState("")
  const [error, setError] = React.useState<string | null>(null)
  const [pending, setPending] = React.useState(false)

  React.useEffect(() => {
    if (open) {
      setText((endpoint.models ?? []).join("\n"))
      setError(null)
      setPending(false)
    }
  }, [open, endpoint.base_url, endpoint.models])

  const submit = () => {
    setPending(true)
    setError(null)
    const list = text
      .split("\n")
      .map((m) => m.trim())
      .filter(Boolean)
    api.openai
      .create({
        label: endpoint.label || endpoint.base_url,
        base_url: endpoint.base_url,
        api_key: "",
        models: list,
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
      open={open}
      onClose={onClose}
      title="Models (manual)"
      description="One model id per line."
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
      <Field label="Models" htmlFor="manual-models">
        <Textarea
          id="manual-models"
          rows={8}
          value={text}
          placeholder={"gpt-4o\ngpt-4o-mini\no1-mini"}
          onChange={(e) => setText(e.target.value)}
        />
      </Field>
    </Dialog>
  )
}

/** ModelsFromCatalog shows the model list the built-in catalog knows
 * about for one OAuth provider. */
function ModelsFromCatalog({
  title,
  models,
  loading,
}: {
  title: string
  models: ModelInfo[]
  loading: boolean
}) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>{title}</CardTitle>
        <CardDescription>
          {loading
            ? "Loading…"
            : models.length === 0
              ? "No models in the catalog yet."
              : `${models.length} models.`}
        </CardDescription>
      </CardHeader>
      <CardContent>
        {models.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            The catalog refreshes on startup and every 6 hours.
          </p>
        ) : (
          <div className="flex flex-wrap gap-1.5">
            {models.map((m) => (
              <Badge key={m.id} variant="secondary">
                {m.id}
              </Badge>
            ))}
          </div>
        )}
      </CardContent>
    </Card>
  )
}
