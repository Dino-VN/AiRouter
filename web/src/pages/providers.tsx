import * as React from "react"
import { useNavigate } from "react-router-dom"
import {
  ArrowRightIcon,
  BoxIcon,
  KeyRoundIcon,
  PlugZapIcon,
  PlusIcon,
  RefreshCwIcon,
} from "lucide-react"

import { Empty } from "@/components/empty"
import { PageHeader } from "@/components/page"
import { Alert, ErrorAlert } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
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
import { Field, Input, Label } from "@/components/ui/field"
import { Skeleton } from "@/components/ui/skeleton"
import { useAuth } from "@/context/auth"
import { useAction, useAsync } from "@/hooks/use-async"
import { api, errorMessage } from "@/lib/api"
import { providerLabel } from "@/lib/format"
import type { OpenAIEndpoint, ProviderInfo } from "@/lib/types"

/** ProvidersPage is the launcher: every provider (Codex, Antigravity, and
 * each OpenAI-compatible endpoint the operator has registered) is a card
 * that links through to its own detail page. */
export default function ProvidersPage() {
  const { isAdmin } = useAuth()
  const navigate = useNavigate()
  const [creating, setCreating] = React.useState(false)
  const [notice, setNotice] = React.useState<string | null>(null)
  const action = useAction()

  // Pull both /api/providers (the canonical provider list: Codex +
  // Antigravity + the OpenAI API umbrella) and /api/openai/endpoints (the
  // registered OpenAI-compatible profiles, one per base URL) in parallel
  // so the cards render in a single pass.
  const view = useAsync(async () => {
    const [providers, openai] = await Promise.all([
      api.providers(),
      api.openai.list(),
    ])
    return { providers, openai }
  }, [])

  const providers = view.data?.providers.providers ?? []
  const endpoints = view.data?.openai.endpoints ?? []

  // OAuth providers always render, even when the operator has no
  // account yet: the launcher is also the entry point for adding an
  // Antigravity / Codex account, so hiding a card with zero connections
  // would also hide the "Add account" button that creates the first one.
  // The empty-state messaging on each card itself says "0 of 0 usable",
  // which is enough information for the operator.
  const oauthProviders = providers.filter((p) => p.oauth)
  const openAIProvider = providers.find((p) => p.id === "openai")

  return (
    <div className="grid gap-5">
      <PageHeader
        title="Providers"
        description="Each provider manages its own accounts and models. Pick one to see details."
      >
        <Button
          type="button"
          variant="outline"
          size="sm"
          onClick={view.reload}
          disabled={view.loading}
        >
          <RefreshCwIcon className={view.loading ? "animate-spin" : undefined} />
          Refresh
        </Button>
        <Button
          type="button"
          size="sm"
          onClick={() => setCreating(true)}
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

      {/* OAuth providers first: Codex and Antigravity. These are the
          "fixed" upstreams the server knows how to sign in to via a
          consent flow. */}
      <section className="grid gap-3">
        <div className="grid gap-1">
          <h2 className="text-sm font-medium">OAuth providers</h2>
          <p className="text-sm text-muted-foreground">
            Sign-in flows backed by an upstream OAuth consent screen.
          </p>
        </div>
        <div className="grid gap-3 sm:grid-cols-2">
          {oauthProviders.length === 0 && view.loading
            ? [0, 1].map((key) => (
                <Skeleton key={key} className="h-28 w-full" />
              ))
            : oauthProviders.map((provider) => (
                <ProviderCard
                  key={provider.id}
                  provider={provider}
                  onOpen={() => navigate(`/providers/${provider.id}`)}
                />
              ))}
        </div>
      </section>

      {/* API-key providers: OpenAI-compatible endpoints. Each row is one
          logical profile (name + base URL) holding any number of API
          keys. The umbrella "OpenAI API" card links to a page that lists
          every endpoint; per-endpoint cards open the detail view for that
          base URL. */}
      <section className="grid gap-3">
        <div className="grid gap-1">
          <h2 className="text-sm font-medium">OpenAI-compatible endpoints</h2>
          <p className="text-sm text-muted-foreground">
            Any endpoint that speaks the OpenAI wire format — api.openai.com,
            Azure OpenAI, OpenRouter, vLLM, LocalAI, Ollama, etc.
          </p>
        </div>
        <div className="grid gap-3 sm:grid-cols-2">
          {/* The umbrella card represents the OpenAI API provider as a
              whole: clicking it opens the page listing every endpoint. */}
          {openAIProvider ? (
            <ProviderCard
              provider={openAIProvider}
              onOpen={() => navigate("/providers/openai")}
            />
          ) : null}

          {endpoints.length === 0 && view.loading && openAIProvider ? (
            <Skeleton className="h-28 w-full" />
          ) : (
            endpoints.map((endpoint) => (
              <EndpointCard
                key={endpoint.base_url}
                endpoint={endpoint}
                onOpen={() =>
                  navigate(
                    `/providers/openai?base_url=${encodeURIComponent(endpoint.base_url)}`
                  )
                }
              />
            ))
          )}
          {endpoints.length === 0 && !view.loading ? (
            <Card className="border-dashed">
              <CardContent>
                <Empty
                  icon={<PlusIcon />}
                  title="No endpoints yet"
                  description={`Click "Add endpoint" up top to register your first OpenAI-compatible API.`}
                />
              </CardContent>
            </Card>
          ) : null}
        </div>
        {isAdmin ? (
          <p className="text-xs text-muted-foreground">
            Admins can mark any connection as <Badge variant="secondary">shared</Badge>{" "}
            from the connection's own page so other users can route through it.
          </p>
        ) : null}
      </section>

      <CreateEndpointDialog
        open={creating}
        onClose={() => setCreating(false)}
        onCreated={(endpoint) => {
          setCreating(false)
          setNotice(`${endpoint.label || endpoint.base_url} added.`)
          view.reload()
        }}
      />
    </div>
  )
}

