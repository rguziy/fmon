package monitor

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rguziy/fmon/internal/models"
)

func TestPrintScanWithChanges(t *testing.T) {
	e := newEnv(t)
	rep := &Report{
		Duration:       1234 * time.Millisecond,
		Sources:        2,
		MissingSources: 1,
		FilesScanned:   500,
		FilesHashed:    7,
		Errors:         []string{"x"},
		Alerts:         []string{`ALERT: Tracked folder "/mnt/share" was deleted from disk!`},
		Changes: []models.Change{
			{Event: models.EventModified, Path: "/etc/b", OldSize: 1, NewSize: 2},
			{Event: models.EventAdded, Path: "/etc/a", NewSize: 9},
			{Event: models.EventDeleted, Path: "/etc/c"},
		},
	}
	e.app.PrintScan(rep)

	want := `ALERT: Tracked folder "/mnt/share" was deleted from disk!
ADDED    "/etc/a" size=9
MODIFIED "/etc/b" size 1 -> 2
DELETED  "/etc/c"

Scan finished in 1.234s: 2 source(s) (1 missing), 500 file(s) scanned (7 hashed), 1 added, 1 modified, 1 deleted, 1 error(s)
`
	if got := e.stdout.String(); got != want {
		t.Fatalf("output:\n%s\nwant:\n%s", got, want)
	}
}

