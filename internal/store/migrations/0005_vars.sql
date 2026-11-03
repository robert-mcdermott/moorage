-- Variables set on the server, and the configuration each release was made
-- with (docs/10-data-model.md). A secret's value is stored only sealed
-- (value_enc), under its app's data key, which the master key seals in
-- turn (docs/08-security.md, "Secrets").
CREATE TABLE data_keys (
    app_id     TEXT PRIMARY KEY REFERENCES apps (id) ON DELETE CASCADE,
    sealed     BLOB NOT NULL,
    created_at INTEGER NOT NULL
) STRICT;

CREATE TABLE vars (
    id         TEXT PRIMARY KEY,
    app_id     TEXT NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    -- NULL: the variable applies to every environment of the app.
    env_id     TEXT REFERENCES environments (id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    value      TEXT,
    value_enc  BLOB,
    secret     INTEGER NOT NULL CHECK (secret IN (0, 1)),
    build      INTEGER NOT NULL CHECK (build IN (0, 1)),
    updated_by TEXT,
    updated_at INTEGER NOT NULL,
    CHECK ((secret = 1 AND value_enc IS NOT NULL AND value IS NULL) OR (secret = 0 AND value IS NOT NULL AND value_enc IS NULL))
) STRICT;

CREATE UNIQUE INDEX vars_name ON vars (app_id, coalesce(env_id, ''), name);

-- The variables a release runs with, as a JSON array: values of secrets are
-- copied sealed, never in the clear.
ALTER TABLE releases ADD COLUMN config TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(config));
