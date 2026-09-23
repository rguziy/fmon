package monitor

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rguziy/fmon/internal/config"
	"github.com/rguziy/fmon/internal/db"
)

type env struct {
	app    *App
	stdout *bytes.Buffer
	stderr *bytes.Buffer
	root   string // directory for files under test (outside the config dir)
	clock  time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	base := t.TempDir()
	cfgDir := filepath.Join(base, "cfg")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "data")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// Resolve symlinks (macOS temp dirs live behind /var -> /private/var) so
	// paths compare equal to what "fmon add" stores.
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}

	paths := config.Paths{
		Dir:    cfgDir,
		Config: filepath.Join(cfgDir, "fmon.toml"),
		DB:     filepath.Join(cfgDir, "fmon.db"),
		Log:    filepath.Join(cfgDir, "fmon.log"),
	}
	cfg := config.Default()
	cfg.Exclude = []string{"*.tmp"}

	d, err := db.Open(context.Background(), paths.DB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	e := &env{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, root: root, clock: time.Now().Add(-time.Hour).Truncate(time.Second)}
	e.app = &App{
		Paths:      paths,
		Cfg:        cfg,
		DB:         d,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Stdin:      strings.NewReader(""),
		Stdout:     e.stdout,
		Stderr:     e.stderr,
		Hostname:   "testhost",
		IsTerminal: func() bool { return false },
	}
	return e
}

// write creates or overwrites a file and gives it a distinct, increasing
// mtime, so the size+mtime shortcut cannot hide a change on filesystems with
// coarse timestamps.
func (e *env) write(t *testing.T, rel, content string) string {
	t.Helper()
	p := filepath.Join(e.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	e.clock = e.clock.Add(time.Minute)
	if err := os.Chtimes(p, e.clock, e.clock); err != nil {
		t.Fatal(err)
	}
	return p
}

func (e *env) scan(t *testing.T) *Report {
	t.Helper()
	rep, err := e.app.Scan(context.Background(), false)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	return rep
}

// scanFull is like scan but with --full (bypasses the stat shortcut).
func (e *env) scanFull(t *testing.T) *Report {
	t.Helper()
	rep, err := e.app.Scan(context.Background(), true)
	if err != nil {
		t.Fatalf("scan --full: %v", err)
	}
	return rep
}

// changes renders the changes of a report as "EVENT rel/path" strings.
func (e *env) changes(rep *Report) []string {
	var out []string
	for _, c := range rep.Changes {
		rel, _ := filepath.Rel(e.root, c.Path)
		out = append(out, string(c.Event)+" "+filepath.ToSlash(rel))
	}
	return out
}

func sameSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	m := map[string]int{}
	for _, g := range got {
		m[g]++
	}
	for _, w := range want {
		m[w]--
	}
	for _, n := range m {
		if n != 0 {
			return false
		}
	}
	return true
}
