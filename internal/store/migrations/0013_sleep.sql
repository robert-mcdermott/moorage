-- An environment asleep has its instances stopped until a request wakes it
-- (spec/05-runtime.md, "Sleep and wake").
ALTER TABLE environments ADD COLUMN asleep_at INTEGER;
