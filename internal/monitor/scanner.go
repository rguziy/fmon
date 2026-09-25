package monitor

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/rguziy/fmon/internal/db"
	"github.com/rguziy/fmon/internal/hasher"
	"github.com/rguziy/fmon/internal/models"
)

// Report collects everything a scan found. Nothing is sent while the scan is
// running: all changes are gathered first and dispatched as one consolidated
// notification afterwards (see Notify).
type Report struct {
	Time    time.Time
	Host    string
	Changes []models.Change
	Alerts  []string // urgent messages, e.g. a tracked folder disappeared
	Notices []string // informational messages, e.g. a source came back
	Errors  []string // non-fatal problems (unreadable paths, ...)

	MissingSources int           // sources that are currently missing from disk
	Sources        int           // sources scanned in this run
	FilesScanned   int           // regular files examined
	FilesHashed    int           // files whose content had to be hashed
	Duration       time.Duration // wall-clock time of the scan
	Full           bool          // this was a --full scan (stat shortcut bypassed)
}

// Counts returns the number of ADDED, MODIFIED and DELETED changes.
func (r *Report) Counts() (added, modified, deleted int) {
	for _, c := range r.Changes {
		switch c.Event {
		case models.EventAdded:
			added++
		case models.EventModified:
			modified++
		case models.EventDeleted:
			deleted++
		}
	}
	return
}

// Empty reports whether there is nothing to notify about.
func (r *Report) Empty() bool {
	return len(r.Changes) == 0 && len(r.Alerts) == 0 && len(r.Notices) == 0
}

// HasErrors reports whether non-fatal errors occurred.
func (r *Report) HasErrors() bool { return len(r.Errors) > 0 }

// plannedSource is a watched source together with a flag telling the scanner
// to baseline it silently (no events) instead of diffing.
type plannedSource struct {
	src      models.Source
	baseline bool
}

