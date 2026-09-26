package monitor

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rguziy/fmon/internal/db"
	"github.com/rguziy/fmon/internal/models"
)

func TestScanDetectsAddedModifiedDeleted(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.write(t, "keep.txt", "same")
	e.write(t, "edit.txt", "before")
	e.write(t, "gone.txt", "bye")
	e.write(t, "ignored.tmp", "excluded by pattern")

	if err := e.app.Add(ctx, e.root); err != nil {
		t.Fatal(err)
	}
	if rep := e.scan(t); !rep.Empty() || rep.FilesHashed != 0 {
		t.Fatalf("clean tree must produce an empty report without hashing, got %v (hashed %d)", e.changes(rep), rep.FilesHashed)
	}

	e.write(t, "edit.txt", "after!")   // different size
	e.write(t, "new.txt", "hello")     // new file
	e.write(t, "sub/deep.txt", "deep") // new file in a new subdirectory
	e.write(t, "new.tmp", "excluded")  // excluded new file
	if err := os.Remove(filepath.Join(e.root, "gone.txt")); err != nil {
		t.Fatal(err)
	}

	rep := e.scan(t)
	want := []string{"MODIFIED edit.txt", "ADDED new.txt", "ADDED sub/deep.txt", "DELETED gone.txt"}
	if got := e.changes(rep); !sameSet(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}

	// The same scan repeated finds nothing: state and history were committed.
	if rep := e.scan(t); !rep.Empty() {
		t.Fatalf("second scan not empty: %v", e.changes(rep))
	}

	hist, err := db.QueryHistory(ctx, e.app.DB, e.root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 4 {
		t.Fatalf("history has %d records, want 4", len(hist))
	}
	for _, h := range hist {
		if h.Event == models.EventModified && (h.OldHash == nil || h.NewHash == nil || *h.OldHash == *h.NewHash || *h.OldSize != 6 || *h.NewSize != 6) {
			// "before" is 6 bytes and "after!" is 6 bytes as well.
			t.Errorf("bad MODIFIED record: %+v", h)
		}
	}
}

func TestScanSameSizeContentChangeIsDetected(t *testing.T) {
	e := newEnv(t)
	e.write(t, "f.txt", "aaaa")
	if err := e.app.Add(context.Background(), e.root); err != nil {
		t.Fatal(err)
	}
	e.write(t, "f.txt", "bbbb") // same size, new mtime, different content
	if got := e.changes(e.scan(t)); !sameSet(got, []string{"MODIFIED f.txt"}) {
		t.Fatalf("changes = %v", got)
	}
}

func TestScanMtimeOnlyChangeIsSilent(t *testing.T) {
	e := newEnv(t)
	p := e.write(t, "f.txt", "content")
	if err := e.app.Add(context.Background(), e.root); err != nil {
		t.Fatal(err)
	}

	e.clock = e.clock.Add(time.Hour)
	if err := os.Chtimes(p, e.clock, e.clock); err != nil { // like "touch"
		t.Fatal(err)
	}
	rep := e.scan(t)
	if !rep.Empty() || rep.FilesHashed != 1 {
		t.Fatalf("touch must be hashed (pass 2) but not reported: changes=%v hashed=%d", e.changes(rep), rep.FilesHashed)
	}
	// Metadata was refreshed, so the next scan skips hashing entirely.
	if rep := e.scan(t); rep.FilesHashed != 0 {
		t.Fatalf("metadata was not refreshed: hashed %d", rep.FilesHashed)
	}
}

func TestScanSingleFileSource(t *testing.T) {
	e := newEnv(t)
	p := e.write(t, "app.yml", "v: 1")
	if err := e.app.Add(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	e.write(t, "other.txt", "not watched")
	if rep := e.scan(t); !rep.Empty() {
		t.Fatalf("unexpected: %v", e.changes(rep))
	}
	e.write(t, "app.yml", "v: 2")
	if got := e.changes(e.scan(t)); !sameSet(got, []string{"MODIFIED app.yml"}) {
		t.Fatalf("changes = %v", got)
	}
}

func TestMissingFolderAlertsOnceAndKeepsState(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	src := filepath.Join(e.root, "share")
	e.write(t, "share/a.txt", "a")
	e.write(t, "share/b.txt", "b")
	if err := e.app.Add(ctx, src); err != nil {
		t.Fatal(err)
	}
	e.write(t, "other/x.txt", "x")
	other := filepath.Join(e.root, "other")
	if err := e.app.Add(ctx, other); err != nil {
		t.Fatal(err)
	}

	// The folder disappears (think: unmounted share) while another source
	// changes in the same scan: one consolidated report, scan continues.
	moved := src + ".away"
	if err := os.Rename(src, moved); err != nil {
		t.Fatal(err)
	}
	e.write(t, "other/y.txt", "y")

	rep := e.scan(t)
	if len(rep.Alerts) != 1 || !strings.Contains(rep.Alerts[0], "was deleted from disk!") || !strings.Contains(rep.Alerts[0], "Tracked folder") {
		t.Fatalf("alerts = %v", rep.Alerts)
	}
	if got := e.changes(rep); !sameSet(got, []string{"ADDED other/y.txt"}) {
		t.Fatalf("other source must still be scanned, changes = %v", got)
	}
	if rep.MissingSources != 1 {
		t.Errorf("MissingSources = %d", rep.MissingSources)
	}

	// The audit trail has a DELETED record for the folder itself.
	hist, _ := db.QueryHistory(ctx, e.app.DB, src, 0)
	if len(hist) != 1 || hist[0].Event != models.EventDeleted || hist[0].Path != src {
		t.Fatalf("history for missing folder = %+v", hist)
	}

	// Second run: no alert, no notification, only a warning on stderr.
	e.stderr.Reset()
	rep = e.scan(t)
	if !rep.Empty() || len(rep.Alerts) != 0 {
		t.Fatalf("second scan must be quiet, got alerts=%v changes=%v", rep.Alerts, e.changes(rep))
	}
	if w := e.stderr.String(); !strings.Contains(w, "[fmon] WARNING: Configured folder") || !strings.Contains(w, "Please remove it via fmon rm") {
		t.Fatalf("expected the warning, stderr = %q", w)
	}

	// State was retained: when the folder returns, unchanged files produce
	// nothing (not ADDED) and a modified one is MODIFIED.
	if err := os.Rename(moved, src); err != nil {
		t.Fatal(err)
	}
	e.write(t, "share/b.txt", "b changed")
	rep = e.scan(t)
	if len(rep.Notices) != 1 || !strings.Contains(rep.Notices[0], "is back on disk") {
		t.Fatalf("notices = %v", rep.Notices)
	}
	if got := e.changes(rep); !sameSet(got, []string{"MODIFIED share/b.txt"}) {
		t.Fatalf("changes after recovery = %v", got)
	}
}

func TestExcludeGrowthPrunesSilently(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.write(t, "a.txt", "a")
	e.write(t, "b.log", "b")
	if err := e.app.Add(ctx, e.root); err != nil {
		t.Fatal(err)
	}

	e.app.Cfg.Exclude = append(e.app.Cfg.Exclude, "*.log")
	if rep := e.scan(t); !rep.Empty() {
		t.Fatalf("a newly excluded file must not be reported as deleted: %v", e.changes(rep))
	}
	hist, _ := db.QueryHistory(ctx, e.app.DB, "", 0)
	if len(hist) != 0 {
		t.Fatalf("history must stay empty, got %+v", hist)
	}
	// And it stays untracked even if modified afterwards.
	e.write(t, "b.log", "changed")
	if rep := e.scan(t); !rep.Empty() {
		t.Fatalf("excluded file reported: %v", e.changes(rep))
	}
}

func TestSymlinksAndSpecialFilesAreIgnored(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}
	e := newEnv(t)
	e.write(t, "real.txt", "real")
	if err := os.Symlink(filepath.Join(e.root, "real.txt"), filepath.Join(e.root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(e.root, filepath.Join(e.root, "loop")); err != nil {
		t.Fatal(err)
	}
	if err := e.app.Add(context.Background(), e.root); err != nil {
		t.Fatal(err)
	}
	states, _ := db.LoadFileStates(context.Background(), e.app.DB, 1)
	if len(states) != 1 {
		t.Fatalf("only the regular file may be tracked, got %d states", len(states))
	}
}

func TestUnreadableDirectoryIsNotReportedAsDeleted(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a non-root Unix user for permission errors")
	}
	e := newEnv(t)
	e.write(t, "open/a.txt", "a")
	e.write(t, "locked/b.txt", "b")
	if err := e.app.Add(context.Background(), e.root); err != nil {
		t.Fatal(err)
	}

	locked := filepath.Join(e.root, "locked")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(locked, 0o755)

	rep := e.scan(t)
	if len(rep.Changes) != 0 {
		t.Fatalf("files below an unreadable directory must not be DELETED: %v", e.changes(rep))
	}
	if !rep.HasErrors() {
		t.Fatal("expected a non-fatal error to be recorded")
	}
}

func TestReconcileConfigAndDatabase(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.write(t, "one/a.txt", "a")
	e.write(t, "two/b.txt", "b")
	one, two := filepath.Join(e.root, "one"), filepath.Join(e.root, "two")

	// A source added by hand to fmon.toml is baselined silently.
	e.app.Cfg.Sources = []string{one}
	if rep := e.scan(t); !rep.Empty() {
		t.Fatalf("baselining must be silent: %v", e.changes(rep))
	}
	if srcs, _ := db.ListSources(ctx, e.app.DB); len(srcs) != 1 || srcs[0].Path != one {
		t.Fatalf("sources = %+v", srcs)
	}
	e.write(t, "one/c.txt", "c")
	if got := e.changes(e.scan(t)); !sameSet(got, []string{"ADDED one/c.txt"}) {
		t.Fatalf("changes = %v", got)
	}

	// Removing a source from fmon.toml removes it (and its states) from the DB.
	e.app.Cfg.Sources = []string{two}
	e.scan(t)
	srcs, _ := db.ListSources(ctx, e.app.DB)
	if len(srcs) != 1 || srcs[0].Path != two {
		t.Fatalf("sources = %+v", srcs)
	}
	var total int
	if err := e.app.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM files_state WHERE file_path LIKE ?`, one+"%").Scan(&total); err != nil || total != 0 {
		t.Fatalf("states of the removed source survived: %d (err %v)", total, err)
	}

	// Overlapping entries in fmon.toml: the parent wins, the child is skipped.
	e.app.Cfg.Sources = []string{e.root, two}
	e.stderr.Reset()
	e.scan(t)
	if !strings.Contains(e.stderr.String(), "overlaps watched source") {
		t.Fatalf("expected an overlap warning, stderr = %q", e.stderr.String())
	}
	if srcs, _ := db.ListSources(ctx, e.app.DB); len(srcs) != 1 || srcs[0].Path != e.root {
		t.Fatalf("sources = %+v", srcs)
	}
}

func TestNeedsBaselineIsSilentAndClearsFlag(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.write(t, "a.txt", "a")
	if err := e.app.Add(ctx, e.root); err != nil {
		t.Fatal(err)
	}

	// Simulate a migration that invalidated files_state.
	if _, err := e.app.DB.ExecContext(ctx, `DELETE FROM files_state`); err != nil {
		t.Fatal(err)
	}
	if err := db.SetMeta(ctx, e.app.DB, db.MetaNeedsBaseline, "1"); err != nil {
		t.Fatal(err)
	}

	rep := e.scan(t)
	if len(rep.Changes) != 0 || len(rep.Notices) != 1 || !strings.Contains(rep.Notices[0], "baseline rebuilt after upgrade") {
		t.Fatalf("changes=%v notices=%v", e.changes(rep), rep.Notices)
	}
	if _, ok, _ := db.GetMeta(ctx, e.app.DB, db.MetaNeedsBaseline); ok {
		t.Fatal("needs_baseline flag was not cleared")
	}
	// From now on the tree is tracked normally.
	e.write(t, "a.txt", "changed")
	if got := e.changes(e.scan(t)); !sameSet(got, []string{"MODIFIED a.txt"}) {
		t.Fatalf("changes = %v", got)
	}
}

func TestScanRollsBackWhenCancelled(t *testing.T) {
	e := newEnv(t)
	e.write(t, "a.txt", "a")
	if err := e.app.Add(context.Background(), e.root); err != nil {
		t.Fatal(err)
	}
	e.write(t, "b.txt", "b")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.app.Scan(ctx, false); err == nil {
		t.Fatal("cancelled scan must fail")
	}
	// Nothing was recorded, so the change is still reported next time.
	if got := e.changes(e.scan(t)); !sameSet(got, []string{"ADDED b.txt"}) {
		t.Fatalf("changes = %v", got)
	}
}

func TestFullScanCatchesSilentCorruption(t *testing.T) {
	e := newEnv(t)
	p := e.write(t, "f.txt", "original content")
	if err := e.app.Add(context.Background(), e.root); err != nil {
		t.Fatal(err)
	}
	mtime := e.clock

	// Simulate corruption that a failing disk can produce: the bytes change
	// but the filesystem metadata (size, mtime) does not, because the write
	// path never goes through a normal write() that would touch them.
	corrupted := "corrupted-conten" // same length (17 bytes) as "original content"
	if len(corrupted) != len("original content") {
		t.Fatal("test fixture: sizes must match")
	}
	if err := os.WriteFile(p, []byte(corrupted), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}

	// A normal scan trusts the stat shortcut and misses it.
	if rep := e.scan(t); !rep.Empty() || rep.FilesHashed != 0 {
		t.Fatalf("normal scan should skip an unchanged-metadata file: changes=%v hashed=%d", e.changes(rep), rep.FilesHashed)
	}

	// --full bypasses the shortcut and catches it.
	rep := e.scanFull(t)
	if got := e.changes(rep); !sameSet(got, []string{"MODIFIED f.txt"}) {
		t.Fatalf("full scan changes = %v", got)
	}
	if rep.FilesHashed != 1 || !rep.Full {
		t.Fatalf("hashed=%d full=%v", rep.FilesHashed, rep.Full)
	}

	// After --full updates the stored hash, a normal scan is clean again.
	if rep := e.scan(t); !rep.Empty() {
		t.Fatalf("scan after full repair should be clean: %v", e.changes(rep))
	}
}

func TestFullScanHashesEverythingEvenUnchanged(t *testing.T) {
	e := newEnv(t)
	e.write(t, "a.txt", "a")
	e.write(t, "b.txt", "b")
	if err := e.app.Add(context.Background(), e.root); err != nil {
		t.Fatal(err)
	}
	if rep := e.scan(t); rep.FilesHashed != 0 {
		t.Fatalf("baseline scan afterwards should skip both: hashed=%d", rep.FilesHashed)
	}
	rep := e.scanFull(t)
	if rep.FilesHashed != 2 || !rep.Empty() {
		t.Fatalf("full scan of an untouched tree: hashed=%d changes=%v", rep.FilesHashed, e.changes(rep))
	}
}

func TestScanOnlySelectedSources(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.write(t, "photo/a.jpg", "a")
	e.write(t, "music/b.mp3", "b")
	photo, music := filepath.Join(e.root, "photo"), filepath.Join(e.root, "music")
	for _, p := range []string{photo, music} {
		if err := e.app.Add(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	e.write(t, "photo/a.jpg", "a changed")
	e.write(t, "music/b.mp3", "b changed")

	// Scanning only "photo" must not touch "music"'s state or history.
	rep, err := e.app.Scan(ctx, false, photo)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Sources != 1 {
		t.Fatalf("Sources = %d, want 1", rep.Sources)
	}
	if got := e.changes(rep); !sameSet(got, []string{"MODIFIED photo/a.jpg"}) {
		t.Fatalf("changes = %v", got)
	}

	// A full, unfiltered scan then finds only the still-unreported music change.
	rep2 := e.scan(t)
	if got := e.changes(rep2); !sameSet(got, []string{"MODIFIED music/b.mp3"}) {
		t.Fatalf("second scan changes = %v", got)
	}
}

func TestScanMultiplePathsAndDeduplication(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.write(t, "photo/a.jpg", "a")
	e.write(t, "music/b.mp3", "b")
	e.write(t, "films/c.mkv", "c")
	photo, music, films := filepath.Join(e.root, "photo"), filepath.Join(e.root, "music"), filepath.Join(e.root, "films")
	for _, p := range []string{photo, music, films} {
		if err := e.app.Add(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	e.write(t, "photo/a.jpg", "a2")
	e.write(t, "music/b.mp3", "b2")
	e.write(t, "films/c.mkv", "c2")

	// Two of the three, one of them repeated: scanned once, films untouched.
	rep, err := e.app.Scan(ctx, false, photo, music, photo)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Sources != 2 {
		t.Fatalf("Sources = %d, want 2", rep.Sources)
	}
	if got := e.changes(rep); !sameSet(got, []string{"MODIFIED photo/a.jpg", "MODIFIED music/b.mp3"}) {
		t.Fatalf("changes = %v", got)
	}

	rep2 := e.scan(t)
	if got := e.changes(rep2); !sameSet(got, []string{"MODIFIED films/c.mkv"}) {
		t.Fatalf("films must still be pending: %v", got)
	}
}

func TestScanFullWithSelectedSource(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.write(t, "photo/a.jpg", "original content")
	photo := filepath.Join(e.root, "photo")
	if err := e.app.Add(ctx, photo); err != nil {
		t.Fatal(err)
	}
	e.write(t, "other/x.txt", "x")
	other := filepath.Join(e.root, "other")
	if err := e.app.Add(ctx, other); err != nil {
		t.Fatal(err)
	}

	mtime := e.clock
	corrupted := "corrupted-conten" // same length as "original content"
	if err := os.WriteFile(p, []byte(corrupted), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}

	rep, err := e.app.Scan(ctx, true, photo)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Full || rep.Sources != 1 {
		t.Fatalf("Full=%v Sources=%d", rep.Full, rep.Sources)
	}
	if got := e.changes(rep); !sameSet(got, []string{"MODIFIED photo/a.jpg"}) {
		t.Fatalf("changes = %v", got)
	}
}

func TestScanRejectsPathNotWatched(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.write(t, "photo/a.jpg", "a")
	photo := filepath.Join(e.root, "photo")
	if err := e.app.Add(ctx, photo); err != nil {
		t.Fatal(err)
	}

	// A path that was never added at all.
	if _, err := e.app.Scan(ctx, false, filepath.Join(e.root, "nope")); err == nil {
		t.Fatal("scanning an unwatched path must fail")
	}

	// A path inside a watched source, but not the source itself: fails and
	// names the covering source, so the user is not misled into thinking the
	// subtree alone was scanned (fmon has no per-file granularity).
	sub := filepath.Join(photo, "a.jpg")
	if _, err := e.app.Scan(ctx, false, sub); err == nil || !strings.Contains(err.Error(), "covered by") || !strings.Contains(err.Error(), strconv.Quote(photo)) {
		t.Fatalf("err = %v", err)
	}

	// Nothing was scanned or committed by the failed attempt.
	if rep := e.scan(t); !rep.Empty() {
		t.Fatalf("a rejected scan must not have committed anything: %v", e.changes(rep))
	}

	// One good path plus one bad path: the whole call fails, nothing scanned.
	e.write(t, "photo/a.jpg", "changed")
	if _, err := e.app.Scan(ctx, false, photo, filepath.Join(e.root, "nope")); err == nil {
		t.Fatal("a request mixing a valid and an invalid path must fail entirely")
	}
	hist, _ := db.QueryHistory(ctx, e.app.DB, "", 0)
	if len(hist) != 0 {
		t.Fatalf("nothing should have been committed: %d history record(s)", len(hist))
	}
}
