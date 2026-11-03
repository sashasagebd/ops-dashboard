-- An app's environment variables, injected when its container starts.
-- Values are encrypted (AES-256-GCM, see internal/secrets) with a key that
-- lives only in the platform's environment, so this table alone reveals
-- nothing. Each value is bound to its app and key name, so a value copied
-- into another row won't decrypt.
CREATE TABLE app_env_vars (
    app_id bigint NOT NULL REFERENCES apps (id) ON DELETE CASCADE,

    -- The usual shell variable shape, which every runtime accepts.
    key text NOT NULL
        CHECK (key ~ '^[A-Za-z_][A-Za-z0-9_]{0,255}$'),

    encrypted_value bytea NOT NULL,
    updated_at      timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (app_id, key)
);
