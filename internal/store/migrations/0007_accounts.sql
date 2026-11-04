-- Accounts (spec/10-data-model.md, "Identity and access"). Passwords, session
-- IDs, tokens and one-time codes are stored only as hashes. A token or an SSH
-- key without a user belongs to the server's local administrator.
CREATE TABLE users (
    id            TEXT PRIMARY KEY,
    email         TEXT NOT NULL UNIQUE,
    name          TEXT NOT NULL,
    -- NULL until the user accepts their invitation.
    password_hash TEXT,
    role          TEXT NOT NULL CHECK (role IN ('admin', 'member')),
    disabled_at   INTEGER,
    created_at    INTEGER NOT NULL
) STRICT;

CREATE TABLE sessions (
    id           TEXT PRIMARY KEY,
    id_hash      TEXT NOT NULL UNIQUE,
    user_id      TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    ip           TEXT,
    user_agent   TEXT
) STRICT;

CREATE INDEX sessions_user ON sessions (user_id);

CREATE TABLE api_tokens (
    id           TEXT PRIMARY KEY,
    user_id      TEXT REFERENCES users (id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    token_hash   TEXT NOT NULL UNIQUE,
    scopes       TEXT NOT NULL CHECK (json_valid(scopes)),
    -- A token restricted to one app can reach only that app.
    app_id       TEXT REFERENCES apps (id) ON DELETE CASCADE,
    expires_at   INTEGER,
    last_used_at INTEGER,
    created_at   INTEGER NOT NULL
) STRICT;

CREATE UNIQUE INDEX api_tokens_name ON api_tokens (coalesce(user_id, ''), name);

-- Keys that may push over SSH.
CREATE TABLE ssh_keys (
    id           TEXT PRIMARY KEY,
    user_id      TEXT REFERENCES users (id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    fingerprint  TEXT NOT NULL UNIQUE,
    public_key   TEXT NOT NULL,
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER
) STRICT;

CREATE UNIQUE INDEX ssh_keys_name ON ssh_keys (coalesce(user_id, ''), name);

-- A CLI's sign-in, waiting for its user to approve it in a browser.
CREATE TABLE device_codes (
    code_hash    TEXT PRIMARY KEY,
    user_code    TEXT NOT NULL UNIQUE,
    client       TEXT NOT NULL,
    ip           TEXT,
    approved_by  TEXT REFERENCES users (id) ON DELETE CASCADE,
    denied       INTEGER NOT NULL DEFAULT 0,
    last_poll_at INTEGER,
    expires_at   INTEGER NOT NULL
) STRICT;

-- One-time links with which a new user chooses a password, or a user who has
-- forgotten theirs chooses another.
CREATE TABLE invitations (
    code_hash  TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_by TEXT,
    expires_at INTEGER NOT NULL
) STRICT;
