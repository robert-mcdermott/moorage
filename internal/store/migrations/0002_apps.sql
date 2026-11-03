-- Apps, their environments, builds and releases (docs/10-data-model.md).
-- A build is an immutable artifact of an app; a release puts a build into
-- an environment and is numbered per environment.
CREATE TABLE apps (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    created_at INTEGER NOT NULL
) STRICT;

CREATE TABLE builds (
    id            TEXT PRIMARY KEY,
    app_id        TEXT NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    source_type   TEXT NOT NULL,
    artifact_kind TEXT NOT NULL CHECK (artifact_kind IN ('dir', 'image')),
    -- For 'dir', a path relative to the data directory.
    artifact_ref  TEXT NOT NULL,
    -- The resolved plan the build was made from, as JSON.
    plan          TEXT NOT NULL CHECK (json_valid(plan)),
    files         INTEGER NOT NULL,
    size_bytes    INTEGER NOT NULL,
    created_at    INTEGER NOT NULL
) STRICT;

CREATE INDEX builds_app ON builds (app_id);

CREATE TABLE environments (
    id                 TEXT PRIMARY KEY,
    app_id             TEXT NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    name               TEXT NOT NULL,
    kind               TEXT NOT NULL CHECK (kind IN ('production', 'named', 'preview')),
    current_release_id TEXT REFERENCES releases (id),
    created_at         INTEGER NOT NULL,
    UNIQUE (app_id, name)
) STRICT;

CREATE TABLE releases (
    id         TEXT PRIMARY KEY,
    env_id     TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    version    INTEGER NOT NULL,
    build_id   TEXT NOT NULL REFERENCES builds (id),
    reason     TEXT NOT NULL CHECK (reason IN ('deploy', 'rollback', 'config', 'promote', 'restart')),
    created_by TEXT,
    created_at INTEGER NOT NULL,
    UNIQUE (env_id, version)
) STRICT;
