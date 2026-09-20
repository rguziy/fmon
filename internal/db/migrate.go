package db

import (
	"context"
	"database/sql"
	"fmt"
)

// A migration upgrades the schema by exactly one version.
//
// Rules for adding a migration (append to the migrations slice, never edit or
// reorder existing ones):
//   - migrations are forward-only and run inside one transaction;
//   - never drop or truncate history_log, it is the audit trail;
//   - if a migration invalidates files_state, truncate that table and call
//     SetMeta(ctx, q, MetaNeedsBaseline, BaselineUpgrade): the next scan
//     rebuilds the baseline silently instead of reporting every file as ADDED.
type migration func(ctx context.Context, q DBTX) error

var migrations = []migration{
	migrateV1,
}

// SchemaVersion is the schema version this binary expects.
func SchemaVersion() int { return len(migrations) }

// Migrate brings the database schema up to SchemaVersion using
// PRAGMA user_version as the version marker.
func Migrate(ctx context.Context, d *sql.DB) error {
	current, err := userVersion(ctx, d)
	if err != nil {
		return err
	}
	if current > len(migrations) {
		return ErrNewerSchema
	}
	if current == len(migrations) {
		return nil // nothing to do; do not take the write lock needlessly
	}

	tx, err := BeginImmediate(ctx, d)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Re-check under the lock: another process may have migrated meanwhile.
	current, err = userVersion(ctx, tx)
	if err != nil {
		return err
	}
	if current > len(migrations) {
		return ErrNewerSchema
	}
	for i := current; i < len(migrations); i++ {
		if err := migrations[i](ctx, tx); err != nil {
			return fmt.Errorf("schema migration %d: %w", i+1, err)
		}
	}
	// PRAGMA does not accept bound parameters; the value is an int we control.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", len(migrations))); err != nil {
		return fmt.Errorf("set schema version: %w", err)
	}
	return tx.Commit()
}

func userVersion(ctx context.Context, q DBTX) (int, error) {
	var v int
	if err := q.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return v, nil
}

// migrateV1 creates the initial schema.
//
// All timestamps are INTEGER Unix nanoseconds (UTC). Hash columns hold the
// uint64 xxHash64 bit pattern reinterpreted as a signed INTEGER (see
// models.HashToDB). history_log intentionally has no foreign keys, so
// cascading deletes of watched sources never touch the audit history.
func migrateV1(ctx context.Context, q DBTX) error {
	stmts := []string{
		`CREATE TABLE meta (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE watched_sources (
			id         INTEGER PRIMARY KEY,
			path       TEXT NOT NULL UNIQUE,
			type       TEXT NOT NULL CHECK (type IN ('file','folder')),
			status     TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','missing')),
			created_at INTEGER NOT NULL
		)`,
		// UNIQUE(file_path) already creates the lookup index.
		`CREATE TABLE files_state (
			id         INTEGER PRIMARY KEY,
			source_id  INTEGER NOT NULL REFERENCES watched_sources(id) ON DELETE CASCADE,
			file_path  TEXT NOT NULL UNIQUE,
			last_hash  INTEGER NOT NULL,
			size       INTEGER NOT NULL,
			mod_time   INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		// Makes the ON DELETE CASCADE from watched_sources an index lookup.
		`CREATE INDEX idx_files_state_source ON files_state(source_id)`,
		`CREATE TABLE history_log (
			id         INTEGER PRIMARY KEY,
			file_path  TEXT NOT NULL,
			event_type TEXT NOT NULL CHECK (event_type IN ('ADDED','MODIFIED','DELETED')),
			old_hash   INTEGER,
			new_hash   INTEGER,
			old_size   INTEGER,
			new_size   INTEGER,
			timestamp  INTEGER NOT NULL
		)`,
		`CREATE INDEX idx_history_path ON history_log(file_path)`,
		`CREATE INDEX idx_history_ts ON history_log(timestamp)`,
	}
	for _, s := range stmts {
		if _, err := q.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}
