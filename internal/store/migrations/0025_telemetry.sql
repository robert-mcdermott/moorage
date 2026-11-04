-- Where what apps say, and what they do, is sent besides being kept here.
CREATE TABLE telemetry_sinks (
    id             TEXT PRIMARY KEY,
    name           TEXT NOT NULL UNIQUE,
    kind           TEXT NOT NULL,
    address        TEXT NOT NULL,
    sealed_headers BLOB,
    logs           INTEGER NOT NULL DEFAULT 1,
    metrics        INTEGER NOT NULL DEFAULT 0,
    -- The one app it is for, or NULL for all of them.
    app_id         TEXT REFERENCES apps (id) ON DELETE CASCADE,
    last_sent_at   INTEGER NOT NULL DEFAULT 0,
    last_error     TEXT NOT NULL DEFAULT '',
    dropped        INTEGER NOT NULL DEFAULT 0,
    created_by     TEXT NOT NULL,
    created_at     INTEGER NOT NULL
) STRICT;

-- How far what is in the log files has been read, across restarts of the server, as apps go on through those.
CREATE TABLE telemetry_cursor (
    id     INTEGER PRIMARY KEY CHECK (id = 1),
    cursor INTEGER NOT NULL
) STRICT;

-- When to tell someone about how an environment is doing.
CREATE TABLE alert_thresholds (
    id         TEXT PRIMARY KEY,
    env_id     TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    metric     TEXT NOT NULL,
    above      REAL NOT NULL,
    minutes    INTEGER NOT NULL,
    firing     INTEGER NOT NULL DEFAULT 0,
    created_by TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    UNIQUE (env_id, metric, above)
) STRICT;
