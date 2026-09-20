package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/rguziy/fmon/internal/models"
)

// MetaNeedsBaseline is the meta key that asks the next scan to rebuild the
// baseline from disk without reporting events. Its value says why.
const MetaNeedsBaseline = "needs_baseline"

// Values of the MetaNeedsBaseline key.
const (
	// BaselineUpgrade: a schema migration invalidated files_state.
	BaselineUpgrade = "1"
	// BaselineDBMissing: the database file was lost (or deleted) while
	// fmon.toml still lists sources, so the database was recreated empty.
	BaselineDBMissing = "db-missing"
)

// ---- meta ---------------------------------------------------------------

// GetMeta returns the value of a meta key and whether it exists.
func GetMeta(ctx context.Context, q DBTX, key string) (string, bool, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read meta %q: %w", key, err)
	}
	return v, true, nil
}

// SetMeta stores a meta key.
func SetMeta(ctx context.Context, q DBTX, key, value string) error {
	_, err := q.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("write meta %q: %w", key, err)
	}
	return nil
}

// DeleteMeta removes a meta key.
func DeleteMeta(ctx context.Context, q DBTX, key string) error {
	if _, err := q.ExecContext(ctx, `DELETE FROM meta WHERE key = ?`, key); err != nil {
		return fmt.Errorf("delete meta %q: %w", key, err)
	}
	return nil
}

// ---- watched_sources ------------------------------------------------------

// InsertSource adds a watched source and returns its id.
func InsertSource(ctx context.Context, q DBTX, path string, typ models.SourceType, now time.Time) (int64, error) {
	res, err := q.ExecContext(ctx,
		`INSERT INTO watched_sources(path, type, status, created_at) VALUES(?, ?, 'active', ?)`,
		path, string(typ), now.UnixNano())
	if err != nil {
		return 0, fmt.Errorf("insert source %q: %w", path, err)
	}
	return res.LastInsertId()
}

// ListSources returns all watched sources ordered by path.
func ListSources(ctx context.Context, q DBTX) ([]models.Source, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, path, type, status, created_at FROM watched_sources ORDER BY path`)
	if err != nil {
		return nil, fmt.Errorf("list sources: %w", err)
	}
	defer rows.Close()

	var out []models.Source
	for rows.Next() {
		var s models.Source
		var typ, status string
		var created int64
		if err := rows.Scan(&s.ID, &s.Path, &typ, &status, &created); err != nil {
			return nil, fmt.Errorf("scan source: %w", err)
		}
		s.Type = models.SourceType(typ)
		s.Status = models.SourceStatus(status)
		s.CreatedAt = time.Unix(0, created).UTC()
		out = append(out, s)
	}
	return out, rows.Err()
}

// SetSourceStatus updates the status of a source.
func SetSourceStatus(ctx context.Context, q DBTX, id int64, status models.SourceStatus) error {
	if _, err := q.ExecContext(ctx,
		`UPDATE watched_sources SET status = ? WHERE id = ?`, string(status), id); err != nil {
		return fmt.Errorf("update source status: %w", err)
	}
	return nil
}

// DeleteSource removes a source. Its files_state rows are removed by the
// foreign key cascade; history_log is untouched.
func DeleteSource(ctx context.Context, q DBTX, id int64) error {
	if _, err := q.ExecContext(ctx, `DELETE FROM watched_sources WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete source: %w", err)
	}
	return nil
}

// ---- files_state ------------------------------------------------------------

