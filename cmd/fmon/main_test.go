package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rguziy/fmon/internal/version"
)

// tempDir returns a temporary directory with symbolic links resolved. "fmon
// add" stores the resolved path, and temporary directories are behind a link on
// macOS (/var -> /private/var) and may use 8.3 short names on Windows, so tests
// that compare paths must start from the resolved form.
func tempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	return dir
}

// runCLI executes the CLI in-process and returns exit code, stdout, stderr.
func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, strings.NewReader(""), &out, &errb)
	return code, out.String(), errb.String()
}

func TestVersion(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}} {
		code, out, _ := runCLI(t, args...)
		if code != exitOK || strings.TrimSpace(out) != "fmon "+version.Version {
			t.Errorf("%v -> %d %q", args, code, out)
		}
	}
	if version.Version != "1.1.0" {
		t.Errorf("source tree version = %q, want 1.1.0", version.Version)
	}
}

func TestBareFmonPrintsUsageWithoutError(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "never-created")
	for _, args := range [][]string{{}, {"--config-dir=" + dir}, {"-v"}} {
		code, out, errs := runCLI(t, args...)
		if code != exitOK || errs != "" {
			t.Errorf("%v -> exit %d, stderr %q", args, code, errs)
		}
		if !strings.Contains(out, "No command given") || !strings.Contains(out, "Commands:") || !strings.Contains(out, "list [--files]") {
			t.Errorf("%v -> stdout %q", args, out)
		}
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("printing the usage must not create anything")
	}
}

func TestUsageErrors(t *testing.T) {
	cfg := "--config-dir=" + t.TempDir()
	tests := []struct {
		name string
		args []string
	}{
		{"unknown command", []string{cfg, "frobnicate"}},
		{"add without path", []string{cfg, "add"}},
		{"rm with two paths", []string{cfg, "rm", "a", "b"}},
		{"clear without flag", []string{cfg, "clear"}},
		{"clear with two flags", []string{cfg, "clear", "--all", "--history-all"}},
		{"limit outside history", []string{cfg, "scan", "--limit", "5"}},
		{"scan flag with command", []string{cfg, "--scan", "history"}},
		{"unknown flag", []string{cfg, "scan", "--nope"}},
		{"files outside list", []string{cfg, "scan", "--files"}},
		{"list path without files", []string{cfg, "list", "/tmp"}},
		{"full outside scan", []string{cfg, "history", "--full"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if code, _, _ := runCLI(t, tc.args...); code != exitFatal {
				t.Errorf("exit code = %d, want %d", code, exitFatal)
			}
		})
	}
}

