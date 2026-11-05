-- Services are what an app declares and Moorage runs and connects for it, one for each environment (spec/13-services.md).
-- They are not instances: nothing here belongs to a release, as what they hold has to outlast one.
CREATE TABLE services (
    id           TEXT PRIMARY KEY,
    env_id       TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    type         TEXT NOT NULL,
    version      TEXT NOT NULL DEFAULT '',
    image        TEXT NOT NULL,
    digest       TEXT NOT NULL DEFAULT '',
    -- The engine that holds its image and its volume, which it stays with.
    engine       TEXT NOT NULL,
    volume       TEXT NOT NULL,
    port         INTEGER NOT NULL,
    password_enc BLOB NOT NULL,
    -- The variable that gives the app its address.
    env_var      TEXT NOT NULL,
    memory       INTEGER NOT NULL DEFAULT 0,
    -- running, or stopped: what it should be doing, and not what it was last seen doing.
    state        TEXT NOT NULL,
    created_by   TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    UNIQUE (env_id, name)
);
CREATE INDEX services_env ON services (env_id);
