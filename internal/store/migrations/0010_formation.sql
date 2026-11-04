-- How many instances of each process an environment runs, where that was set on the
-- server: it overrides the manifest's `instances` (spec/05-runtime.md, "Scaling and
-- load balancing").
ALTER TABLE environments ADD COLUMN formation TEXT NOT NULL DEFAULT '{}';
