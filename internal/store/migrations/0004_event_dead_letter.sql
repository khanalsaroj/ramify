-- Retire events that must not be retried again, keeping them queryable for
-- operators instead of deleting them or leaving them pending forever.
ALTER TABLE events ADD COLUMN dead_lettered_at DATETIME;

CREATE INDEX IF NOT EXISTS idx_events_dead_lettered
    ON events(dead_lettered_at, created_at);