// Scan performs one snapshot scan of all watched sources, or only the ones
// named in only (their exact watched path, as shown by "fmon list"). An empty
// only means every watched source, which is what unattended cron/systemd runs
// want. Filtering only changes which sources are diffed and hashed: fmon.toml
// is still reconciled against the database in full beforehand (see
// reconcile), and the audit history of every source is unaffected.
//
// The whole scan runs inside a single write transaction (BEGIN IMMEDIATE), so
// two concurrent runs cannot interleave and a failed or interrupted scan
// leaves the database untouched. Changes are committed together and only then
// returned to the caller, which dispatches notifications.
//
// With full=true, every regular file is stream-hashed regardless of its
// stored size and modification time (the normal pass-1 stat shortcut is
// skipped). This is slower but catches content that changed silently while
// size and mtime stayed the same, for example bits flipped by a failing
// disk without the filesystem updating the file's metadata.
func (a *App) Scan(ctx context.Context, full bool, only ...string) (*Report, error) {
	started := a.now()
	rep := &Report{Time: started, Host: a.Hostname, Full: full}

	tx, err := db.BeginImmediate(ctx, a.DB)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// A migration or a lost-and-recreated database asks for a full silent
	// re-baseline. A scan restricted to a subset of sources still honors it
	// (there is no partial baseline), but the caller should not normally
	// combine "only" with an upgrade in progress.
	baselineAll := false
	if v, ok, err := db.GetMeta(ctx, tx, db.MetaNeedsBaseline); err != nil {
		return nil, err
	} else if ok {
		baselineAll = true
		if v == db.BaselineDBMissing {
			rep.Notices = append(rep.Notices,
				"NOTICE: database was missing and has been recreated; baseline rebuilt, changes made before this scan are not reported")
		} else {
			rep.Notices = append(rep.Notices,
				"NOTICE: baseline rebuilt after upgrade; changes since the previous scan are not reported")
		}
	}

	planned, err := a.reconcile(ctx, tx, rep)
	if err != nil {
		return nil, err
	}

	if len(only) > 0 {
		planned, err = filterPlanned(ctx, tx, a.Cfg.Sources, planned, only)
		if err != nil {
			return nil, err
		}
	}

	excl := NewExcluder(a.Cfg.Exclude)
	for _, ps := range planned {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		scannedBefore, hashedBefore := rep.FilesScanned, rep.FilesHashed
		if err := a.scanSource(ctx, tx, ps.src, excl, ps.baseline || baselineAll, full, rep); err != nil {
			return nil, err
		}
		a.vlogf("%s %q: %d file(s) scanned, %d hashed", kindOf(ps.src.Type), ps.src.Path,
			rep.FilesScanned-scannedBefore, rep.FilesHashed-hashedBefore)
	}

	if baselineAll {
		if err := db.DeleteMeta(ctx, tx, db.MetaNeedsBaseline); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	rep.Sources = len(planned)
	rep.Duration = a.now().Sub(started)
	return rep, nil
}

// filterPlanned resolves each of the requested paths to a watched source and
// returns the subset of planned matching them, in the order the paths were
// given. It fails, naming the offending path, if a requested path is not
// watched at all or is only covered by a source rather than being one, so
// that "fmon scan /etc/ssh" cannot silently scan the whole of "/etc" instead.
// A path given more than once scans that source only once.
func filterPlanned(ctx context.Context, q db.DBTX, cfgSources []string, planned []plannedSource, only []string) ([]plannedSource, error) {
	byPath := make(map[string]plannedSource, len(planned))
	for _, ps := range planned {
		byPath[ps.src.Path] = ps
	}

	var out []plannedSource
	seen := make(map[string]bool, len(only))
	for _, arg := range only {
		target, err := resolveWatchedSource(ctx, q, cfgSources, arg)
		if err != nil {
			return nil, err
		}
		if seen[target] {
			continue
		}
		seen[target] = true
		ps, ok := byPath[target]
		if !ok {
			// Resolved to a real watched source, but it is not part of this
			// scan's plan: currently only possible for a source whose root
			// could not be baselined (see reconcile), which already added a
			// warning or a non-fatal error to the report.
			return nil, fmt.Errorf("%q could not be scanned; see the warnings above", target)
		}
		out = append(out, ps)
	}
	return out, nil
}

// reconcile makes watched_sources match the sources listed in fmon.toml, which
// is the source of truth, and returns the sources to scan in a stable order.
//
//   - a source in the database but not in the file is removed (with cascade);
//   - a source in the file but not in the database is baselined silently;
//   - sources that overlap an earlier one are skipped with a warning, because
//     files_state has a global UNIQUE(file_path).
func (a *App) reconcile(ctx context.Context, tx db.DBTX, rep *Report) ([]plannedSource, error) {
	dbSources, err := db.ListSources(ctx, tx)
	if err != nil {
		return nil, err
	}
	byPath := make(map[string]models.Source, len(dbSources))
	for _, s := range dbSources {
		byPath[s.Path] = s
	}

	// Normalize, de-duplicate and sort the configured paths. Sorting puts a
	// parent directory before its children, so the parent wins an overlap.
	seen := make(map[string]bool)
	var configured []string
	for _, p := range a.Cfg.Sources {
		np := filepath.Clean(p)
		if !filepath.IsAbs(np) {
			a.warnf("Configured source %q is not an absolute path; ignoring it", p)
			continue
		}
		if !seen[np] {
			seen[np] = true
			configured = append(configured, np)
		}
	}
	sort.Strings(configured)

	var accepted []string
	for _, p := range configured {
		conflict := ""
		for _, q := range accepted {
			if overlaps(p, q) {
				conflict = q
				break
			}
		}
		if conflict != "" {
			a.warnf("%q overlaps watched source %q; skipping it", p, conflict)
			continue
		}
		accepted = append(accepted, p)
	}

	keep := make(map[string]bool, len(accepted))
	for _, p := range accepted {
		keep[p] = true
	}
	for _, s := range dbSources {
		if !keep[s.Path] {
			if err := db.DeleteSource(ctx, tx, s.ID); err != nil {
				return nil, err
			}
			a.logger().Info("source is no longer configured; removed it and its file states", "path", s.Path)
		}
	}

	var out []plannedSource
	for _, p := range accepted {
		if s, ok := byPath[p]; ok {
			out = append(out, plannedSource{src: s})
			continue
		}

		// Listed in fmon.toml but unknown to the database (edited by hand, or
		// a previous "fmon add" was interrupted).
		info, err := os.Lstat(p)
		if err != nil {
			if isNotExist(err) {
				a.warnf("Configured path %q does not exist. Please remove it via fmon rm", p)
			} else {
				a.nonFatal(rep, "cannot stat configured path %q: %v", p, err)
			}
			continue
		}
		var typ models.SourceType
		switch {
		case info.IsDir():
			typ = models.SourceFolder
		case info.Mode().IsRegular():
			typ = models.SourceFile
		default:
			a.nonFatal(rep, "configured path %q is a symlink or special file; run fmon add to store its resolved path", p)
			continue
		}
		id, err := db.InsertSource(ctx, tx, p, typ, a.now())
		if err != nil {
			return nil, err
		}
		a.logger().Info("baselining source found in configuration", "path", p, "type", string(typ))
		out = append(out, plannedSource{
			src:      models.Source{ID: id, Path: p, Type: typ, Status: models.StatusActive},
			baseline: true,
		})
	}
	return out, nil
}

// sourceScan carries the per-source state of a scan.
type sourceScan struct {
	src        models.Source
	baseline   bool
	full       bool // --full: bypass the pass-1 stat shortcut, always hash
	states     map[string]models.FileState
	seen       map[string]struct{}
	unreadable []string // directories whose contents could not be listed
}

func kindOf(t models.SourceType) string {
	if t == models.SourceFile {
		return "file"
	}
	return "folder"
}

// scanSource diffs one source against the stored state. In baseline mode it
// only records the current state and reports nothing. Non-fatal problems are
// added to rep; the returned error is fatal (database failure, cancellation).
func (a *App) scanSource(ctx context.Context, tx db.DBTX, src models.Source, excl *Excluder, baseline, full bool, rep *Report) error {
	kind := kindOf(src.Type)

	info, err := os.Lstat(src.Path)
	switch {
	case err == nil:
	case isNotExist(err):
		return a.handleMissing(ctx, tx, src, rep)
	default:
		a.nonFatal(rep, "cannot stat %s %q: %v", kind, src.Path, err)
		return nil
	}

	if src.Status == models.StatusMissing {
		if err := db.SetSourceStatus(ctx, tx, src.ID, models.StatusActive); err != nil {
			return err
		}
		rep.Notices = append(rep.Notices, fmt.Sprintf("NOTICE: Tracked %s %q is back on disk", kind, src.Path))
	}

	// Guard against the root changing type (e.g. /opt/app became a symlink to
	// another release). Walking would silently find nothing and report every
	// file as DELETED, so the source is skipped and the state kept.
	if (src.Type == models.SourceFolder && !info.IsDir()) || (src.Type == models.SourceFile && !info.Mode().IsRegular()) {
		a.nonFatal(rep, "tracked %s %q is no longer a %s (replaced by another type of entry); skipping it, state kept", kind, src.Path, kind)
		return nil
	}

	states, err := db.LoadFileStates(ctx, tx, src.ID)
	if err != nil {
		return err
	}
	sc := &sourceScan{
		src:      src,
		baseline: baseline,
		full:     full,
		states:   states,
		seen:     make(map[string]struct{}, len(states)),
	}

	if src.Type == models.SourceFile {
		// An explicitly added file is always tracked; exclude patterns only
		// apply to files discovered inside folders.
		return a.checkFile(ctx, tx, rep, sc, src.Path, info)
	}

	walkErr := filepath.WalkDir(src.Path, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			// WalkDir reports a directory it could not list by calling the
			// function a second time for that directory with the error.
			if d != nil && d.IsDir() {
				sc.unreadable = append(sc.unreadable, p)
				a.nonFatal(rep, "cannot read directory %q: %v", p, werr)
				return nil
			}
			a.nonFatal(rep, "cannot access %q: %v", p, werr)
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			if p != src.Path && excl.Match(p) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // symlinks, devices, pipes, sockets are ignored entirely
		}
		if excl.Match(p) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			if isNotExist(err) {
				return nil // vanished while walking; reported as DELETED below
			}
			// Keep the stored state so an unreadable entry is never
			// mistaken for a deletion.
			sc.seen[p] = struct{}{}
			a.nonFatal(rep, "cannot stat %q: %v", p, err)
			return nil
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		return a.checkFile(ctx, tx, rep, sc, p, fi)
	})
	if walkErr != nil {
		return walkErr
	}

	return a.finishFolder(ctx, tx, rep, sc, excl)
}

