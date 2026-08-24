CREATE TABLE environments (
    id              TEXT PRIMARY KEY,       -- uuid
    project         TEXT NOT NULL,
    branch          TEXT NOT NULL,
    pr_number       INTEGER,                -- nullable: branch pushed without an open PR
    subdomain       TEXT NOT NULL,
    artifact_ref    TEXT NOT NULL,          -- image tag / build reference
    deploy_ref      TEXT,                   -- provider-specific handle once deployed
    status          TEXT NOT NULL,          -- pending|deploying|ready|failed|sleeping|destroying|destroyed
    pinned          INTEGER NOT NULL DEFAULT 0,
    ttl_expires_at  DATETIME,
    created_at      DATETIME NOT NULL,
    updated_at      DATETIME NOT NULL,
    UNIQUE (project, branch)
);

CREATE TABLE dns_records (
    id              TEXT PRIMARY KEY,
    environment_id  TEXT NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    record_type     TEXT NOT NULL,
    name            TEXT NOT NULL,
    value           TEXT NOT NULL,
    ownership_tag   TEXT NOT NULL,          -- TXT ownership marker, see security rules
    created_at      DATETIME NOT NULL
);

CREATE TABLE events (
    id              TEXT PRIMARY KEY,
    environment_id  TEXT REFERENCES environments(id) ON DELETE SET NULL,
    kind            TEXT NOT NULL,
    payload         TEXT NOT NULL,          -- JSON
    processed_at    DATETIME,               -- NULL until the reconciler has consumed it
    created_at      DATETIME NOT NULL
);

CREATE INDEX idx_dns_records_environment_id ON dns_records(environment_id);
CREATE INDEX idx_events_environment_id ON events(environment_id);
CREATE INDEX idx_events_unprocessed ON events(processed_at) WHERE processed_at IS NULL;
