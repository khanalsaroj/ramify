ALTER TABLE events ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE events ADD COLUMN next_attempt_at DATETIME;
ALTER TABLE events ADD COLUMN last_error TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_events_due
    ON events(processed_at, next_attempt_at, created_at);
