package monitor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rguziy/fmon/internal/db"
	"github.com/rguziy/fmon/internal/models"
)

// ErrAborted is returned when the user declines a confirmation prompt.
var ErrAborted = errors.New("aborted")

// confirm asks a [y/N] question. With yes=true it returns immediately. When
// stdin is not a terminal (cron, pipes) it refuses instead of guessing.
func (a *App) confirm(prompt string, yes bool) (bool, error) {
	if yes {
		return true, nil
	}
	if !a.stdinIsTerminal() {
		return false, errors.New("confirmation required: re-run with --yes (stdin is not a terminal)")
	}
	fmt.Fprintf(a.stdout(), "%s [y/N]: ", prompt)
	line, err := bufio.NewReader(a.Stdin).ReadString('\n')
	if err != nil && line == "" {
		// EOF: stdin looked like a terminal but delivers no input (for
		// example /dev/null). Do not treat that as consent or as a refusal.
		return false, errors.New("no confirmation received: re-run with --yes")
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes", nil
}

// Init wipes the database (sources, file states and history) and clears the
// source list in fmon.toml, keeping all other settings. Also used by
// "clear --all".
func (a *App) Init(ctx context.Context, yes bool) error {
	has, err := db.HasData(ctx, a.DB)
	if err != nil {
		return err
	}
	if has || len(a.Cfg.Sources) > 0 {
		ok, err := a.confirm("This deletes all watched sources, file states and the change history. Continue?", yes)
		if err != nil {
			return err
		}
		if !ok {
			return ErrAborted
		}
	}

	tx, err := db.BeginImmediate(ctx, a.DB)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := db.ResetAll(ctx, tx); err != nil {
		return err
	}
	a.Cfg.ClearSources()
	// Saved before the commit: if writing fmon.toml fails, nothing changed.
	if err := a.Cfg.Save(a.Paths.Config); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	a.infof("[fmon] Initialized: database reset, configuration at %s\n", a.Paths.Config)
	return nil
}

// Add starts watching a file or folder and indexes its current contents as
// the baseline (hashing every regular file).
func (a *App) Add(ctx context.Context, arg string) error {
	started := a.now()
	abs, err := normalizePath(arg)
	if err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return fmt.Errorf("cannot access %q: %w", abs, err)
	}
	resolved = filepath.Clean(resolved)
	if !equalPath(resolved, abs) {
		a.infof("[fmon] %q resolves to %q; watching the resolved path\n", abs, resolved)
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("cannot access %q: %w", resolved, err)
	}
	var typ models.SourceType
	switch {
	case info.IsDir():
		typ = models.SourceFolder
	case info.Mode().IsRegular():
		typ = models.SourceFile
	default:
		return fmt.Errorf("%q is neither a regular file nor a directory", resolved)
	}

	tx, err := db.BeginImmediate(ctx, a.DB)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Overlap rule: refuse a path that equals, lies inside, or contains an
	// existing source (files_state paths are globally unique).
	existing, err := db.ListSources(ctx, tx)
	if err != nil {
		return err
	}
	known := append([]string(nil), a.Cfg.Sources...)
	for _, s := range existing {
		known = append(known, s.Path)
	}
	for _, other := range known {
		if overlaps(resolved, other) {
			a.warnf("%s overlaps watched source %s; nothing added", resolved, other)
			return nil
		}
	}

	id, err := db.InsertSource(ctx, tx, resolved, typ, a.now())
	if err != nil {
		return err
	}
	src := models.Source{ID: id, Path: resolved, Type: typ, Status: models.StatusActive}

	// A new source is always hashed in full regardless of full=false/true:
	// baseline mode has no prior state to compare stat metadata against, so
	// scanSource hashes every file unconditionally either way.
	rep := &Report{Time: a.now(), Host: a.Hostname}
	if err := a.scanSource(ctx, tx, src, NewExcluder(a.Cfg.Exclude), true, false, rep); err != nil {
		return err
	}

	a.Cfg.AddSource(resolved)
	if err := a.Cfg.Save(a.Paths.Config); err != nil { // before commit: all or nothing
		a.Cfg.RemoveSource(resolved)
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	if typ == models.SourceFile {
		a.infof("[fmon] Added file %q in %s\n", resolved, a.now().Sub(started).Round(time.Millisecond))
	} else {
		a.infof("[fmon] Added folder %q: %d file(s) indexed in %s\n",
			resolved, rep.FilesScanned, a.now().Sub(started).Round(time.Millisecond))
	}
	if len(rep.Errors) > 0 {
		a.infof("[fmon] %d path(s) could not be read and were skipped\n", len(rep.Errors))
	}
	return nil
}

// Remove stops watching an exact source. Its file states are removed by the
// foreign key cascade; the change history is kept.
// resolveWatchedSource resolves a user-given path to the exact path of a
// currently watched source (from cfgSources and the database, matched
// against both the given path and its symlink-resolved form, as "fmon add"
// stores the resolved path). It returns an error naming the covering source
// when the path lies inside one instead of being a source itself.
func resolveWatchedSource(ctx context.Context, q db.DBTX, cfgSources []string, arg string) (string, error) {
	abs, err := normalizePath(arg)
	if err != nil {
		return "", err
	}
	candidates := []string{abs}
	if r, err := filepath.EvalSymlinks(abs); err == nil && !equalPath(filepath.Clean(r), abs) {
		candidates = append(candidates, filepath.Clean(r))
	}

	sources, err := db.ListSources(ctx, q)
	if err != nil {
		return "", err
	}
	all := append([]string(nil), cfgSources...)
	for _, s := range sources {
		all = append(all, s.Path)
	}

	for _, c := range candidates {
		for _, p := range all {
			if equalPath(c, p) {
				return p, nil
			}
		}
	}
	for _, p := range all {
		if within(abs, p) {
			return "", fmt.Errorf("%q is not a watched source; it is covered by %q", abs, p)
		}
	}
	return "", fmt.Errorf("%q is not a watched source", abs)
}

func (a *App) Remove(ctx context.Context, arg string) error {
	tx, err := db.BeginImmediate(ctx, a.DB)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	target, err := resolveWatchedSource(ctx, tx, a.Cfg.Sources, arg)
	if err != nil {
		if strings.Contains(err.Error(), "covered by") {
			return fmt.Errorf("%s (use: fmon rm %s)", err, target)
		}
		return err
	}

	sources, err := db.ListSources(ctx, tx)
	if err != nil {
		return err
	}
	for _, s := range sources {
		if s.Path == target {
			if err := db.DeleteSource(ctx, tx, s.ID); err != nil {
				return err
			}
			break
		}
	}
	a.Cfg.RemoveSource(target)
	if err := a.Cfg.Save(a.Paths.Config); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	a.infof("[fmon] Removed %q from the watch list\n", target)
	return nil
}

// History prints the change history in chronological order: oldest first, the
// newest record last (like reading a log). A path limits the output to that
// file or to everything below that folder. limit <= 0 means no limit; with a
// limit, the newest N records are shown, still oldest-first.
func (a *App) History(ctx context.Context, path string, limit int) error {
	if path != "" {
		abs, err := normalizePath(path)
		if err != nil {
			return err
		}
		path = abs
	}
	entries, err := db.QueryHistory(ctx, a.DB, path, limit)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Fprintln(a.stdout(), "No history records found.")
		return nil
	}
	// QueryHistory returns newest first, which is what makes --limit select
	// the most recent records. Print them in reverse: oldest to newest.
	for i := len(entries) - 1; i >= 0; i-- {
		fmt.Fprintln(a.stdout(), formatHistory(entries[i]))
	}
	return nil
}