/** ProviderCard renders one fixed OAuth provider (Codex or Antigravity). */
function ProviderCard({
  provider,
  onOpen,
}: {
  provider: ProviderInfo
  onOpen: () => void
}) {
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <PlugZapIcon className="size-4 text-muted-foreground" />
          {providerLabel(provider.id)}
          {provider.allowed ? null : (
            <Badge variant="destructive">not allowed</Badge>
          )}
        </CardTitle>
        <CardDescription>
          {provider.usable} of {provider.connections} usable ·{" "}
          {provider.models} models
        </CardDescription>
        <CardAction>
          <Button
            type="button"
            size="sm"
            variant="outline"
            onClick={onOpen}
            disabled={!provider.allowed}
          >
            Open
            <ArrowRightIcon />
          </Button>
        </CardAction>
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
  )
}

/** EndpointCard renders one registered OpenAI-compatible endpoint. */
function EndpointCard({
  endpoint,
  onOpen,
}: {
  endpoint: OpenAIEndpoint
  onOpen: () => void
}) {
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <BoxIcon className="size-4 text-muted-foreground" />
          {endpoint.label || endpoint.base_url}
        </CardTitle>
        <CardDescription>
          {endpoint.usable_count} of {endpoint.key_count} keys usable
          {endpoint.models && endpoint.models.length > 0
            ? ` · ${endpoint.models.length} curated models`
            : " · no model list yet"}
        </CardDescription>
        <CardAction>
          <Button
            type="button"
            size="sm"
            variant="outline"
            onClick={onOpen}
          >
            Open
            <ArrowRightIcon />
          </Button>
        </CardAction>
      </CardHeader>
      <CardContent className="grid gap-1 text-xs text-muted-foreground">
        <span>
          Base URL{" "}
          <code className="rounded bg-muted px-1 py-0.5 font-mono break-all">
            {endpoint.base_url}
          </code>
        </span>
        <span>
          {endpoint.key_count === 0
            ? "No API keys yet — open to add one."
            : `Last used ${
                endpoint.keys[0]?.last_used_at
                  ? new Date(endpoint.keys[0].last_used_at).toLocaleString()
                  : "—"
              }`}
        </span>
      </CardContent>
    </Card>
  )
}

