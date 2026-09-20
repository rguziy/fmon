// Package models contains the data structures shared by all fmon packages.
package models

import (
	"fmt"
	"time"
)

// SourceType is the kind of a watched source.
type SourceType string

// Supported source types.
const (
	SourceFile   SourceType = "file"
	SourceFolder SourceType = "folder"
)

// SourceStatus describes whether a watched source currently exists on disk.
type SourceStatus string

// Supported source statuses.
const (
	StatusActive  SourceStatus = "active"
	StatusMissing SourceStatus = "missing"
)

// EventType is the kind of change recorded in the history log.
type EventType string

// Supported event types.
const (
	EventAdded    EventType = "ADDED"
	EventModified EventType = "MODIFIED"
	EventDeleted  EventType = "DELETED"
)

// Source is a watched file or folder (a row of watched_sources).
type Source struct {
	ID        int64
	Path      string
	Type      SourceType
	Status    SourceStatus
	CreatedAt time.Time
}

// FileState is the last known state of a tracked file (a row of files_state).
//
// ModTime and UpdatedAt are Unix timestamps in nanoseconds (UTC). Keeping them
// as plain integers avoids time.Time conversions in the hot scan loop and
// mirrors the on-disk representation.
type FileState struct {
	ID        int64
	SourceID  int64
	Path      string
	Hash      uint64
	Size      int64
	ModTime   int64
	UpdatedAt int64
}

// HistoryEntry is a persisted audit record (a row of history_log).
// Hash and size fields are nil when they do not apply to the event
// (for example, the old hash of an ADDED event).
type HistoryEntry struct {
	ID        int64
	Path      string
	Event     EventType
	OldHash   *uint64
	NewHash   *uint64
	OldSize   *int64
	NewSize   *int64
	Timestamp time.Time
}

// Change is a single detected difference collected during a scan.
type Change struct {
	Event   EventType
	Path    string
	Source  string // path of the watched source that contains Path
	OldHash uint64
	NewHash uint64
	OldSize int64
	NewSize int64
}

// HashToDB converts an xxHash64 sum into the value stored in an SQLite
// INTEGER column. SQLite integers are signed 64-bit, so the uint64 bit pattern
// is reinterpreted (not converted numerically). database/sql refuses to bind
// uint64 values that have the high bit set, hence this explicit helper.
func HashToDB(h uint64) int64 { return int64(h) }

// HashFromDB is the inverse of HashToDB.
func HashFromDB(v int64) uint64 { return uint64(v) }

// FormatHash renders a hash as 16 lowercase hexadecimal digits.
func FormatHash(h uint64) string { return fmt.Sprintf("%016x", h) }
