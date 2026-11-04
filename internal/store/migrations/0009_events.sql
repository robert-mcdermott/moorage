-- What happened, for the activity timeline and the live updates of the console
-- (spec/07-observability.md, "Events and activity"). The app and environment are
-- by name: an event outlives what it is about.
CREATE TABLE events (
    id    TEXT PRIMARY KEY,
    at    INTEGER NOT NULL,
    type  TEXT NOT NULL,
    app   TEXT NOT NULL DEFAULT '',
    env   TEXT NOT NULL DEFAULT '',
    actor TEXT NOT NULL DEFAULT '',
    data  TEXT NOT NULL DEFAULT '{}'
) STRICT;

CREATE INDEX events_by_app ON events (app, id);
CREATE INDEX events_by_time ON events (at);
