package db

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rguziy/fmon/internal/models"
)

func newDB(t *testing.T, dir string) string {
	t.Helper()
	return filepath.Join(dir, "fmon.db")
}

func TestOpenAndMigrate(t *testing.T) {
	ctx := context.Background()
	// A directory with a space exercises the DSN escaping.
	path := newDB(t, filepath.Join(t.TempDir(), "with space"))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}

	d, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	v, err := userVersion(ctx, d)
	if err != nil || v != SchemaVersion() {
		t.Fatalf("user_version = %d (err %v), want %d", v, err, SchemaVersion())
	}

	var fk int
	if err := d.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("foreign_keys = %d (err %v), want 1", fk, err)
	}

	// Re-opening is a no-op.
	d2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	d2.Close()
}

func TestNewerSchemaRejected(t *testing.T) {
	ctx := context.Background()
	path := newDB(t, t.TempDir())
	d, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, "PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	d.Close()

	if _, err := Open(ctx, path); !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("err = %v, want ErrNewerSchema", err)
	}
}

func TestFileStateHighBitHash(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, newDB(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	id, err := InsertSource(ctx, d, "/src", models.SourceFolder, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	const high = uint64(0xffffffffffffffff)
	if err := UpsertFileState(ctx, d, models.FileState{SourceID: id, Path: "/src/a", Hash: high, Size: 5, ModTime: 7, UpdatedAt: 9}); err != nil {
		t.Fatal(err)
	}
	// Upsert again with a different hash: must update, not fail on UNIQUE.
	if err := UpsertFileState(ctx, d, models.FileState{SourceID: id, Path: "/src/a", Hash: 0x8000000000000001, Size: 6, ModTime: 8, UpdatedAt: 10}); err != nil {
		t.Fatal(err)
	}
	got, err := LoadFileStates(ctx, d, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["/src/a"].Hash != 0x8000000000000001 || got["/src/a"].Size != 6 {
		t.Fatalf("unexpected state: %+v", got)
	}
}

func TestCascadeKeepsHistory(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, newDB(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	id, _ := InsertSource(ctx, d, "/src", models.SourceFolder, time.Now())
	_ = UpsertFileState(ctx, d, models.FileState{SourceID: id, Path: "/src/a", Hash: 1, Size: 1, ModTime: 1, UpdatedAt: 1})
	h := uint64(3)
	sz := int64(4)
	if err := InsertHistory(ctx, d, models.HistoryEntry{Path: "/src/a", Event: models.EventAdded, NewHash: &h, NewSize: &sz, Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}

	if err := DeleteSource(ctx, d, id); err != nil {
		t.Fatal(err)
	}
	states, _ := LoadFileStates(ctx, d, id)
	if len(states) != 0 {
		t.Errorf("files_state rows survived the cascade: %v", states)
	}
	hist, err := QueryHistory(ctx, d, "", 0)
	if err != nil || len(hist) != 1 {
		t.Fatalf("history = %v (err %v), want 1 record", hist, err)
	}
	if hist[0].NewHash == nil || *hist[0].NewHash != 3 || hist[0].OldHash != nil {
		t.Errorf("history nullability wrong: %+v", hist[0])
	}
}

func TestSubtreeClause(t *testing.T) {
	ctx := context.Background()
	d, err := Open(ctx, newDB(t, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	sep := string(filepath.Separator)
	p := func(parts ...string) string { return sep + strings.Join(parts, sep) }

	inside := []string{
		p("data", "a"),
		p("data", "a", "x.txt"),
		p("data", "a", "sub", "100%_real.txt"),
		p("data", "a", "sub", "deep", "f"),
	}
	outside := []string{
		p("data", "ab"),           // shares the textual prefix, different folder
		p("data", "ab", "x.txt"),  // same
		p("data", "A", "x.txt"),   // different case
		p("data", "a.txt"),        // sibling file
		p("data0"),                // sorts right after the subtree range
		p("data", "a_b", "x.txt"), // '_' must not act as a wildcard
		p("data"),
	}
	now := time.Now()
	for i, path := range append(append([]string{}, inside...), outside...) {
		e := models.HistoryEntry{Path: path, Event: models.EventDeleted, Timestamp: now.Add(time.Duration(i))}
		if err := InsertHistory(ctx, d, e); err != nil {
			t.Fatal(err)
		}
	}

	got, err := QueryHistory(ctx, d, p("data", "a"), 0)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, e := range got {
		paths = append(paths, e.Path)
	}
	if len(paths) != len(inside) {
		t.Fatalf("matched %d paths %v, want %d %v", len(paths), paths, len(inside), inside)
	}
	for _, want := range inside {
		found := false
		for _, g := range paths {
			if g == want {
				found = true
			}
		}
		if !found {
			t.Errorf("missing %q in %v", want, paths)
		}
	}

	// A file path matches only itself.
	one, _ := QueryHistory(ctx, d, p("data", "a.txt"), 0)
	if len(one) != 1 || one[0].Path != p("data", "a.txt") {
		t.Errorf("file match = %v", one)
	}

	// Newest first, and limit works.
	limited, _ := QueryHistory(ctx, d, "", 3)
	if len(limited) != 3 || !limited[0].Timestamp.After(limited[1].Timestamp) {
		t.Errorf("limit/order wrong: %v", limited)
	}

	// clear --path removes exactly the subtree.
	n, err := ClearHistory(ctx, d, p("data", "a"))
	if err != nil || int(n) != len(inside) {
		t.Fatalf("ClearHistory removed %d (err %v), want %d", n, err, len(inside))
	}
	rest, _ := QueryHistory(ctx, d, "", 0)
	if len(rest) != len(outside) {
		t.Errorf("remaining = %d, want %d", len(rest), len(outside))
	}
}

func TestBeginImmediateLocked(t *testing.T) {
	old := busyTimeoutMS
	busyTimeoutMS = 100
	defer func() { busyTimeoutMS = old }()

	ctx := context.Background()
	path := newDB(t, t.TempDir())
	d1, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer d1.Close()
	d2, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()

	tx, err := BeginImmediate(ctx, d1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BeginImmediate(ctx, d2); !errors.Is(err, ErrLocked) {
		t.Fatalf("second BeginImmediate err = %v, want ErrLocked", err)
	}

	// Readers are not blocked while a writer is active (WAL).
	if _, err := QueryHistory(ctx, d2, "", 1); err != nil {
		t.Errorf("read during write failed: %v", err)
	}

	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx2, err := BeginImmediate(ctx, d2)
	if err != nil {
		t.Fatalf("lock not released after commit: %v", err)
	}
	if err := tx2.Rollback(); err != nil {
		t.Fatal(err)
	}
}
