// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// sqliteStore is the SQLite-backed implementation of Store.
type sqliteStore struct {
	db *sql.DB
}

var _ Store = (*sqliteStore)(nil)

// Open opens (creating if necessary) the SQLite database at path, applies any
// pending migrations, and returns a Store backed by it. Use ":memory:" for an
// ephemeral in-process database.
func Open(ctx context.Context, path string) (Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite database %s: %w", path, err)
	}
	// modernc.org/sqlite connections are not safe for concurrent writers; a
	// single shared connection serializes access and avoids SQLITE_BUSY errors.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		_ = db.Close() // best-effort cleanup after a failed open
		return nil, fmt.Errorf("enabling foreign keys on %s: %w", path, err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout = 5000`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("setting sqlite busy timeout on %s: %w", path, err)
	}
	if err := applyMigrations(ctx, db); err != nil {
		_ = db.Close() // best-effort cleanup after a failed open
		return nil, fmt.Errorf("migrating sqlite database %s: %w", path, err)
	}
	return &sqliteStore{db: db}, nil
}

// Close implements Store.
func (s *sqliteStore) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("closing sqlite database: %w", err)
	}
	return nil
}

func isUniqueConstraintErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// CreateEnvironment implements Store.
func (s *sqliteStore) CreateEnvironment(ctx context.Context, env Environment) (Environment, error) {
	if env.ID == "" {
		id, err := newID()
		if err != nil {
			return Environment{}, fmt.Errorf("creating environment %s/%s: %w", env.Project, env.Branch, err)
		}
		env.ID = id
	}
	now := time.Now().UTC()
	env.CreatedAt = now
	env.UpdatedAt = now

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO environments
			(id, project, branch, pr_number, subdomain, artifact_ref, deploy_ref, status, pinned, ttl_expires_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		env.ID, env.Project, env.Branch, nullableInt(env.PRNumber), env.Subdomain, env.ArtifactRef, env.DeployRef,
		env.Status, env.Pinned, env.TTLExpiresAt, env.CreatedAt, env.UpdatedAt,
	)
	if isUniqueConstraintErr(err) {
		return Environment{}, fmt.Errorf("creating environment %s/%s: %w", env.Project, env.Branch, ErrConflict)
	}
	if err != nil {
		return Environment{}, fmt.Errorf("creating environment %s/%s: %w", env.Project, env.Branch, err)
	}
	return env, nil
}

// UpdateEnvironment implements Store.
func (s *sqliteStore) UpdateEnvironment(ctx context.Context, env Environment) (Environment, error) {
	env.UpdatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		UPDATE environments SET
			project = ?, branch = ?, pr_number = ?, subdomain = ?, artifact_ref = ?,
			deploy_ref = ?, status = ?, pinned = ?, ttl_expires_at = ?, updated_at = ?
		WHERE id = ?`,
		env.Project, env.Branch, nullableInt(env.PRNumber), env.Subdomain, env.ArtifactRef,
		env.DeployRef, env.Status, env.Pinned, env.TTLExpiresAt, env.UpdatedAt, env.ID,
	)
	if isUniqueConstraintErr(err) {
		return Environment{}, fmt.Errorf("updating environment %s: %w", env.ID, ErrConflict)
	}
	if err != nil {
		return Environment{}, fmt.Errorf("updating environment %s: %w", env.ID, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return Environment{}, fmt.Errorf("checking update result for environment %s: %w", env.ID, err)
	} else if n == 0 {
		return Environment{}, fmt.Errorf("updating environment %s: %w", env.ID, ErrNotFound)
	}
	return s.GetEnvironment(ctx, env.ID)
}

func scanEnvironment(row interface{ Scan(...any) error }) (Environment, error) {
	var env Environment
	var prNumber sql.NullInt64
	var ttl sql.NullTime
	err := row.Scan(
		&env.ID, &env.Project, &env.Branch, &prNumber, &env.Subdomain, &env.ArtifactRef,
		&env.DeployRef, &env.Status, &env.Pinned, &ttl, &env.CreatedAt, &env.UpdatedAt,
	)
	if err != nil {
		return Environment{}, err
	}
	env.PRNumber = int(prNumber.Int64)
	if ttl.Valid {
		t := ttl.Time
		env.TTLExpiresAt = &t
	}
	return env, nil
}

const environmentColumns = `id, project, branch, pr_number, subdomain, artifact_ref, deploy_ref, status, pinned, ttl_expires_at, created_at, updated_at`

// GetEnvironment implements Store.
func (s *sqliteStore) GetEnvironment(ctx context.Context, id string) (Environment, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+environmentColumns+` FROM environments WHERE id = ?`, id)
	env, err := scanEnvironment(row)
	if err == sql.ErrNoRows {
		return Environment{}, fmt.Errorf("getting environment %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Environment{}, fmt.Errorf("getting environment %s: %w", id, err)
	}
	return env, nil
}

