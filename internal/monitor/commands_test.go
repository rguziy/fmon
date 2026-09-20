package monitor

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rguziy/fmon/internal/config"
	"github.com/rguziy/fmon/internal/db"
	"github.com/rguziy/fmon/internal/models"
)

func TestAddOverlapRule(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.write(t, "etc/ssh/sshd_config", "x")
	e.write(t, "etc/hosts", "y")
	etc := filepath.Join(e.root, "etc")
	ssh := filepath.Join(etc, "ssh")

	if err := e.app.Add(ctx, etc); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{ssh, etc, filepath.Join(etc, "hosts"), e.root} {
		e.stderr.Reset()
		if err := e.app.Add(ctx, p); err != nil {
			t.Fatalf("Add(%q): %v", p, err)
		}
		if w := e.stderr.String(); !strings.Contains(w, "WARNING") || !strings.Contains(w, "overlaps watched source") || !strings.Contains(w, "nothing added") {
			t.Errorf("Add(%q) stderr = %q", p, w)
		}
	}
	srcs, _ := db.ListSources(ctx, e.app.DB)
	if len(srcs) != 1 || srcs[0].Path != etc || len(e.app.Cfg.Sources) != 1 {
		t.Fatalf("nothing else may be added: db=%+v cfg=%v", srcs, e.app.Cfg.Sources)
	}

	// The reverse order (child first, then parent) is refused too.
	e2 := newEnv(t)
	e2.write(t, "etc/ssh/sshd_config", "x")
	if err := e2.app.Add(ctx, filepath.Join(e2.root, "etc", "ssh")); err != nil {
		t.Fatal(err)
	}
	e2.stderr.Reset()
	if err := e2.app.Add(ctx, filepath.Join(e2.root, "etc")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e2.stderr.String(), "overlaps") {
		t.Errorf("parent after child must warn, stderr = %q", e2.stderr.String())
	}
}

func TestAddValidation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.app.Add(ctx, filepath.Join(e.root, "does-not-exist")); err == nil {
		t.Error("adding a missing path must fail")
	}
	if runtime.GOOS != "windows" {
		if err := e.app.Add(ctx, "/dev/null"); err == nil {
			t.Error("adding a device must fail")
		}
	}
}

func TestAddResolvesSymlinkRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}
	e := newEnv(t)
	e.write(t, "real/a.txt", "a")
	link := filepath.Join(e.root, "link")
	if err := os.Symlink(filepath.Join(e.root, "real"), link); err != nil {
		t.Fatal(err)
	}
	if err := e.app.Add(context.Background(), link); err != nil {
		t.Fatal(err)
	}
	srcs, _ := db.ListSources(context.Background(), e.app.DB)
	if len(srcs) != 1 || srcs[0].Path != filepath.Join(e.root, "real") {
		t.Fatalf("the resolved path must be stored, got %+v", srcs)
	}
	if !strings.Contains(e.stdout.String(), "resolves to") {
		t.Errorf("the user must be told about the resolution: %q", e.stdout.String())
	}
	// Files are indexed, so the first scan is clean.
	if rep := e.scan(t); !rep.Empty() {
		t.Fatalf("scan after add must be clean: %v", e.changes(rep))
	}
}

func TestAddWritesConfigAndIndexesFiles(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.write(t, "d/a.txt", "a")
	e.write(t, "d/sub/b.txt", "b")
	e.write(t, "d/skip.tmp", "excluded")
	d := filepath.Join(e.root, "d")
	if err := e.app.Add(ctx, d); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.stdout.String(), "2 file(s) indexed") {
		t.Errorf("stdout = %q", e.stdout.String())
	}
	cfg, err := config.Load(e.app.Paths.Config)
	if err != nil || len(cfg.Sources) != 1 || cfg.Sources[0] != d {
		t.Fatalf("fmon.toml sources = %v (err %v)", cfg.Sources, err)
	}
	srcs, _ := db.ListSources(ctx, e.app.DB)
	if states, _ := db.LoadFileStates(ctx, e.app.DB, srcs[0].ID); len(states) != 2 {
		t.Fatalf("indexed %d files, want 2", len(states))
	}
}

