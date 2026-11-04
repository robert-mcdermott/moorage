-- A second factor, for those who have one. Its secret is sealed by the master key, and it counts as
-- theirs once they have shown that they can use it.
CREATE TABLE user_totp (
    user_id      TEXT PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    sealed       BLOB NOT NULL,
    created_at   INTEGER NOT NULL,
    confirmed_at INTEGER,
    -- The last step of time used, so that a code works once.
    last_step    INTEGER NOT NULL DEFAULT 0
);

-- What gets them in if they lose it, by hash, once each.
CREATE TABLE user_recovery_codes (
    user_id   TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    code_hash TEXT NOT NULL,
    used_at   INTEGER,
    PRIMARY KEY (user_id, code_hash)
);
