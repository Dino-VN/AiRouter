# aihub

A self-hosted proxy that puts your **Codex** and **Antigravity** subscriptions behind one
OpenAI-, Anthropic- and Gemini-compatible API — with a real web UI in front of it, accounts
for your team, per-user quotas, and everything kept in PostgreSQL.

It ships as **one executable**. The web UI is compiled into the binary, so deploying to
another machine means copying a single file and giving it a database URL. No Go toolchain,
no Node, no config directory to sync.

```
┌──────────────┐   OpenAI / Anthropic / Gemini      ┌─────────────┐   OAuth   ┌──────────────┐
│ your client  │ ─────────  ah-… API key  ────────► │   aihub     │ ────────► │ Codex        │
│ (SDK, CLI)   │                                    │  (1 binary) │           │ Antigravity  │
└──────────────┘                                    └──────┬──────┘           └──────────────┘
                                                           │
                                                    PostgreSQL (accounts, sealed
                                                    credentials, quotas, usage)
```

## What it does

- **Web UI first.** Login, dashboard, connections, API keys, usage charts and user
  administration are the product, not an afterthought. Served from the same port as the API.
- **Database-backed.** Accounts, connections, quotas and usage live in PostgreSQL. Upstream
  refresh tokens are sealed with AES-256-GCM before they are stored, so a database dump is
  not a credential dump.
- **Multi-tenant.** The first admin creates further accounts; each one signs in to its own
  area with its own connections, API keys, quota and usage history. A connection can be kept
  `private` or published to the `shared` pool for other accounts to route through.
- **Two providers, done properly.** Codex (ChatGPT plans) and Antigravity (Google). Both are
  reached with the vendors' own OAuth flows and refreshed automatically.
- **Temporary connections are visible.** An OAuth attempt is a first-class row: pending
  sign-ins are listed in the UI with their provider, age and expiry, and can be completed or
  cancelled from there instead of leaking as orphaned state.
- **Quotas that mean something.** Requests and tokens per day and per month, maximum
  connections, maximum API keys, allowed providers, allowed models, shared-pool access and a
  concurrency ceiling — per user. Anywhere a limit is `0`, it means unlimited.

## Requirements