/** CreateEndpointDialog asks for just a name and a base URL. The first API
 * key is optional; operators who want to bookmark an endpoint and add keys
 * later can leave it blank. */
function CreateEndpointDialog({
  open,
  onClose,
  onCreated,
}: {
  open: boolean
  onClose: () => void
  onCreated: (endpoint: OpenAIEndpoint) => void
}) {
  const [label, setLabel] = React.useState("")
  const [baseURL, setBaseURL] = React.useState("https://api.openai.com/v1")
  const [apiKey, setAPIKey] = React.useState("")
  const [error, setError] = React.useState<string | null>(null)
  const [pending, setPending] = React.useState(false)

  React.useEffect(() => {
    if (open) {
      setLabel("")
      setBaseURL("https://api.openai.com/v1")
      setAPIKey("")
      setError(null)
      setPending(false)
    }
  }, [open])

  const submit = () => {
    const trimmedLabel = label.trim()
    const trimmedBase = baseURL.trim()
    if (!trimmedLabel) {
      setError("Name is required.")
      return
    }
    if (!trimmedBase) {
      setError("Base URL is required.")
      return
    }
    setPending(true)
    setError(null)
    api.openai
      .create({
        label: trimmedLabel,
        base_url: trimmedBase,
        api_key: apiKey.trim() || undefined,
      })
      .then((response) => {
        setPending(false)
        // The server returns a connection row; the launcher cares about
        // enough of it to construct a card-shaped object so the caller
        // can show it without reloading. The full list reload is what
        // the caller does next, so this is best-effort.
        onCreated({
          base_url: trimmedBase,
          label: trimmedLabel,
          keys: [
            {
              id: response.connection.id,
              label: response.connection.label,
              account_email: response.connection.account_email,
              plan: response.connection.plan,
              status: response.connection.status,
              scope: response.connection.scope,
              weight: response.connection.weight,
              has_api_key:
                (response.connection as unknown as { credential?: unknown })
                  .credential !== undefined,
              extra_headers_keys: [],
            },
          ],
          created_at: response.connection.created_at,
          usable_count: 0,
          key_count: 1,
        })
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
      title="Add OpenAI-compatible endpoint"
      description="Register a base URL (api.openai.com, Azure OpenAI, OpenRouter, vLLM, …). You can add API keys and scan models from the next page."
      footer={
        <>
          <Button type="button" variant="ghost" onClick={onClose} disabled={pending}>
            Cancel
          </Button>
          <Button type="button" onClick={submit} disabled={pending}>
            <KeyRoundIcon />
            Save endpoint
          </Button>
        </>
      }
    >
      <ErrorAlert error={error} />
      <div className="grid gap-3">
        <Field label="Name" hint="A short label for this endpoint." htmlFor="endpoint-label">
          <Input
            id="endpoint-label"
            value={label}
            placeholder="My OpenAI key"
            onChange={(e) => setLabel(e.target.value)}
          />
        </Field>
        <Field
          label="Base URL"
          hint="The upstream API base. Defaults to https://api.openai.com/v1."
          htmlFor="endpoint-base"
        >
          <Input
            id="endpoint-base"
            value={baseURL}
            placeholder="https://api.openai.com/v1"
            onChange={(e) => setBaseURL(e.target.value)}
          />
        </Field>
        <Field
          label="API key (optional)"
          hint="Leave blank to add keys later from the endpoint's page."
          htmlFor="endpoint-key"
        >
          <Input
            id="endpoint-key"
            type="password"
            value={apiKey}
            placeholder="sk-..."
            onChange={(e) => setAPIKey(e.target.value)}
          />
        </Field>
        <Label className="text-xs text-muted-foreground">
          Models can be scanned from the upstream's <code>/v1/models</code>{" "}
          endpoint or entered manually on the next page.
        </Label>
      </div>
    </Dialog>
  )
}