func TestEndToEnd(t *testing.T) {
	base := tempDir(t)
	cfgDir := filepath.Join(base, "cfg")
	data := filepath.Join(base, "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "a.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := "--config-dir=" + cfgDir

	if code, out, errs := runCLI(t, cfg, "add", data); code != exitOK || !strings.Contains(out, "1 file(s) indexed") || !strings.Contains(out, "Created configuration") {
		t.Fatalf("add: %d %q %q", code, out, errs)
	}
	// A scan with nothing to report prints only the statistics line;
	// --scan is an alias of the scan command.
	for _, args := range [][]string{{cfg, "scan"}, {cfg, "--scan"}, {"scan", cfg}} {
		code, out, errs := runCLI(t, args...)
		if code != exitOK || errs != "" {
			t.Fatalf("%v: %d stderr=%q", args, code, errs)
		}
		if !strings.HasPrefix(out, "No changes. Scan finished in ") || !strings.Contains(out, "1 source(s), 1 file(s) scanned (0 hashed), 0 added, 0 modified, 0 deleted, 0 error(s)") {
			t.Fatalf("%v: stdout = %q", args, out)
		}
	}
	// -q (and --quiet) print nothing on stdout.
	for _, flagName := range []string{"-q", "--quiet"} {
		if code, out, errs := runCLI(t, cfg, flagName, "scan"); code != exitOK || out != "" || errs != "" {
			t.Fatalf("%s scan: %d stdout=%q stderr=%q", flagName, code, out, errs)
		}
	}

	// Change something: the changed files and the statistics are printed,
	// the exit code stays 0 (changes are not errors) and fmon.log is written.
	if err := os.WriteFile(filepath.Join(data, "b.txt"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errs := runCLI(t, cfg, "scan")
	if code != exitOK {
		t.Fatalf("scan with changes: %d %q", code, errs)
	}
	if !strings.Contains(out, "ADDED    "+strconv.Quote(filepath.Join(data, "b.txt"))+" size=3") ||
		!strings.Contains(out, "2 file(s) scanned (1 hashed), 1 added, 0 modified, 0 deleted, 0 error(s)") ||
		strings.Contains(out, "No changes.") {
		t.Fatalf("scan output = %q", out)
	}
	logData, _ := os.ReadFile(filepath.Join(cfgDir, "fmon.log"))
	if !strings.Contains(string(logData), "ADDED    "+strconv.Quote(filepath.Join(data, "b.txt"))) {
		t.Fatalf("fmon.log = %q", logData)
	}

	// History accepts the flag after the positional argument.
	code, out, _ = runCLI(t, cfg, "history", data, "--limit", "5")
	if code != exitOK || !strings.Contains(out, "ADDED") {
		t.Fatalf("history: %d %q", code, out)
	}

	// --full bypasses the size+mtime shortcut.
	code, out, errs = runCLI(t, cfg, "scan", "--full")
	if code != exitOK || errs != "" || !strings.Contains(out, "Full scan.") || !strings.Contains(out, "(2 hashed)") {
		t.Fatalf("scan --full: %d stdout=%q stderr=%q", code, out, errs)
	}

	// list shows the watched source; list --files every tracked file.
	code, out, _ = runCLI(t, cfg, "list")
	if code != exitOK || !strings.Contains(out, "TYPE") || !strings.Contains(out, "folder") || !strings.Contains(out, "active") ||
		!strings.Contains(out, strconv.Quote(data)) || !strings.Contains(out, "1 source(s), 2 file(s) tracked") {
		t.Fatalf("list: %d %q", code, out)
	}
	code, out, _ = runCLI(t, cfg, "list", "--files")
	if code != exitOK || !strings.Contains(out, strconv.Quote(filepath.Join(data, "a.txt"))) || !strings.Contains(out, strconv.Quote(filepath.Join(data, "b.txt"))) {
		t.Fatalf("list --files: %d %q", code, out)
	}
	code, out, _ = runCLI(t, cfg, "list", "--files", filepath.Join(data, "a.txt"))
	if code != exitOK || !strings.Contains(out, "a.txt") || strings.Contains(out, "b.txt") {
		t.Fatalf("list --files <file>: %d %q", code, out)
	}

	// clear --all needs --yes when data exists and stdin is not interactive.
	if code, _, _ := runCLI(t, cfg, "clear", "--all"); code != exitFatal {
		t.Fatalf("clear --all without --yes: exit %d, want %d", code, exitFatal)
	}
	if code, _, errs := runCLI(t, cfg, "clear", "--all", "--yes"); code != exitOK {
		t.Fatalf("clear --all --yes: %d %q", code, errs)
	}
	if code, out, _ := runCLI(t, cfg, "history"); code != exitOK || !strings.Contains(out, "No history") {
		t.Fatalf("history after reset: %d %q", code, out)
	}
}

func TestScanExitCodeTwoOnNonFatalErrors(t *testing.T) {
	base := tempDir(t)
	cfgDir := filepath.Join(base, "cfg")
	data := filepath.Join(base, "data")
	_ = os.MkdirAll(data, 0o755)
	_ = os.WriteFile(filepath.Join(data, "a.txt"), []byte("x"), 0o644)
	cfg := "--config-dir=" + cfgDir
	if code, _, e := runCLI(t, cfg, "add", data); code != exitOK {
		t.Fatalf("add: %d %s", code, e)
	}

	// A notification script that cannot be executed is a non-fatal sink error.
	toml, _ := os.ReadFile(filepath.Join(cfgDir, "fmon.toml"))
	toml = append(toml, []byte("\n[[script]]\npath = '"+filepath.Join(base, "missing-script.sh")+"'\n")...)
	if err := os.WriteFile(filepath.Join(cfgDir, "fmon.toml"), toml, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(data, "b.txt"), []byte("y"), 0o644)

	code, _, errs := runCLI(t, cfg, "scan")
	if code != exitNonFatal || !strings.Contains(errs, "notification sink") {
		t.Fatalf("exit %d, stderr %q; want %d", code, errs, exitNonFatal)
	}
}

func TestInvalidConfigIsFatal(t *testing.T) {
	cfgDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(cfgDir, "fmon.toml"), []byte("[log]\ndetail = 'loud'\n"), 0o600)
	code, _, errs := runCLI(t, "--config-dir="+cfgDir, "scan")
	if code != exitFatal || !strings.Contains(errs, "detail") {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
}

func TestFirstRunDoesNotCreateFiles(t *testing.T) {
	cfgDir := filepath.Join(t.TempDir(), "fmon")
	cfg := "--config-dir=" + cfgDir

	// list and history explain what to do and succeed.
	for _, cmd := range []string{"list", "history"} {
		code, out, errs := runCLI(t, cfg, cmd)
		if code != exitOK || errs != "" || !strings.Contains(out, "No configuration found at") || !strings.Contains(out, "fmon init") {
			t.Errorf("%s: exit %d stdout %q stderr %q", cmd, code, out, errs)
		}
	}
	// Commands that cannot do anything useful fail with the same hint.
	for _, args := range [][]string{{"scan"}, {"rm", "/tmp"}, {"clear", "--history-all"}} {
		code, _, errs := runCLI(t, append([]string{cfg}, args...)...)
		if code != exitFatal || !strings.Contains(errs, "fmon init") {
			t.Errorf("%v: exit %d stderr %q", args, code, errs)
		}
	}
	if _, err := os.Stat(cfgDir); err == nil {
		t.Fatal("no command may create the configuration directory before init/add")
	}

	// init creates the configuration and the database.
	code, out, errs := runCLI(t, cfg, "init")
	if code != exitOK || !strings.Contains(out, "Initialized") {
		t.Fatalf("init: %d %q %q", code, out, errs)
	}
	for _, name := range []string{"fmon.toml", "fmon.db"} {
		if _, err := os.Stat(filepath.Join(cfgDir, name)); err != nil {
			t.Errorf("init did not create %s: %v", name, err)
		}
	}
	if code, out, _ := runCLI(t, cfg, "list"); code != exitOK || !strings.Contains(out, "No watched sources") {
		t.Errorf("list after init: %d %q", code, out)
	}
	if code, out, errs := runCLI(t, cfg, "scan"); code != exitOK || !strings.Contains(out, "0 source(s)") {
		t.Errorf("scan after init: %d %q %q", code, out, errs)
	}
}

func TestMissingConfigWithExistingDatabaseIsProtected(t *testing.T) {
	base := tempDir(t)
	cfgDir := filepath.Join(base, "cfg")
	data := filepath.Join(base, "data")
	_ = os.MkdirAll(data, 0o755)
	_ = os.WriteFile(filepath.Join(data, "a.txt"), []byte("x"), 0o644)
	cfg := "--config-dir=" + cfgDir
	if code, _, e := runCLI(t, cfg, "add", data); code != exitOK {
		t.Fatalf("add: %d %s", code, e)
	}

	if err := os.Remove(filepath.Join(cfgDir, "fmon.toml")); err != nil {
		t.Fatal(err)
	}
	// Mutating commands refuse: continuing would forget every stored source.
	for _, args := range [][]string{{"scan"}, {"add", data}, {"rm", data}} {
		code, _, errs := runCLI(t, append([]string{cfg}, args...)...)
		if code != exitFatal || !strings.Contains(errs, "fmon.toml") || !strings.Contains(errs, "fmon init") {
			t.Errorf("%v: exit %d stderr %q", args, code, errs)
		}
	}
	// Reading still works and shows the sources as orphaned.
	code, out, _ := runCLI(t, cfg, "list")
	if code != exitOK || !strings.Contains(out, "orphaned") {
		t.Errorf("list: %d %q", code, out)
	}
	// init recovers.
	if code, _, e := runCLI(t, cfg, "init", "--yes"); code != exitOK {
		t.Fatalf("init --yes: %d %s", code, e)
	}
}

func TestDeletedDatabaseIsReportedNotSilent(t *testing.T) {
	base := tempDir(t)
	cfgDir := filepath.Join(base, "cfg")
	data := filepath.Join(base, "data")
	_ = os.MkdirAll(data, 0o755)
	_ = os.WriteFile(filepath.Join(data, "a.txt"), []byte("x"), 0o644)
	cfg := "--config-dir=" + cfgDir
	if code, _, e := runCLI(t, cfg, "add", data); code != exitOK {
		t.Fatalf("add: %d %s", code, e)
	}

	for _, name := range []string{"fmon.db", "fmon.db-wal", "fmon.db-shm"} {
		_ = os.Remove(filepath.Join(cfgDir, name))
	}
	code, out, errs := runCLI(t, cfg, "scan")
	if code != exitOK || errs != "" || !strings.Contains(out, "NOTICE: database was missing") {
		t.Fatalf("scan after deleting the database: %d stdout %q stderr %q", code, out, errs)
	}
	// The notice goes to the log sink too, and only once.
	logData, _ := os.ReadFile(filepath.Join(cfgDir, "fmon.log"))
	if !strings.Contains(string(logData), "database was missing") {
		t.Errorf("notice missing from fmon.log: %q", logData)
	}
	code, out, _ = runCLI(t, cfg, "scan")
	if code != exitOK || !strings.HasPrefix(out, "No changes.") {
		t.Fatalf("second scan: %d %q", code, out)
	}
}