- PostgreSQL 14 or newer. A free [Neon](https://neon.tech) database is enough.
- To build: Go 1.25+ and [Bun](https://bun.sh). To *run* a release binary: neither.

## Quick start

```bash
cp .env.example .env
$EDITOR .env                 # set AIHUB_DATABASE_URL

make deps                    # install UI dependencies (once)
make build                   # build the UI, then the binary -> bin/aihub
./bin/aihub
```

Then open <http://localhost:8317>.

Migrations run automatically at startup. The first time you open the UI, the database has no
accounts yet, so it shows a **setup screen**: pick a username and password, and that account
becomes the administrator and is signed in immediately. Usernames are 3–32 characters of
letters, digits, dot, underscore or hyphen, starting and ending with a letter or digit.

That screen is only offered while there are no accounts at all, but anybody who reaches the
port before you do can use it — so finish setup before exposing it, or create the account up
front for an unattended deployment:

```bash
AIHUB_ADMIN_USERNAME=admin AIHUB_ADMIN_PASSWORD='…' ./bin/aihub
```

That runs before the server starts listening, which closes the window entirely. With a
username but no password, one is generated and printed to the log **once**:

```
WARN created the first admin account with a generated password username=admin password=…
```

Forgot it? `./bin/aihub -reset-password 'admin:new-password'`.

### With Docker

```bash
cp .env.example .env         # set AIHUB_DATABASE_URL
docker compose up -d aihub
```

Or, to run PostgreSQL in a container too, point `AIHUB_DATABASE_URL` at
`postgres://aihub:aihub@db:5432/aihub?sslmode=disable` and use
`docker compose --profile local up -d`.

## Connecting a provider

1. Sign in, go to **Connections → Add connection**, pick Codex or Antigravity.
2. Open the authorisation URL it gives you and sign in to the provider.
3. The provider redirects to its own registered loopback URL — Codex to
   `http://localhost:1455/auth/callback?code=…`, Antigravity to
   `http://localhost:51121/oauth-callback?code=…`. The ports and paths belong to the vendors'
   OAuth clients, so they are not configurable.
   - **aihub running on your own machine:** set `AIHUB_LOCAL_OAUTH_LISTENERS=true` and the
     redirect is captured for you — the connection appears by itself.
   - **aihub on a server:** the browser lands on a page that cannot load. Copy the whole URL
     out of the address bar and paste it into the pending connection in the UI.
4. Until it is redeemed the attempt stays in **pending connections**, where you can cancel
   it. Unredeemed attempts expire on their own.

## Using it

Create an API key in the UI (`ah-…`, shown once) and point any SDK at your server.

```bash
# OpenAI-compatible
curl http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer ah-…" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}]}'
```

```python
from openai import OpenAI
client = OpenAI(base_url="http://localhost:8317/v1", api_key="ah-…")
```

```bash
export ANTHROPIC_BASE_URL=http://localhost:8317
export ANTHROPIC_AUTH_TOKEN=ah-…
```

The key is read from `Authorization: Bearer`, `X-Api-Key`, `X-Goog-Api-Key`, `Api-Key` or
`?key=`, so Anthropic and Gemini SDKs work unchanged.

### Endpoints

| Endpoint | Shape |
| --- | --- |
| `POST /v1/chat/completions` | OpenAI Chat Completions |
| `POST /v1/responses` | OpenAI Responses |
| `POST /v1/messages` | Anthropic Messages |
| `POST /v1beta/models/{model}:generateContent` | Gemini |
| `POST /v1beta/models/{model}:streamGenerateContent` | Gemini, streaming |
| `GET /v1/models`, `GET /v1beta/models` | model list, filtered by your quota |

Streaming works on all of them. Requests are translated into a canonical form and back, so
any client shape reaches any provider; the two native pairs (Responses → Codex, Gemini →
Antigravity) pass through with the least rewriting.

Routing is automatic: aihub picks a healthy connection that serves the requested model,
preferring your own over the shared pool. To pin one, send `X-Aihub-Provider: codex` or
`X-Aihub-Connection: <uuid>`.

## Configuration

Everything is environment variables, documented in [.env.example](.env.example). The ones
that matter most:

| Variable | Default | Notes |
| --- | --- | --- |
| `AIHUB_DATABASE_URL` | — | **Required.** `DATABASE_URL` also works. |
| `AIHUB_LISTEN` | `:8317` | Bind address. |
| `AIHUB_ENCRYPTION_KEY` | generated | 32 bytes of hex. Seals provider credentials. |
| `AIHUB_JWT_SECRET` | generated | Signs UI access tokens. |
| `AIHUB_DATA_DIR` | `~/.aihub` | Where generated secrets are kept. |
| `AIHUB_ADMIN_USERNAME` | — | Optional. Skips the setup screen for the first account. |
| `AIHUB_LOCAL_OAUTH_LISTENERS` | `false` | Capture provider redirects locally. |
| `AIHUB_TRUST_PROXY_HEADERS` | `false` | Enable only behind your own reverse proxy. |
| `AIHUB_ANTIGRAVITY_FILTER_MODE` | `off` | Screen requests bound for the Antigravity upstream for non-Antigravity coding-client names inside the JSON `system` field. `block` rejects with HTTP 403, `rewrite` replaces the names with "Antigravity". |
| `AIHUB_ANTIGRAVITY_FILTER_USE_DEFAULT_KEYWORDS` | `true` | Toggle the built-in preset (Claude Code, Codex, Cursor, Windsurf, Cline, Aider, etc.). |
| `AIHUB_ANTIGRAVITY_FILTER_CUSTOM_MAPPINGS` | — | Extra `from: to` pairs, comma- or newline-delimited. |

> **Back up the encryption key.** It is generated on first start and written to
> `$AIHUB_DATA_DIR/encryption.key`. Lose it and every stored provider credential becomes
> unreadable — every connection has to be authorised again. In a container, set
> `AIHUB_ENCRYPTION_KEY` explicitly or persist `/data`.

## Antigravity coding filter

aihub can screen requests bound for the Antigravity upstream for non-Antigravity
coding-client names inside the JSON `system` field. The filter is a native Go
port of the [cpa-plugin-antigravity-coding-filter](https://github.com/jellyfish-p/cpa-plugin-antigravity-coding-filter)
plugin — it ships inside the binary, so no plugin runtime is needed.

| Mode | Behaviour |
| --- | --- |
| `off` (default) | No filtering; every request passes through. |
| `block` | Reject matching requests with HTTP 403 and an error of type `blocked_by_antigravity_coding_filter`. |
| `rewrite` | Replace matched names with `Antigravity` and forward the rewritten request. |

Matching is case-insensitive and only scans JSON fields named `system`. Mentions
in `messages`, user prompts, or any other field do **not** trigger the filter.

### Configuration

```bash
# .env
AIHUB_ANTIGRAVITY_FILTER_MODE=block
AIHUB_ANTIGRAVITY_FILTER_USE_DEFAULT_KEYWORDS=true
AIHUB_ANTIGRAVITY_FILTER_CUSTOM_MAPPINGS=Cursor: Antigravity, Windsurf: Antigravity
```

The built-in preset covers the major AI coding editors, terminal agents and
general-purpose coding agents: Claude Code, OpenAI Codex / Codex CLI, OpenCode,
GitHub Copilot / Copilot CLI, Gemini Code Assist / Gemini CLI, Cursor, Windsurf
/ Codeium, Cline, Roo Code, Kilo Code, Aider, Continue.dev, Amazon Q Developer /
CodeWhisperer, JetBrains AI Assistant / Junie, Kiro, Qoder / Qoder CLI, Qwen Code,
Trae, Tabnine, Sourcegraph Cody, Augment Code, Replit Agent / Ghostwriter, Devin,
OpenHands, SWE-agent, Goose, Zed AI, Void Editor, PearAI, Refact.ai, Tabby,
GitLab Duo, Visual Studio IntelliCode, CodeBuddy, Blackbox AI, Pieces for
Developers, Qodo / CodiumAI, Rovo Dev CLI, Factory Droid, OpenClaw (incl. Clawdbot
and Moltbot), Hermes Agent and WorkBuddy.

Disable `AIHUB_ANTIGRAVITY_FILTER_USE_DEFAULT_KEYWORDS` and supply your own
keyword list via `AIHUB_ANTIGRAVITY_FILTER_CUSTOM_MAPPINGS` to override the
preset entirely.

## Deploying elsewhere

`make release` cross-compiles stripped binaries into `dist/` for Linux, macOS and Windows.
Copy one, plus a `.env` holding `AIHUB_DATABASE_URL`, and run it — the UI is inside.

```bash
make release
scp dist/aihub-0.1.0-linux-amd64 server:/usr/local/bin/aihub
```

Put it behind a reverse proxy for TLS. Disable response buffering (nginx:
`proxy_buffering off;`) or streaming replies will arrive in one lump, and set
`AIHUB_TRUST_PROXY_HEADERS=true` so the login throttle sees real client addresses.

## Development

```bash
make migrate     # apply migrations and exit
make dev         # API on :8317, using whatever UI is already embedded
make ui-dev      # Vite dev server with hot reload, proxying /api to :8317
make check       # fmt + vet + test
make help        # every target
```

Layout:

```
cmd/aihub/          entry point and flags
internal/config/    environment configuration
internal/dbx/       pool and embedded SQL migrations
internal/model/     domain types
internal/store/     every database query
internal/cryptobox/ AES-256-GCM sealing for stored credentials
internal/authn/     password hashing, JWT access tokens, refresh sessions
internal/provider/  Codex and Antigravity: OAuth, refresh, quota, models
internal/proxy/     canonical request/response IR and per-format translation
internal/httpapi/   routing, the /api surface, the /v1 and /v1beta proxy
internal/webui/     go:embed of the built UI
web/                the UI itself (Vite + React + shadcn/ui)
```

`internal/webui/dist` is where the UI build lands and what gets embedded. The repository
holds a placeholder `index.html` there so `go build ./...` works on a fresh clone; the
server logs a warning at startup if that placeholder is still what is embedded. Run
`make ui` to replace it.

## Notes

- Provider credentials belong to the account that authorised them. An admin can see that a
  connection exists and can delete it, but the sealed tokens are never exposed through the
  API — not to the owner either.
- Quota counters are read from committed usage rows rather than under a lock, so a burst of
  simultaneous requests can overshoot a limit slightly. That is deliberate: it keeps a
  database write off the critical path of every proxied call.
- The concurrency limit is per process. Behind several replicas, each enforces its own share.
- Usage history is pruned after `AIHUB_USAGE_RETENTION_DAYS` (default 90).

## Prior art

Inspired by [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI). This is an
independent implementation, not a fork: it keeps files out of the loop entirely, treats the
web UI and multi-tenancy as the primary interface, and limits itself to the two providers
above so that both can be supported properly.
