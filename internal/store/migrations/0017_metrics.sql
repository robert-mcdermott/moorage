-- Roll-ups of what environments did (spec/07-observability.md, "Metrics"). A row holds packed
-- records of an hour's minutes (tier 1) or a day's hours (tier 2), so that a day of a series is at
-- most 24 rows and not 1,440. env_id is empty for what is of the server as a whole.
CREATE TABLE metric_points (
    env_id TEXT NOT NULL,
    name   TEXT NOT NULL,
    part   TEXT NOT NULL,
    tier   INTEGER NOT NULL,
    bucket INTEGER NOT NULL,
    data   BLOB NOT NULL,
    PRIMARY KEY (env_id, name, part, tier, bucket)
) WITHOUT ROWID;
CREATE INDEX metric_points_age ON metric_points (tier, bucket);

CREATE TRIGGER metric_points_of_env AFTER DELETE ON environments BEGIN
    DELETE FROM metric_points WHERE env_id = old.id;
END;
