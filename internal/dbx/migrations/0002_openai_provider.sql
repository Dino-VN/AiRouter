-- Allow OpenAI-compatible endpoints as a connection provider.
--
-- The application already supports provider='openai' (API-key based,
-- no OAuth flow — see internal/provider/openai_api.go and
-- internal/model/model.go ProviderOpenAI), but the original 0001
-- schema's CHECK constraint only listed 'codex' and 'antigravity'.
-- Attempts to insert an OpenAI-compatible connection failed with
-- SQLSTATE 23514 'violates check constraint connections_provider_check'.
--
-- Drop and re-create the constraint on both `connections` and
-- `oauth_sessions` so 'openai' is accepted. The oauth_sessions table
-- never actually holds an openai row (the OAuth flow is rejected at
-- the API layer before a session is created), but the constraint
-- is updated anyway so the schema matches the application's view of
-- which providers exist.

ALTER TABLE connections
    DROP CONSTRAINT IF EXISTS connections_provider_check,
    ADD  CONSTRAINT connections_provider_check
        CHECK (provider IN ('codex', 'antigravity', 'openai'));

ALTER TABLE oauth_sessions
    DROP CONSTRAINT IF EXISTS oauth_sessions_provider_check,
    ADD  CONSTRAINT oauth_sessions_provider_check
        CHECK (provider IN ('codex', 'antigravity', 'openai'));
