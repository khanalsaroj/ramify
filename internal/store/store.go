// SPDX-License-Identifier: Apache-2.0

// Package store defines the Ramify persistence interface and its SQLite
// implementation.
package store

import (
	"context"
	"errors"
	"time"
)

// Environment status values.
const (
	StatusPending    = "pending"
	StatusDeploying  = "deploying"
	StatusReady      = "ready"
	StatusFailed     = "failed"
	StatusSleeping   = "sleeping"
	StatusDestroying = "destroying"
	StatusDestroyed  = "destroyed"
)

// EventKindWebhookReceived is a durable inbox event created before a webhook
// request is acknowledged. Its payload contains the normalized provider event.
const EventKindWebhookReceived = "webhook_received"

// ErrNotFound is returned when a lookup by ID or unique key finds no row.
var ErrNotFound = errors.New("store: not found")

// ErrConflict is returned when a write would violate a uniqueness constraint, such
// as the UNIQUE(project, branch) constraint on environments.
var ErrConflict = errors.New("store: conflict")

// Environment is a preview environment record.
type Environment struct {
	ID           string
	Project      string
	Branch       string
	PRNumber     int // 0 means no associated pull request
	Subdomain    string
	ArtifactRef  string
	DeployRef    string
	Status       string
	Pinned       bool
	TTLExpiresAt *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// DNSRecord is a DNS record created for an environment.
type DNSRecord struct {
	ID            string
	EnvironmentID string
	RecordType    string
	Name          string
	Value         string
	OwnershipTag  string
	CreatedAt     time.Time
}

// Event is an append-only log entry recording an environment state transition,
// written before any provider call is made so a crash mid-reconciliation can be
// recovered by replaying unprocessed events.
type Event struct {
	ID             string
	EnvironmentID  string // empty if not associated with an environment
	Kind           string
	DedupeKey      string // optional provider delivery ID; unique when present
	Payload        string // JSON
	Attempts       int
	NextAttemptAt  *time.Time
	LastError      string
	ProcessedAt    *time.Time
	DeadLetteredAt *time.Time // set when the event was retired without succeeding
	CreatedAt      time.Time
}

// Default and maximum page sizes for ListEnvironments. The maximum exists so a
// caller cannot turn a paginated endpoint back into an unbounded one by asking
// for an enormous limit.
const (
	DefaultListLimit = 100
	MaxListLimit     = 500
)

// maxQueryLimit is the largest Limit the store will honor: one row beyond the
// client-facing cap. A caller detecting "is there another page?" asks for one
// extra row, and clamping that lookahead away would make a full page look like
// the last one and silently hide every row after it.
const maxQueryLimit = MaxListLimit + 1

// ListOptions selects and windows the rows returned by ListEnvironments. The
// zero value returns the first DefaultListLimit environments unfiltered.
type ListOptions struct {
	Project string // exact match when non-empty
	Branch  string // exact match when non-empty
	Limit   int    // clamped to maxQueryLimit; 0 means DefaultListLimit
	Offset  int    // negative values are treated as 0
}

// normalized returns opts with Limit and Offset clamped to their valid ranges.
func (o ListOptions) normalized() ListOptions {
	switch {
	case o.Limit <= 0:
		o.Limit = DefaultListLimit
	case o.Limit > maxQueryLimit:
		o.Limit = maxQueryLimit
	}
	if o.Offset < 0 {
		o.Offset = 0
	}
	return o
}

// Store is the Ramify persistence interface. It is implemented by sqliteStore for
// production use and can be faked in-memory for unit tests that don't need SQLite.
type Store interface {
	// CreateEnvironment inserts env and returns the stored row. It returns
	// ErrConflict if an environment with the same Project and Branch already
	// exists.
	CreateEnvironment(ctx context.Context, env Environment) (Environment, error)
	// UpdateEnvironment overwrites the mutable fields of the environment
	// identified by env.ID. It returns ErrNotFound if no such environment exists.
	UpdateEnvironment(ctx context.Context, env Environment) (Environment, error)
	// GetEnvironment returns the environment with the given ID, or ErrNotFound.
	GetEnvironment(ctx context.Context, id string) (Environment, error)
	// GetEnvironmentByProjectBranch returns the environment for the given project
	// and branch, or ErrNotFound.
	GetEnvironmentByProjectBranch(ctx context.Context, project, branch string) (Environment, error)
	// ListEnvironments returns one page of environments matching opts, oldest
	// first. See ListOptions for the defaults applied to an empty value.
	ListEnvironments(ctx context.Context, opts ListOptions) ([]Environment, error)
	// ListExpiredEnvironments returns non-pinned environments whose TTLExpiresAt
	// is set and at or before now.
	ListExpiredEnvironments(ctx context.Context, now time.Time) ([]Environment, error)

	// CreateDNSRecord inserts rec and returns the stored row.
	CreateDNSRecord(ctx context.Context, rec DNSRecord) (DNSRecord, error)
	// ListDNSRecords returns every DNS record for the given environment ID.
	ListDNSRecords(ctx context.Context, environmentID string) ([]DNSRecord, error)
	// DeleteDNSRecord removes the DNS record with the given ID.
	DeleteDNSRecord(ctx context.Context, id string) error

	// CreateEvent inserts ev, unprocessed, and returns the stored row.
	CreateEvent(ctx context.Context, ev Event) (Event, error)
	// ListUnprocessedEvents returns every event with a NULL processed_at that has
	// not been dead-lettered, oldest first.
	ListUnprocessedEvents(ctx context.Context) ([]Event, error)
	// ListDueEvents returns pending, non-dead-lettered events whose retry time
	// has arrived.
	ListDueEvents(ctx context.Context, now time.Time, limit int) ([]Event, error)
	// ClaimEvent atomically leases a due event to one worker.
	ClaimEvent(ctx context.Context, id string, now, leaseUntil time.Time) (bool, error)
	// MarkEventProcessed sets processed_at on the event with the given ID.
	MarkEventProcessed(ctx context.Context, id string, processedAt time.Time) error
	// MarkEventRetry records a failed attempt and schedules the next retry.
	MarkEventRetry(ctx context.Context, id string, nextAttempt time.Time, lastError string) error
	// MarkEventDeadLettered retires an event that must not be retried again,
	// either because it failed permanently or because it exhausted its attempt
	// budget. Dead-lettered events stay in the table for inspection but are
	// excluded from the due and unprocessed sets.
	MarkEventDeadLettered(ctx context.Context, id string, at time.Time, lastError string) error
	// ListDeadLetteredEvents returns retired events, newest first, for operator
	// diagnostics.
	ListDeadLetteredEvents(ctx context.Context, limit int) ([]Event, error)
	// CountDeadLetteredEvents returns how many events have been retired. It is
	// separate from ListDeadLetteredEvents so the metrics gauge reports a true
	// total rather than a page size.
	CountDeadLetteredEvents(ctx context.Context) (int, error)
	// PruneProcessedEvents removes old completed events and returns the count.
	PruneProcessedEvents(ctx context.Context, before time.Time, limit int) (int, error)
	// Backup creates a consistent SQLite backup at path without modifying the
	// live database.
	Backup(ctx context.Context, path string) error

	// Close releases resources held by the store.
	Close() error
}
