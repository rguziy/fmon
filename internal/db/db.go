// Package db wraps the embedded SQLite database used by fmon.
//
// It uses the pure Go driver modernc.org/sqlite, so the resulting binaries
// need no CGO and cross-compile cleanly.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	// Register the pure Go "sqlite" driver.
	_ "modernc.org/sqlite"
)

// busyTimeoutMS is how long a connection waits for a competing writer before
// giving up with SQLITE_BUSY. It is a variable so tests can shorten it.
var busyTimeoutMS = 5000

var (
	// ErrLocked is returned when the write lock cannot be obtained because
	// another fmon process holds it.
	ErrLocked = errors.New("another fmon run is in progress")

	// ErrNewerSchema is returned when the database was created by a newer
	// fmon release than the running binary.
	ErrNewerSchema = errors.New("database schema is newer than this fmon binary; upgrade fmon")
)

// DBTX is the subset of database/sql shared by *sql.DB, *sql.Conn and *Tx.
// All query helpers in this package accept it, so they work both inside and
// outside of a transaction.
type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// dsn builds the SQLite URI for path. Every connection gets the same pragmas:
// foreign keys must be enabled per connection for ON DELETE CASCADE to work.
func dsn(path string) string {
	p := filepath.ToSlash(path)
	if len(p) >= 2 && p[1] == ':' { // Windows drive letter: C:/x -> /C:/x
		p = "/" + p
	}
	u := url.URL{Scheme: "file", Path: p}
	u.RawQuery = strings.Join([]string{
		"_pragma=foreign_keys(1)",
		"_pragma=journal_mode(WAL)",
		"_pragma=synchronous(NORMAL)",
		fmt.Sprintf("_pragma=busy_timeout(%d)", busyTimeoutMS),
	}, "&")
	return u.String()
}

// Open opens (creating it if necessary) the database at path and applies any
// pending schema migrations.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	// Pre-create the file with restrictive permissions; SQLite would use the
	// process umask (typically 0644). The -wal and -shm files inherit the mode.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create database file: %w", err)
	}
	_ = f.Close()

	d, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := d.PingContext(ctx); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("open database %q: %w", path, err)
	}
	if err := Migrate(ctx, d); err != nil {
		_ = d.Close()
		return nil, err
	}
	return d, nil
}

func isBusy(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database is locked") || strings.Contains(msg, "sqlite_busy")
}

// Tx is a write transaction started with BEGIN IMMEDIATE on a dedicated
// connection. Taking the write lock up front gives mutual exclusion between
// concurrent fmon runs and avoids mid-transaction lock upgrades that could
// fail with SQLITE_BUSY.
type Tx struct {
	conn *sql.Conn
	open bool
}

// BeginImmediate starts a write transaction. It returns ErrLocked when another
// process holds the write lock for longer than the busy timeout.
func BeginImmediate(ctx context.Context, d *sql.DB) (*Tx, error) {
	conn, err := d.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		_ = conn.Close()
		if isBusy(err) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	return &Tx{conn: conn, open: true}, nil
}

// ExecContext implements DBTX.
func (t *Tx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.conn.ExecContext(ctx, query, args...)
}

// QueryContext implements DBTX.
func (t *Tx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return t.conn.QueryContext(ctx, query, args...)
}

// QueryRowContext implements DBTX.
func (t *Tx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return t.conn.QueryRowContext(ctx, query, args...)
}

// Commit commits the transaction and releases the connection.
func (t *Tx) Commit() error {
	if !t.open {
		return errors.New("transaction already finished")
	}
	t.open = false
	defer t.conn.Close()
	if _, err := t.conn.ExecContext(context.Background(), "COMMIT"); err != nil {
		_, _ = t.conn.ExecContext(context.Background(), "ROLLBACK")
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// Rollback aborts the transaction. It is safe to call after Commit (no-op),
// which makes `defer tx.Rollback()` the idiomatic cleanup.
func (t *Tx) Rollback() error {
	if !t.open {
		return nil
	}
	t.open = false
	defer t.conn.Close()
	// A background context is used on purpose: the caller's context may
	// already be cancelled, and the rollback must still happen.
	if _, err := t.conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		return fmt.Errorf("rollback: %w", err)
	}
	return nil
}