// LoadFileStates returns all tracked files of a source keyed by path.
func LoadFileStates(ctx context.Context, q DBTX, sourceID int64) (map[string]models.FileState, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, source_id, file_path, last_hash, size, mod_time, updated_at
		 FROM files_state WHERE source_id = ?`, sourceID)
	if err != nil {
		return nil, fmt.Errorf("load file states: %w", err)
	}
	defer rows.Close()

	out := make(map[string]models.FileState)
	for rows.Next() {
		var s models.FileState
		var h int64
		if err := rows.Scan(&s.ID, &s.SourceID, &s.Path, &h, &s.Size, &s.ModTime, &s.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan file state: %w", err)
		}
		s.Hash = models.HashFromDB(h)
		out[s.Path] = s
	}
	return out, rows.Err()
}

// FileTotals summarizes the tracked files of one source.
type FileTotals struct {
	Files int64
	Bytes int64
}

// FileTotalsBySource returns the number of tracked files and their total size
// for every source that has at least one tracked file.
func FileTotalsBySource(ctx context.Context, q DBTX) (map[int64]FileTotals, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT source_id, COUNT(*), COALESCE(SUM(size), 0) FROM files_state GROUP BY source_id`)
	if err != nil {
		return nil, fmt.Errorf("count tracked files: %w", err)
	}
	defer rows.Close()

	out := make(map[int64]FileTotals)
	for rows.Next() {
		var id int64
		var t FileTotals
		if err := rows.Scan(&id, &t.Files, &t.Bytes); err != nil {
			return nil, fmt.Errorf("scan file totals: %w", err)
		}
		out[id] = t
	}
	return out, rows.Err()
}

// ForEachFileState calls fn for every tracked file ordered by path, streaming
// the rows instead of loading them all. A non-empty path limits the walk to
// that file or the tree below that folder. Returning an error from fn stops
// the walk and returns that error.
func ForEachFileState(ctx context.Context, q DBTX, path string, fn func(models.FileState) error) error {
	query := `SELECT id, source_id, file_path, last_hash, size, mod_time, updated_at FROM files_state`
	var args []any
	if path != "" {
		clause, a := SubtreeClause("file_path", path)
		query += " WHERE " + clause
		args = a
	}
	query += " ORDER BY file_path"

	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("list file states: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var s models.FileState
		var h int64
		if err := rows.Scan(&s.ID, &s.SourceID, &s.Path, &h, &s.Size, &s.ModTime, &s.UpdatedAt); err != nil {
			return fmt.Errorf("scan file state: %w", err)
		}
		s.Hash = models.HashFromDB(h)
		if err := fn(s); err != nil {
			return err
		}
	}
	return rows.Err()
}

// UpsertFileState inserts or updates the state of a file.
func UpsertFileState(ctx context.Context, q DBTX, s models.FileState) error {
	_, err := q.ExecContext(ctx,
		`INSERT INTO files_state(source_id, file_path, last_hash, size, mod_time, updated_at)
		 VALUES(?, ?, ?, ?, ?, ?)
		 ON CONFLICT(file_path) DO UPDATE SET
		   source_id  = excluded.source_id,
		   last_hash  = excluded.last_hash,
		   size       = excluded.size,
		   mod_time   = excluded.mod_time,
		   updated_at = excluded.updated_at`,
		s.SourceID, s.Path, models.HashToDB(s.Hash), s.Size, s.ModTime, s.UpdatedAt)
	if err != nil {
		return fmt.Errorf("upsert file state %q: %w", s.Path, err)
	}
	return nil
}

// DeleteFileState removes a tracked file.
func DeleteFileState(ctx context.Context, q DBTX, path string) error {
	if _, err := q.ExecContext(ctx, `DELETE FROM files_state WHERE file_path = ?`, path); err != nil {
		return fmt.Errorf("delete file state %q: %w", path, err)
	}
	return nil
}

// ---- history_log --------------------------------------------------------------

// InsertHistory appends an audit record.
func InsertHistory(ctx context.Context, q DBTX, e models.HistoryEntry) error {
	_, err := q.ExecContext(ctx,
		`INSERT INTO history_log(file_path, event_type, old_hash, new_hash, old_size, new_size, timestamp)
		 VALUES(?, ?, ?, ?, ?, ?, ?)`,
		e.Path, string(e.Event), hashArg(e.OldHash), hashArg(e.NewHash),
		sizeArg(e.OldSize), sizeArg(e.NewSize), e.Timestamp.UnixNano())
	if err != nil {
		return fmt.Errorf("insert history for %q: %w", e.Path, err)
	}
	return nil
}

