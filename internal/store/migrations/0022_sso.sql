-- Single sign-on with an OpenID Connect provider. There is one row, once someone has set it up.
CREATE TABLE sso_config (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    issuer        TEXT NOT NULL,
    client_id     TEXT NOT NULL,
    sealed_secret BLOB,
    label         TEXT NOT NULL DEFAULT '',
    scopes        TEXT NOT NULL DEFAULT '',
    email_domains TEXT NOT NULL DEFAULT '',
    provision     INTEGER NOT NULL DEFAULT 0,
    group_claim   TEXT NOT NULL DEFAULT '',
    group_rules   TEXT NOT NULL DEFAULT '',
    passwords_off INTEGER NOT NULL DEFAULT 0,
    updated_by    TEXT NOT NULL,
    updated_at    INTEGER NOT NULL
);

-- Which account someone is, by what their provider calls them, which stays the same when their
-- address changes.
CREATE TABLE user_identities (
    issuer  TEXT NOT NULL,
    subject TEXT NOT NULL,
    user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    PRIMARY KEY (issuer, subject)
);