// checkFile applies the double-pass check to one file.
//
// Pass 1 compares size and mtime with the stored values and skips unchanged
// files without reading them. Pass 2 stream-hashes files whose metadata
// changed: an unchanged hash silently refreshes the metadata, a different hash
// is a MODIFIED event, and an unknown file is ADDED.
func (a *App) checkFile(ctx context.Context, tx db.DBTX, rep *Report, sc *sourceScan, p string, fi fs.FileInfo) error {
	sc.seen[p] = struct{}{}
	rep.FilesScanned++

	// Metadata is captured BEFORE hashing. If the file is modified while it
	// is being read, the stored metadata no longer matches the file and the
	// next scan re-checks it, instead of missing the change.
	size := fi.Size()
	mtime := fi.ModTime().UnixNano()

	old, known := sc.states[p]
	if !sc.full && known && old.Size == size && old.ModTime == mtime {
		return nil // pass 1: unchanged (skipped entirely with --full)
	}

	sum, _, err := hasher.File(ctx, p)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		// e.g. permission denied. The stored state is kept, so this is never
		// reported as a deletion.
		a.nonFatal(rep, "%v", err)
		return nil
	}
	rep.FilesHashed++

	st := models.FileState{
		SourceID:  sc.src.ID,
		Path:      p,
		Hash:      sum,
		Size:      size,
		ModTime:   mtime,
		UpdatedAt: a.now().UnixNano(),
	}

	switch {
	case !known:
		if !sc.baseline {
			if err := a.emit(ctx, tx, rep, sc.src, models.Change{
				Event: models.EventAdded, Path: p, NewHash: sum, NewSize: size,
			}); err != nil {
				return err
			}
		}
	case old.Hash != sum:
		if err := a.emit(ctx, tx, rep, sc.src, models.Change{
			Event: models.EventModified, Path: p,
			OldHash: old.Hash, NewHash: sum, OldSize: old.Size, NewSize: size,
		}); err != nil {
			return err
		}
	}
	// Same hash with different size/mtime (e.g. "touch"): refresh silently.
	return db.UpsertFileState(ctx, tx, st)
}