func TestPrintScanNoChanges(t *testing.T) {
	e := newEnv(t)
	e.app.PrintScan(&Report{Duration: 40 * time.Millisecond, Sources: 1, FilesScanned: 12})
	want := "No changes. Scan finished in 40ms: 1 source(s), 12 file(s) scanned (0 hashed), 0 added, 0 modified, 0 deleted, 0 error(s)\n"
	if got := e.stdout.String(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestPrintScanQuietAndHostileNames(t *testing.T) {
	e := newEnv(t)
	e.app.Quiet = true
	e.app.PrintScan(&Report{Changes: []models.Change{{Event: models.EventAdded, Path: "/x"}}})
	if e.stdout.Len() != 0 {
		t.Fatalf("-q must print nothing, got %q", e.stdout.String())
	}

	e.app.Quiet = false
	e.app.PrintScan(&Report{Changes: []models.Change{{Event: models.EventAdded, Path: "/tmp/evil\nDELETED  \"/etc/passwd\""}}})
	if strings.Contains(e.stdout.String(), "\nDELETED") {
		t.Fatalf("a crafted file name forged a line:\n%s", e.stdout.String())
	}
}

func TestScanFillsStatistics(t *testing.T) {
	e := newEnv(t)
	e.write(t, "a.txt", "a")
	e.write(t, "b.txt", "b")
	if err := e.app.Add(context.Background(), e.root); err != nil {
		t.Fatal(err)
	}
	e.write(t, "b.txt", "changed")
	e.write(t, "c.txt", "c")

	rep := e.scan(t)
	if rep.Sources != 1 || rep.FilesScanned != 3 || rep.FilesHashed != 2 {
		t.Fatalf("stats: sources=%d scanned=%d hashed=%d", rep.Sources, rep.FilesScanned, rep.FilesHashed)
	}
	if rep.Duration < 0 {
		t.Errorf("duration = %v", rep.Duration)
	}
}

func TestListSourcesStatuses(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.write(t, "watched/a.txt", "12345")
	e.write(t, "watched/b.txt", "678")
	e.write(t, "gone/x.txt", "x")
	e.write(t, "later/y.txt", "y")
	e.write(t, "file.conf", "cfg")
	watched, gone, later := filepath.Join(e.root, "watched"), filepath.Join(e.root, "gone"), filepath.Join(e.root, "later")
	file := filepath.Join(e.root, "file.conf")
	for _, p := range []string{watched, gone, file} {
		if err := e.app.Add(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	e.stdout.Reset()

	// "gone" disappears and is noticed by a scan; "later" is added to the
	// configuration by hand; "file.conf" is removed from the configuration by hand.
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	e.scan(t)
	e.app.Cfg.Sources = []string{watched, gone, later}

	if err := e.app.List(ctx, false, ""); err != nil {
		t.Fatal(err)
	}
	out := e.stdout.String()

	row := func(path string) string {
		for _, l := range strings.Split(out, "\n") {
			if strings.HasSuffix(l, strconv.Quote(path)) {
				return l
			}
		}
		t.Fatalf("no row for %s in:\n%s", path, out)
		return ""
	}
	if r := row(watched); !strings.Contains(r, "folder") || !strings.Contains(r, "active") || !strings.Contains(r, "2 ") || !strings.Contains(r, "8 B") {
		t.Errorf("watched row = %q", r)
	}
	if r := row(gone); !strings.Contains(r, "missing") {
		t.Errorf("gone row = %q", r)
	}
	if r := row(later); !strings.Contains(r, "pending") || !strings.Contains(r, "folder") {
		t.Errorf("later row = %q", r)
	}
	if r := row(file); !strings.Contains(r, "orphaned") || !strings.Contains(r, "file") {
		t.Errorf("file row = %q", r)
	}
	for _, want := range []string{
		"TYPE", "4 source(s), 4 file(s) tracked",
		`Exclude patterns: "*.tmp"`,
		"pending:  listed in fmon.toml", "orphaned: not listed in fmon.toml",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestListEmptyAndFiles(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.app.List(ctx, false, ""); err != nil || !strings.Contains(e.stdout.String(), "No watched sources") {
		t.Fatalf("empty list: %v %q", err, e.stdout.String())
	}

	e.write(t, "d/a.txt", "a")
	e.write(t, "d/sub/b.txt", "bb")
	e.write(t, "d2/c.txt", "c")
	d, d2 := filepath.Join(e.root, "d"), filepath.Join(e.root, "d2")
	for _, p := range []string{d, d2} {
		if err := e.app.Add(ctx, p); err != nil {
			t.Fatal(err)
		}
	}

	e.stdout.Reset()
	if err := e.app.List(ctx, true, ""); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(strings.TrimSpace(e.stdout.String()), "\n") + 1; n != 3 {
		t.Fatalf("expected 3 tracked files:\n%s", e.stdout.String())
	}

	e.stdout.Reset()
	if err := e.app.List(ctx, true, d); err != nil {
		t.Fatal(err)
	}
	out := e.stdout.String()
	if !strings.Contains(out, "a.txt") || !strings.Contains(out, "b.txt") || strings.Contains(out, "c.txt") {
		t.Fatalf("filtered listing:\n%s", out)
	}
	if len(strings.Fields(strings.SplitN(out, "\n", 2)[0])[0]) != 16 {
		t.Errorf("first column must be the 16-digit hash:\n%s", out)
	}

	e.stdout.Reset()
	if err := e.app.List(ctx, true, filepath.Join(e.root, "nothing")); err != nil || !strings.Contains(e.stdout.String(), "No tracked files") {
		t.Fatalf("no match: %v %q", err, e.stdout.String())
	}
}

func TestHumanBytes(t *testing.T) {
	tests := map[int64]string{
		0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 1536: "1.5 KiB",
		1048576: "1.0 MiB", 5 * 1024 * 1024 * 1024: "5.0 GiB",
	}
	for in, want := range tests {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestQuietSuppressesCommandMessages(t *testing.T) {
	e := newEnv(t)
	e.app.Quiet = true
	e.write(t, "d/a.txt", "a")
	ctx := context.Background()
	if err := e.app.Add(ctx, filepath.Join(e.root, "d")); err != nil {
		t.Fatal(err)
	}
	if err := e.app.Remove(ctx, filepath.Join(e.root, "d")); err != nil {
		t.Fatal(err)
	}
	if e.stdout.Len() != 0 {
		t.Fatalf("-q must silence confirmations, got %q", e.stdout.String())
	}
}
