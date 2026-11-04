-- Where word of what happened goes (spec/07-observability.md, "Notifications"). A channel's
-- address and secrets are sealed, by the master key.
CREATE TABLE notification_channels (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    kind       TEXT NOT NULL,
    sealed     BLOB NOT NULL,
    created_by TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    sent_at    INTEGER,
    error      TEXT
);

-- Which events go to which channel: for the whole server, or a project, or an app.
CREATE TABLE notification_rules (
    id         TEXT PRIMARY KEY,
    channel_id TEXT NOT NULL REFERENCES notification_channels (id) ON DELETE CASCADE,
    project_id TEXT REFERENCES projects (id) ON DELETE CASCADE,
    app_id     TEXT REFERENCES apps (id) ON DELETE CASCADE,
    events     TEXT NOT NULL,
    created_by TEXT NOT NULL,
    created_at INTEGER NOT NULL
);

-- What someone is told in the console.
CREATE TABLE notifications (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind       TEXT NOT NULL,
    app        TEXT NOT NULL DEFAULT '',
    env        TEXT NOT NULL DEFAULT '',
    summary    TEXT NOT NULL,
    link       TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    read_at    INTEGER
);
CREATE INDEX notifications_user ON notifications (user_id, created_at);