func TestRemoveCascadesButKeepsHistory(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.write(t, "d/a.txt", "a")
	d := filepath.Join(e.root, "d")
	if err := e.app.Add(ctx, d); err != nil {
		t.Fatal(err)
	}
	e.write(t, "d/b.txt", "b")
	e.scan(t) // creates a history record

	// A path merely covered by a source is not removable and the error says why.
	err := e.app.Remove(ctx, filepath.Join(d, "a.txt"))
	if err == nil || !strings.Contains(err.Error(), "covered by") {
		t.Fatalf("err = %v", err)
	}
	if err := e.app.Remove(ctx, filepath.Join(e.root, "unknown")); err == nil {
		t.Fatal("removing an unknown path must fail")
	}

	if err := e.app.Remove(ctx, d); err != nil {
		t.Fatal(err)
	}
	var states int
	_ = e.app.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM files_state`).Scan(&states)
	if states != 0 {
		t.Errorf("files_state rows left after rm: %d", states)
	}
	if hist, _ := db.QueryHistory(ctx, e.app.DB, "", 0); len(hist) != 1 {
		t.Errorf("history must survive rm, got %d records", len(hist))
	}
	if cfg, _ := config.Load(e.app.Paths.Config); len(cfg.Sources) != 0 {
		t.Errorf("fmon.toml still lists %v", cfg.Sources)
	}
}

func TestInitRequiresConfirmation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.write(t, "a.txt", "a")
	if err := e.app.Add(ctx, e.root); err != nil {
		t.Fatal(err)
	}
	e.app.Cfg.Scripts = []config.ScriptConfig{{Path: "/x", Detail: "summary", Timeout: "5s", MaxMessageBytes: 1000}}
	e.app.Cfg.Exclude = []string{"*.bak"}

	// Non-interactive without --yes: refused, nothing changes.
	if err := e.app.Init(ctx, false); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("err = %v", err)
	}
	if srcs, _ := db.ListSources(ctx, e.app.DB); len(srcs) != 1 {
		t.Fatal("data was deleted without confirmation")
	}

	// Interactive "n" aborts, "y" proceeds.
	e.app.IsTerminal = func() bool { return true }
	e.app.Stdin = strings.NewReader("n\n")
	if err := e.app.Init(ctx, false); err != ErrAborted {
		t.Fatalf("err = %v, want ErrAborted", err)
	}
	e.app.Stdin = strings.NewReader("y\n")
	if err := e.app.Init(ctx, false); err != nil {
		t.Fatal(err)
	}

	if has, _ := db.HasData(ctx, e.app.DB); has {
		t.Error("database not wiped")
	}
	cfg, err := config.Load(e.app.Paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Sources) != 0 || len(cfg.Scripts) != 1 || len(cfg.Exclude) != 1 || cfg.Exclude[0] != "*.bak" {
		t.Errorf("only sources may be cleared, other settings kept: %+v", cfg)
	}

	// A fresh database needs no confirmation.
	e.app.IsTerminal = func() bool { return false }
	if err := e.app.Init(ctx, false); err != nil {
		t.Fatalf("init on an empty setup must not prompt: %v", err)
	}
}

func TestHistoryAndClear(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.write(t, "d/a.txt", "a")
	e.write(t, "d2/b.txt", "b")
	d, d2 := filepath.Join(e.root, "d"), filepath.Join(e.root, "d2")
	for _, p := range []string{d, d2} {
		if err := e.app.Add(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	e.write(t, "d/new.txt", "n")
	e.write(t, "d2/b.txt", "changed")
	e.scan(t)

	e.stdout.Reset()
	if err := e.app.History(ctx, d, 0); err != nil {
		t.Fatal(err)
	}
	out := e.stdout.String()
	if !strings.Contains(out, "ADDED") || !strings.Contains(out, `"`+filepath.Join(d, "new.txt")+`"`) || strings.Contains(out, "b.txt") {
		t.Fatalf("history for d = %q", out)
	}

	e.stdout.Reset()
	if err := e.app.History(ctx, "", 1); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(strings.TrimSpace(e.stdout.String()), "\n") + 1; n != 1 {
		t.Fatalf("limit 1 printed %d lines", n)
	}

	if err := e.app.ClearHistoryPath(ctx, d); err != nil {
		t.Fatal(err)
	}
	rest, _ := db.QueryHistory(ctx, e.app.DB, "", 0)
	if len(rest) != 1 || rest[0].Event != models.EventModified {
		t.Fatalf("remaining history = %+v", rest)
	}
	if err := e.app.ClearHistoryAll(ctx); err != nil {
		t.Fatal(err)
	}
	e.stdout.Reset()
	_ = e.app.History(ctx, "", 0)
	if !strings.Contains(e.stdout.String(), "No history records found") {
		t.Errorf("stdout = %q", e.stdout.String())
	}
}

func TestHistoryIsChronologicalAndLimitKeepsNewest(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tick := 0
	e.app.Now = func() time.Time { // strictly increasing event times
		tick++
		return time.Date(2026, 1, 1, 0, 0, tick, 0, time.UTC)
	}

	e.write(t, "x.txt", "x")
	if err := e.app.Add(ctx, e.root); err != nil {
		t.Fatal(err)
	}
	e.write(t, "y.txt", "y") // scan 1: ADDED y
	e.scan(t)
	e.write(t, "x.txt", "xx") // scan 2: MODIFIED x
	e.scan(t)
	if err := os.Remove(filepath.Join(e.root, "y.txt")); err != nil { // scan 3: DELETED y
		t.Fatal(err)
	}
	e.scan(t)

	events := func(limit int) []string {
		e.stdout.Reset()
		if err := e.app.History(ctx, "", limit); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, l := range strings.Split(strings.TrimSpace(e.stdout.String()), "\n") {
			out = append(out, strings.Fields(l)[1])
		}
		return out
	}
	if got := strings.Join(events(0), ","); got != "ADDED,MODIFIED,DELETED" {
		t.Fatalf("order = %s, want oldest first: ADDED,MODIFIED,DELETED", got)
	}
	if got := strings.Join(events(2), ","); got != "MODIFIED,DELETED" {
		t.Fatalf("limit 2 = %s, want the two newest, oldest first: MODIFIED,DELETED", got)
	}
}
