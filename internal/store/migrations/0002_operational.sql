-- Add operational indexes and durable webhook delivery deduplication.
ALTER TABLE events ADD COLUMN dedupe_key TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS idx_events_kind_dedupe ON events(kind, dedupe_key)
    WHERE dedupe_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_environments_ttl
    ON environments(pinned, ttl_expires_at);
CREATE INDEX IF NOT EXISTS idx_events_kind_processed
    ON events(kind, processed_at);