func formatHistory(e models.HistoryEntry) string {
	ts := e.Timestamp.Local().Format(time.RFC3339)
	line := fmt.Sprintf("%s  %-8s  %q", ts, e.Event, e.Path)
	switch e.Event {
	case models.EventAdded:
		if e.NewHash != nil && e.NewSize != nil {
			line += fmt.Sprintf("  hash=%s size=%d", models.FormatHash(*e.NewHash), *e.NewSize)
		}
	case models.EventModified:
		if e.OldHash != nil && e.NewHash != nil && e.OldSize != nil && e.NewSize != nil {
			line += fmt.Sprintf("  hash %s -> %s  size %d -> %d",
				models.FormatHash(*e.OldHash), models.FormatHash(*e.NewHash), *e.OldSize, *e.NewSize)
		}
	case models.EventDeleted:
		if e.OldHash != nil && e.OldSize != nil {
			line += fmt.Sprintf("  hash=%s size=%d", models.FormatHash(*e.OldHash), *e.OldSize)
		}
		// A DELETED record without hash/size marks a whole watched source
		// that disappeared from disk.
	}
	return line
}

// ClearHistoryAll deletes the entire change history.
func (a *App) ClearHistoryAll(ctx context.Context) error {
	return a.clearHistory(ctx, "")
}

// ClearHistoryPath deletes history records for a file or a whole folder tree.
func (a *App) ClearHistoryPath(ctx context.Context, path string) error {
	abs, err := normalizePath(path)
	if err != nil {
		return err
	}
	return a.clearHistory(ctx, abs)
}

func (a *App) clearHistory(ctx context.Context, path string) error {
	tx, err := db.BeginImmediate(ctx, a.DB)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	n, err := db.ClearHistory(ctx, tx, path)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	a.infof("[fmon] Deleted %d history record(s)\n", n)
	return nil
}