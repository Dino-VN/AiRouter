-- Initial schema for aihub: multi-tenant proxy for Codex and Antigravity.

CREATE TABLE users (
    id            uuid PRIMARY KEY,
    username      text NOT NULL,
    password_hash text NOT NULL,
    display_name  text NOT NULL DEFAULT '',
    role          text NOT NULL DEFAULT 'user',
    status        text NOT NULL DEFAULT 'active',
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    last_login_at timestamptz,
    CONSTRAINT users_role_check   CHECK (role IN ('admin', 'user')),
    -- 'revoked' is a permanent lockout; 'suspended' is meant to be reversible.
    -- Both are accepted by the API, so both must be accepted here.
    CONSTRAINT users_status_check CHECK (status IN ('active', 'suspended', 'revoked')),
    -- The API enforces the full handle rules (length, allowed characters). This
    -- is only a backstop guaranteeing the column can serve as a login handle.
    CONSTRAINT users_username_check CHECK (username <> '' AND username !~ '\s')
);

CREATE UNIQUE INDEX users_username_key ON users (lower(username));

CREATE TABLE user_quotas (
    user_id            uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    requests_per_day   bigint NOT NULL DEFAULT 0,
    tokens_per_day     bigint NOT NULL DEFAULT 0,
    requests_per_month bigint NOT NULL DEFAULT 0,
    tokens_per_month   bigint NOT NULL DEFAULT 0,
    max_connections    integer NOT NULL DEFAULT 0,
    max_api_keys       integer NOT NULL DEFAULT 0,
    allowed_providers  text[] NOT NULL DEFAULT '{}',
    allow_shared_pool  boolean NOT NULL DEFAULT true,
    concurrent_limit   integer NOT NULL DEFAULT 0,
    updated_at         timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE connections (
    id               uuid PRIMARY KEY,
    owner_id         uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    provider         text NOT NULL,
    label            text NOT NULL DEFAULT '',
    account_email    text NOT NULL DEFAULT '',
    account_id       text NOT NULL DEFAULT '',
    project_id       text NOT NULL DEFAULT '',
    plan             text NOT NULL DEFAULT '',
    status           text NOT NULL DEFAULT 'active',
    scope            text NOT NULL DEFAULT 'private',
    weight           integer NOT NULL DEFAULT 1,
    secret           bytea NOT NULL,
    metadata         jsonb,
    quota            jsonb,
    quota_updated_at timestamptz,
    disabled_until   timestamptz,
    last_error       text NOT NULL DEFAULT '',
    last_used_at     timestamptz,
    token_expires_at timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT connections_provider_check CHECK (provider IN ('codex', 'antigravity')),
    CONSTRAINT connections_scope_check    CHECK (scope IN ('private', 'shared')),
    CONSTRAINT connections_status_check   CHECK (status IN ('active', 'expired', 'disabled', 'error'))
);

CREATE INDEX connections_owner_idx ON connections (owner_id);
CREATE INDEX connections_provider_idx ON connections (provider, status);
-- The same upstream account must not be registered twice for one owner.
CREATE UNIQUE INDEX connections_owner_account_key
    ON connections (owner_id, provider, lower(account_email))
    WHERE account_email <> '';

CREATE TABLE api_keys (
    id             uuid PRIMARY KEY,
    user_id        uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name           text NOT NULL DEFAULT '',
    prefix         text NOT NULL,
    key_hash       text NOT NULL UNIQUE,
    status         text NOT NULL DEFAULT 'active',
    allowed_models text[] NOT NULL DEFAULT '{}',
    expires_at     timestamptz,
    last_used_at   timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT api_keys_status_check CHECK (status IN ('active', 'revoked'))
);

CREATE INDEX api_keys_user_idx ON api_keys (user_id);

CREATE TABLE usage_records (
    id                bigserial PRIMARY KEY,
    created_at        timestamptz NOT NULL DEFAULT now(),
    user_id           uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    api_key_id        uuid REFERENCES api_keys (id) ON DELETE SET NULL,
    connection_id     uuid REFERENCES connections (id) ON DELETE SET NULL,
    provider          text NOT NULL DEFAULT '',
    model             text NOT NULL DEFAULT '',
    client_format     text NOT NULL DEFAULT '',
    status_code       integer NOT NULL DEFAULT 0,
    stream            boolean NOT NULL DEFAULT false,
    prompt_tokens     bigint NOT NULL DEFAULT 0,
    completion_tokens bigint NOT NULL DEFAULT 0,
    reasoning_tokens  bigint NOT NULL DEFAULT 0,
    cached_tokens     bigint NOT NULL DEFAULT 0,
    total_tokens      bigint NOT NULL DEFAULT 0,
    latency_ms        bigint NOT NULL DEFAULT 0,
    error             text NOT NULL DEFAULT ''
);

CREATE INDEX usage_records_user_created_idx ON usage_records (user_id, created_at DESC);
CREATE INDEX usage_records_created_idx ON usage_records (created_at DESC);
CREATE INDEX usage_records_connection_idx ON usage_records (connection_id, created_at DESC);

CREATE TABLE oauth_sessions (
    id            uuid PRIMARY KEY,
    user_id       uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    provider      text NOT NULL,
    state         text NOT NULL UNIQUE,
    code_verifier text NOT NULL DEFAULT '',
    redirect_uri  text NOT NULL DEFAULT '',
    auth_url      text NOT NULL DEFAULT '',
    label         text NOT NULL DEFAULT '',
    target_scope  text NOT NULL DEFAULT 'private',
    status        text NOT NULL DEFAULT 'pending',
    error         text NOT NULL DEFAULT '',
    connection_id uuid REFERENCES connections (id) ON DELETE SET NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    completed_at  timestamptz,
    CONSTRAINT oauth_sessions_provider_check CHECK (provider IN ('codex', 'antigravity')),
    CONSTRAINT oauth_sessions_status_check
        CHECK (status IN ('pending', 'completed', 'failed', 'cancelled', 'expired'))
);

CREATE INDEX oauth_sessions_user_idx ON oauth_sessions (user_id, created_at DESC);
CREATE INDEX oauth_sessions_status_idx ON oauth_sessions (status, expires_at);

CREATE TABLE web_sessions (
    id         uuid PRIMARY KEY,
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash text NOT NULL UNIQUE,
    user_agent text NOT NULL DEFAULT '',
    ip         text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz
);

CREATE INDEX web_sessions_user_idx ON web_sessions (user_id);

CREATE TABLE settings (
    key        text PRIMARY KEY,
    value      jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
