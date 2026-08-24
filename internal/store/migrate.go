// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// applyMigrations runs every embedded .sql file against db, in filename order,
// tracking applied filenames in a schema_migrations table so re-running is a no-op.
func applyMigrations(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		filename TEXT PRIMARY KEY,
		applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		return fmt.Errorf("creating schema_migrations table: %w", err)
	}

	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return fmt.Errorf("reading embedded migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		var applied int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(1) FROM schema_migrations WHERE filename = ?`, name).Scan(&applied); err != nil {
			return fmt.Errorf("checking migration %s: %w", name, err)
		}
		if applied > 0 {
			continue
		}

		contents, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("reading migration %s: %w", name, err)
		}

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("beginning transaction for migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, string(contents)); err != nil {
			_ = tx.Rollback() // rollback error is not actionable once the exec already failed
			return fmt.Errorf("applying migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (filename) VALUES (?)`, name); err != nil {
			_ = tx.Rollback() // rollback error is not actionable once the exec already failed
			return fmt.Errorf("recording migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("committing migration %s: %w", name, err)
		}
	}
	return nil
}