// GetEnvironmentByProjectBranch implements Store.
func (s *sqliteStore) GetEnvironmentByProjectBranch(ctx context.Context, project, branch string) (Environment, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+environmentColumns+` FROM environments WHERE project = ? AND branch = ?`, project, branch)
	env, err := scanEnvironment(row)
	if err == sql.ErrNoRows {
		return Environment{}, fmt.Errorf("getting environment %s/%s: %w", project, branch, ErrNotFound)
	}
	if err != nil {
		return Environment{}, fmt.Errorf("getting environment %s/%s: %w", project, branch, err)
	}
	return env, nil
}

// ListEnvironments implements Store.
func (s *sqliteStore) ListEnvironments(ctx context.Context) ([]Environment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+environmentColumns+` FROM environments ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("listing environments: %w", err)
	}
	defer func() { _ = rows.Close() }() // read-only cleanup; nothing actionable if it fails

	var out []Environment
	for rows.Next() {
		env, err := scanEnvironment(rows)
		if err != nil {
			return nil, fmt.Errorf("listing environments: %w", err)
		}
		out = append(out, env)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing environments: %w", err)
	}
	return out, nil
}

// ListExpiredEnvironments implements Store.
func (s *sqliteStore) ListExpiredEnvironments(ctx context.Context, now time.Time) ([]Environment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+environmentColumns+` FROM environments
		WHERE pinned = 0 AND ttl_expires_at IS NOT NULL AND ttl_expires_at <= ?
		ORDER BY ttl_expires_at`, now.UTC())
	if err != nil {
		return nil, fmt.Errorf("listing expired environments: %w", err)
	}
	defer func() { _ = rows.Close() }() // read-only cleanup; nothing actionable if it fails

	var out []Environment
	for rows.Next() {
		env, err := scanEnvironment(rows)
		if err != nil {
			return nil, fmt.Errorf("listing expired environments: %w", err)
		}
		out = append(out, env)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing expired environments: %w", err)
	}
	return out, nil
}

// CreateDNSRecord implements Store.
func (s *sqliteStore) CreateDNSRecord(ctx context.Context, rec DNSRecord) (DNSRecord, error) {
	if rec.ID == "" {
		id, err := newID()
		if err != nil {
			return DNSRecord{}, fmt.Errorf("creating dns record for environment %s: %w", rec.EnvironmentID, err)
		}
		rec.ID = id
	}
	rec.CreatedAt = time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO dns_records (id, environment_id, record_type, name, value, ownership_tag, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		rec.ID, rec.EnvironmentID, rec.RecordType, rec.Name, rec.Value, rec.OwnershipTag, rec.CreatedAt,
	)
	if err != nil {
		return DNSRecord{}, fmt.Errorf("creating dns record for environment %s: %w", rec.EnvironmentID, err)
	}
	return rec, nil
}

// ListDNSRecords implements Store.
func (s *sqliteStore) ListDNSRecords(ctx context.Context, environmentID string) ([]DNSRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, environment_id, record_type, name, value, ownership_tag, created_at
		FROM dns_records WHERE environment_id = ? ORDER BY created_at`, environmentID)
	if err != nil {
		return nil, fmt.Errorf("listing dns records for environment %s: %w", environmentID, err)
	}
	defer func() { _ = rows.Close() }() // read-only cleanup; nothing actionable if it fails

	var out []DNSRecord
	for rows.Next() {
		var rec DNSRecord
		if err := rows.Scan(&rec.ID, &rec.EnvironmentID, &rec.RecordType, &rec.Name, &rec.Value, &rec.OwnershipTag, &rec.CreatedAt); err != nil {
			return nil, fmt.Errorf("listing dns records for environment %s: %w", environmentID, err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing dns records for environment %s: %w", environmentID, err)
	}
	return out, nil
}

// DeleteDNSRecord implements Store.
func (s *sqliteStore) DeleteDNSRecord(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM dns_records WHERE id = ?`, id); err != nil {
		return fmt.Errorf("deleting dns record %s: %w", id, err)
	}
	return nil
}

// CreateEvent implements Store.
func (s *sqliteStore) CreateEvent(ctx context.Context, ev Event) (Event, error) {
	if ev.ID == "" {
		id, err := newID()
		if err != nil {
			return Event{}, fmt.Errorf("creating event %s: %w", ev.Kind, err)
		}
		ev.ID = id
	}
	ev.CreatedAt = time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO events (id, environment_id, kind, dedupe_key, payload, processed_at, created_at, attempts, next_attempt_at, last_error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.ID, nullableString(ev.EnvironmentID), ev.Kind, nullableString(ev.DedupeKey), ev.Payload, ev.ProcessedAt, ev.CreatedAt, ev.Attempts, ev.NextAttemptAt, ev.LastError,
	)
	if isUniqueConstraintErr(err) {
		return Event{}, fmt.Errorf("creating event %s: %w", ev.Kind, ErrConflict)
	}
	if err != nil {
		return Event{}, fmt.Errorf("creating event %s: %w", ev.Kind, err)
	}
	return ev, nil
}

// ListUnprocessedEvents implements Store.
func (s *sqliteStore) ListUnprocessedEvents(ctx context.Context) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, environment_id, kind, dedupe_key, payload, processed_at, created_at, attempts, next_attempt_at, last_error
		FROM events WHERE processed_at IS NULL ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("listing unprocessed events: %w", err)
	}
	defer func() { _ = rows.Close() }() // read-only cleanup; nothing actionable if it fails

	var out []Event
	for rows.Next() {
		var ev Event
		var environmentID sql.NullString
		var processedAt sql.NullTime
		var dedupeKey sql.NullString
		var nextAttemptAt sql.NullTime
		if err := rows.Scan(&ev.ID, &environmentID, &ev.Kind, &dedupeKey, &ev.Payload, &processedAt, &ev.CreatedAt, &ev.Attempts, &nextAttemptAt, &ev.LastError); err != nil {
			return nil, fmt.Errorf("listing unprocessed events: %w", err)
		}
		ev.EnvironmentID = environmentID.String
		ev.DedupeKey = dedupeKey.String
		if nextAttemptAt.Valid {
			t := nextAttemptAt.Time
			ev.NextAttemptAt = &t
		}
		if processedAt.Valid {
			t := processedAt.Time
			ev.ProcessedAt = &t
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing unprocessed events: %w", err)
	}
	return out, nil
}

// ListDueEvents implements Store.
func (s *sqliteStore) ListDueEvents(ctx context.Context, now time.Time, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, environment_id, kind, dedupe_key, payload, processed_at, created_at, attempts, next_attempt_at, last_error
		FROM events
		WHERE processed_at IS NULL AND (next_attempt_at IS NULL OR next_attempt_at <= ?)
		ORDER BY created_at LIMIT ?`, now.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("listing due events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Event
	for rows.Next() {
		var ev Event
		var environmentID, dedupeKey sql.NullString
		var processedAt, nextAttemptAt sql.NullTime
		if err := rows.Scan(&ev.ID, &environmentID, &ev.Kind, &dedupeKey, &ev.Payload, &processedAt, &ev.CreatedAt, &ev.Attempts, &nextAttemptAt, &ev.LastError); err != nil {
			return nil, fmt.Errorf("listing due events: %w", err)
		}
		ev.EnvironmentID, ev.DedupeKey = environmentID.String, dedupeKey.String
		if processedAt.Valid {
			t := processedAt.Time
			ev.ProcessedAt = &t
		}
		if nextAttemptAt.Valid {
			t := nextAttemptAt.Time
			ev.NextAttemptAt = &t
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing due events: %w", err)
	}
	return out, nil
}

func (s *sqliteStore) ClaimEvent(ctx context.Context, id string, now, leaseUntil time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE events SET next_attempt_at = ?
		WHERE id = ? AND processed_at IS NULL
		  AND (next_attempt_at IS NULL OR next_attempt_at <= ?)`, leaseUntil.UTC(), id, now.UTC())
	if err != nil {
		return false, fmt.Errorf("claiming event %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claiming event %s: %w", id, err)
	}
	return n == 1, nil
}

// MarkEventProcessed implements Store.
func (s *sqliteStore) MarkEventProcessed(ctx context.Context, id string, processedAt time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE events SET processed_at = ? WHERE id = ?`, processedAt.UTC(), id)
	if err != nil {
		return fmt.Errorf("marking event %s processed: %w", id, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("marking event %s processed: %w", id, err)
	} else if n == 0 {
		return fmt.Errorf("marking event %s processed: %w", id, ErrNotFound)
	}
	return nil
}

func (s *sqliteStore) MarkEventRetry(ctx context.Context, id string, nextAttempt time.Time, lastError string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE events SET attempts = attempts + 1, next_attempt_at = ?, last_error = ? WHERE id = ? AND processed_at IS NULL`, nextAttempt.UTC(), lastError, id)
	if err != nil {
		return fmt.Errorf("marking event %s retryable: %w", id, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("marking event %s retryable: %w", id, err)
	} else if n == 0 {
		return fmt.Errorf("marking event %s retryable: %w", id, ErrNotFound)
	}
	return nil
}

func (s *sqliteStore) PruneProcessedEvents(ctx context.Context, before time.Time, limit int) (int, error) {
	if limit <= 0 {
		limit = 5000
	}
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM events
		WHERE id IN (
			SELECT id FROM events
			WHERE processed_at IS NOT NULL AND created_at < ?
			ORDER BY created_at LIMIT ?
		)`, before.UTC(), limit)
	if err != nil {
		return 0, fmt.Errorf("pruning processed events: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("counting pruned events: %w", err)
	}
	return int(n), nil
}

// Backup implements Store using SQLite's online VACUUM INTO mechanism. The
// destination must not already exist; this avoids silently overwriting an
// operator's backup.
func (s *sqliteStore) Backup(ctx context.Context, path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("backing up sqlite database: empty destination path")
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("backing up sqlite database: destination already exists: %s", path)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("checking backup destination %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("creating backup directory: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		return fmt.Errorf("backing up sqlite database to %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("securing sqlite backup %s: %w", path, err)
	}
	return nil
}

func nullableInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullableString(v string) any {
	if v == "" {
		return nil
	}
	return v
}
