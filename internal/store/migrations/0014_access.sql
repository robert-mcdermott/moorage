-- Who may reach an environment (spec/06-ingress.md, "Access gates"). A mode that is not
-- set leaves it to the manifest, and then the server's default for its kind.
ALTER TABLE environments ADD COLUMN access_mode TEXT;
ALTER TABLE environments ADD COLUMN access_password TEXT;
ALTER TABLE environments ADD COLUMN access_allow TEXT;
ALTER TABLE environments ADD COLUMN access_any_user INTEGER NOT NULL DEFAULT 0;
-- Changes when a change of access must end who is in: it is part of what a gate cookie signs.
ALTER TABLE environments ADD COLUMN access_generation INTEGER NOT NULL DEFAULT 1;

CREATE TABLE share_links (
    id           TEXT PRIMARY KEY,
    env_id       TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    token_hash   TEXT NOT NULL UNIQUE,
    created_by   TEXT NOT NULL,
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    max_uses     INTEGER NOT NULL DEFAULT 0,
    uses         INTEGER NOT NULL DEFAULT 0,
    last_used_at INTEGER,
    revoked_at   INTEGER
);
CREATE INDEX share_links_env ON share_links (env_id, created_at);