// finishFolder handles stored files that were not seen during the walk:
// pruning rows that are now excluded and recording DELETED events.
func (a *App) finishFolder(ctx context.Context, tx db.DBTX, rep *Report, sc *sourceScan, excl *Excluder) error {
	var gone []string
	for p := range sc.states {
		if _, ok := sc.seen[p]; !ok {
			gone = append(gone, p)
		}
	}
	sort.Strings(gone)

	for _, p := range gone {
		// Files below a directory we could not list are unknown, not deleted.
		if underAny(p, sc.unreadable) {
			continue
		}
		if excl.Match(p) {
			// The exclude list grew since the last scan: forget the file
			// silently instead of reporting a deletion.
			a.logger().Info("pruned file state that now matches an exclude pattern", "path", p)
			if err := db.DeleteFileState(ctx, tx, p); err != nil {
				return err
			}
			continue
		}
		old := sc.states[p]
		if !sc.baseline {
			if err := a.emit(ctx, tx, rep, sc.src, models.Change{
				Event: models.EventDeleted, Path: p, OldHash: old.Hash, OldSize: old.Size,
			}); err != nil {
				return err
			}
		}
		if err := db.DeleteFileState(ctx, tx, p); err != nil {
			return err
		}
	}
	return nil
}

func underAny(p string, dirs []string) bool {
	for _, d := range dirs {
		if within(p, d) {
			return true
		}
	}
	return false
}

// emit records a change in the report and in the persistent history.
func (a *App) emit(ctx context.Context, tx db.DBTX, rep *Report, src models.Source, c models.Change) error {
	c.Source = src.Path
	rep.Changes = append(rep.Changes, c)

	e := models.HistoryEntry{Path: c.Path, Event: c.Event, Timestamp: a.now()}
	switch c.Event {
	case models.EventAdded:
		e.NewHash, e.NewSize = &c.NewHash, &c.NewSize
	case models.EventModified:
		e.OldHash, e.NewHash, e.OldSize, e.NewSize = &c.OldHash, &c.NewHash, &c.OldSize, &c.NewSize
	case models.EventDeleted:
		e.OldHash, e.OldSize = &c.OldHash, &c.OldSize
	}
	return db.InsertHistory(ctx, tx, e)
}

// handleMissing deals with a watched source whose root is gone.
//
// The stored file states are deliberately kept: a vanished root is often an
// unmounted NFS share or USB disk, and wiping the baseline would report every
// file as ADDED once it reappears. The first time a source is found missing
// it is marked "missing", one DELETED record is written for the source path,
// and an ALERT joins the consolidated report. Afterwards only a warning is
// printed on each run until the source is removed with "fmon rm" or returns.
func (a *App) handleMissing(ctx context.Context, tx db.DBTX, src models.Source, rep *Report) error {
	kind := kindOf(src.Type)
	rep.MissingSources++

	if src.Status == models.StatusMissing {
		a.warnf("Configured %s %q does not exist. Please remove it via fmon rm", kind, src.Path)
		return nil
	}

	if err := db.SetSourceStatus(ctx, tx, src.ID, models.StatusMissing); err != nil {
		return err
	}
	if err := db.InsertHistory(ctx, tx, models.HistoryEntry{
		Path: src.Path, Event: models.EventDeleted, Timestamp: a.now(),
	}); err != nil {
		return err
	}
	msg := fmt.Sprintf("ALERT: Tracked %s %q was deleted from disk!", kind, src.Path)
	rep.Alerts = append(rep.Alerts, msg)
	a.logger().Warn(msg)
	return nil
}
