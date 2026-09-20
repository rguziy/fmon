package monitor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/rguziy/fmon/internal/db"
	"github.com/rguziy/fmon/internal/models"
)

// Extra list-only statuses besides "active" and "missing".
const (
	statusPending  = "pending"  // listed in fmon.toml, not indexed yet
	statusOrphaned = "orphaned" // in the database, no longer listed in fmon.toml
)

// List prints the watched sources. With files=true it prints every tracked
// file instead, optionally limited to a file or folder tree. It only reads.
func (a *App) List(ctx context.Context, files bool, path string) error {
	if files {
		return a.listFiles(ctx, path)
	}
	return a.listSources(ctx)
}

type sourceRow struct {
	path    string
	typ     string
	status  string
	files   string
	size    string
	created string
}

func (a *App) listSources(ctx context.Context) error {
	sources, err := db.ListSources(ctx, a.DB)
	if err != nil {
		return err
	}
	totals, err := db.FileTotalsBySource(ctx, a.DB)
	if err != nil {
		return err
	}

	inConfig := make(map[string]bool, len(a.Cfg.Sources))
	for _, p := range a.Cfg.Sources {
		inConfig[filepath.Clean(p)] = true
	}
	inDB := make(map[string]bool, len(sources))

	var rows []sourceRow
	var totalFiles, totalBytes int64
	pending, orphaned := false, false

	for _, s := range sources {
		inDB[s.Path] = true
		t := totals[s.ID]
		totalFiles += t.Files
		totalBytes += t.Bytes

		status := string(s.Status)
		if !inConfig[s.Path] {
			status = statusOrphaned
			orphaned = true
		}
		rows = append(rows, sourceRow{
			path:    s.Path,
			typ:     string(s.Type),
			status:  status,
			files:   fmt.Sprint(t.Files),
			size:    humanBytes(t.Bytes),
			created: s.CreatedAt.Local().Format(time.RFC3339),
		})
	}
	for p := range inConfig {
		if inDB[p] {
			continue
		}
		pending = true
		rows = append(rows, sourceRow{
			path: p, typ: guessType(p), status: statusPending, files: "-", size: "-", created: "-",
		})
	}

	if len(rows) == 0 {
		fmt.Fprintln(a.stdout(), "No watched sources. Use 'fmon add <path>' to start watching a file or folder.")
		return nil
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].path < rows[j].path })

	w := a.stdout()
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TYPE\tSTATUS\tFILES\tSIZE\tADDED\tPATH")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%q\n", r.typ, r.status, r.files, r.size, r.created, r.path)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintf(w, "\n%d source(s), %d file(s) tracked, %s\n", len(rows), totalFiles, humanBytes(totalBytes))
	if len(a.Cfg.Exclude) > 0 {
		quoted := make([]string, len(a.Cfg.Exclude))
		for i, p := range a.Cfg.Exclude {
			quoted[i] = fmt.Sprintf("%q", p)
		}
		fmt.Fprintf(w, "Exclude patterns: %s\n", strings.Join(quoted, " "))
	}
	if pending {
		fmt.Fprintln(w, "pending:  listed in fmon.toml, will be indexed by the next scan")
	}
	if orphaned {
		fmt.Fprintln(w, "orphaned: not listed in fmon.toml, will be removed by the next scan")
	}
	return nil
}

func (a *App) listFiles(ctx context.Context, path string) error {
	if path != "" {
		abs, err := normalizePath(path)
		if err != nil {
			return err
		}
		path = abs
	}

	w := a.stdout()
	n := 0
	err := db.ForEachFileState(ctx, a.DB, path, func(s models.FileState) error {
		n++
		_, err := fmt.Fprintf(w, "%s  %12d  %s  %q\n",
			models.FormatHash(s.Hash), s.Size,
			time.Unix(0, s.ModTime).Local().Format(time.RFC3339), s.Path)
		return err
	})
	if err != nil {
		return err
	}
	if n == 0 {
		fmt.Fprintln(w, "No tracked files.")
	}
	return nil
}

// guessType reports the kind of a not-yet-indexed source from the file system.
func guessType(p string) string {
	info, err := os.Lstat(p)
	switch {
	case err != nil:
		return "-"
	case info.IsDir():
		return string(models.SourceFolder)
	case info.Mode().IsRegular():
		return string(models.SourceFile)
	}
	return "?"
}

// humanBytes formats a byte count with binary units (KiB, MiB, ...).
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