func hashArg(h *uint64) any {
	if h == nil {
		return nil
	}
	return models.HashToDB(*h)
}

func sizeArg(s *int64) any {
	if s == nil {
		return nil
	}
	return *s
}

// QueryHistory returns history records, newest first. A non-empty path limits
// the result to that file or to everything below that folder. A limit <= 0
// means no limit.
func QueryHistory(ctx context.Context, q DBTX, path string, limit int) ([]models.HistoryEntry, error) {
	query := `SELECT id, file_path, event_type, old_hash, new_hash, old_size, new_size, timestamp
	          FROM history_log`
	var args []any
	if path != "" {
		clause, a := SubtreeClause("file_path", path)
		query += " WHERE " + clause
		args = append(args, a...)
	}
	query += " ORDER BY timestamp DESC, id DESC"
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query history: %w", err)
	}
	defer rows.Close()

	var out []models.HistoryEntry
	for rows.Next() {
		var e models.HistoryEntry
		var event string
		var oh, nh, os_, ns sql.NullInt64
		var ts int64
		if err := rows.Scan(&e.ID, &e.Path, &event, &oh, &nh, &os_, &ns, &ts); err != nil {
			return nil, fmt.Errorf("scan history: %w", err)
		}
		e.Event = models.EventType(event)
		if oh.Valid {
			v := models.HashFromDB(oh.Int64)
			e.OldHash = &v
		}
		if nh.Valid {
			v := models.HashFromDB(nh.Int64)
			e.NewHash = &v
		}
		if os_.Valid {
			v := os_.Int64
			e.OldSize = &v
		}
		if ns.Valid {
			v := ns.Int64
			e.NewSize = &v
		}
		e.Timestamp = time.Unix(0, ts).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// ClearHistory deletes history records and returns how many were removed. An
// empty path deletes everything; otherwise only records for that file or the
// tree below that folder.
func ClearHistory(ctx context.Context, q DBTX, path string) (int64, error) {
	query := `DELETE FROM history_log`
	var args []any
	if path != "" {
		clause, a := SubtreeClause("file_path", path)
		query += " WHERE " + clause
		args = a
	}
	res, err := q.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("clear history: %w", err)
	}
	return res.RowsAffected()
}

// ---- maintenance ------------------------------------------------------------

// HasData reports whether any watched source or history record exists.
func HasData(ctx context.Context, q DBTX) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM watched_sources) + (SELECT COUNT(*) FROM history_log)`).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("check database contents: %w", err)
	}
	return n > 0, nil
}

// ResetAll wipes every table's data (sources, file states, history, meta)
// while keeping the schema.
func ResetAll(ctx context.Context, q DBTX) error {
	for _, stmt := range []string{
		`DELETE FROM history_log`,
		`DELETE FROM files_state`,
		`DELETE FROM watched_sources`,
		`DELETE FROM meta`,
	} {
		if _, err := q.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("reset database: %w", err)
		}
	}
	return nil
}

// SubtreeClause returns a SQL condition (and its arguments) that matches the
// path itself and everything below it, for a folder or file path p.
//
// LIKE is deliberately avoided: '%' and '_' inside file names would act as
// wildcards, matching is case-insensitive for ASCII, and the index cannot be
// used. Instead the subtree is the half-open byte range
//
//	[p + sep, p + succ(sep))
//
// where succ(sep) is the next character after the separator ('/' -> '0',
// '\' -> ']'). Every string with the prefix p+sep lies inside that range and
// nothing else does, so the query works with the default BINARY collation and
// uses the UNIQUE/INDEX on the column.
//
// column must be a trusted identifier (it is not bound as a parameter).
func SubtreeClause(column, p string) (string, []any) {
	sep := filepath.Separator
	base := strings.TrimRight(p, string(sep))
	lo := base + string(sep)
	hi := base + string(sep+1)
	clause := fmt.Sprintf("(%[1]s = ? OR (%[1]s >= ? AND %[1]s < ?))", column)
	return clause, []any{p, lo, hi}
}
