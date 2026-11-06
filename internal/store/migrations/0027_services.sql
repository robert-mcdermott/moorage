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
    -- Where it is mounted, which depends on the image that made it.
    mount        TEXT NOT NULL DEFAULT '',
    -- What it held before an upgrade or a restore, and what ran on it, kept until someone says that that was good.
    old_volume   TEXT NOT NULL DEFAULT '',
    old_version  TEXT NOT NULL DEFAULT '',
    old_image    TEXT NOT NULL DEFAULT '',
    old_digest   TEXT NOT NULL DEFAULT '',
    old_mount    TEXT NOT NULL DEFAULT '',
    port         INTEGER NOT NULL,
    password_enc BLOB NOT NULL,
    -- The variable that gives the app its address.
    env_var      TEXT NOT NULL,
    memory       INTEGER NOT NULL DEFAULT 0,
    -- running, or stopped: what it should be doing, and not what it was last seen doing. seeding is one that has not finished being made.
    state        TEXT NOT NULL,
    -- Its volume holds nothing that should be kept, until what its dump holds has been put back in it.
    pending_load INTEGER NOT NULL DEFAULT 0,
    -- It holds a copy of production's data, which is not for everyone.
    seeded       INTEGER NOT NULL DEFAULT 0,
    created_by   TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    UNIQUE (env_id, name)
) STRICT;
CREATE INDEX services_env ON services (env_id);
