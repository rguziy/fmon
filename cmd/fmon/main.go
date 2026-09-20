// Command fmon is a lightweight file integrity monitoring (FIM) tool.
//
// It takes one snapshot of the watched files and folders per run
// ("fmon scan"), compares it with the previous snapshot and reports changes.
// There is no daemon: schedule it with cron or a systemd timer.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/rguziy/fmon/internal/config"
	"github.com/rguziy/fmon/internal/db"
	"github.com/rguziy/fmon/internal/monitor"
	"github.com/rguziy/fmon/internal/version"
)

// Exit codes.
const (
	exitOK       = 0 // success; for scan: completed, with or without changes
	exitFatal    = 1 // fatal error: configuration, database, lock, usage
	exitNonFatal = 2 // scan completed, but some paths or sinks failed
)

const usageText = `fmon - file integrity monitor

Usage:
  fmon [global flags] <command> [arguments]

Commands:
  scan                    Take a snapshot and report changes (alias: fmon --scan)
  list [--files] [path]   List watched files and folders (--files: every tracked file)
  add <path>              Start watching a file or folder and index it
  rm <path>               Stop watching a file or folder
  history [path]          Show the change history, oldest first (--limit N: last N)
  clear --history-all     Delete the whole change history
  clear --path=<path>     Delete history records of a file or folder tree
  clear --all             Reset everything (same as init)
  init                    Create the configuration, or reset the database and
                          clear the watched sources
  version                 Print the version

Global flags:
  --config-dir <dir>      Configuration directory (default: FMON_CONFIG_DIR, then
                          the user config directory, e.g. ~/.config/fmon)
  -v                      Verbose progress on stderr
  -q, --quiet             No normal output on stdout (warnings and errors still
                          go to stderr); meant for cron and timers
  --yes                   Do not ask for confirmation (init, clear --all)

Exit codes: 0 ok, 1 fatal error, 2 scan completed with non-fatal errors.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

type globalOpts struct {
	configDir string
	verbose   bool
	quiet     bool
	yes       bool
}

// bindGlobal registers the global flags on fs. The current values are used as
// defaults, so a flag given before the command is not reset by the same flag
// being defined again on the command's own flag set.
func bindGlobal(fs *flag.FlagSet, g *globalOpts) {
	fs.StringVar(&g.configDir, "config-dir", g.configDir, "configuration directory")
	fs.BoolVar(&g.verbose, "v", g.verbose, "verbose output on stderr")
	fs.BoolVar(&g.quiet, "q", g.quiet, "no normal output on stdout")
	fs.BoolVar(&g.quiet, "quiet", g.quiet, "same as -q")
	fs.BoolVar(&g.yes, "yes", g.yes, "assume yes for confirmation prompts")
}

// parseInterspersed parses flags that may appear before or after positional
// arguments (the standard flag package stops at the first positional).
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	var g globalOpts

	top := flag.NewFlagSet("fmon", flag.ContinueOnError)
	top.SetOutput(stderr)
	top.Usage = func() { fmt.Fprint(stderr, usageText) }
	bindGlobal(top, &g)
	var scanAlias, showVersion bool
	top.BoolVar(&scanAlias, "scan", false, "alias for the scan command")
	top.BoolVar(&showVersion, "version", false, "print the version and exit")
	if err := top.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitFatal
	}

	rest := top.Args()
	cmd := ""
	if len(rest) > 0 {
		cmd, rest = rest[0], rest[1:]
	}
	switch {
	case showVersion || cmd == "version":
		fmt.Fprintf(stdout, "fmon %s\n", version.Version)
		return exitOK
	case cmd == "help":
		fmt.Fprint(stdout, usageText)
		return exitOK
	case scanAlias && cmd != "":
		fmt.Fprintln(stderr, "[fmon] ERROR: --scan cannot be combined with a command")
		return exitFatal
	case scanAlias:
		cmd = "scan"
	case cmd == "":
		// Not an error: the user simply did not say what to do.
		fmt.Fprintf(stdout, "No command given. Use one of the commands below:\n\n%s", usageText)
		return exitOK
	}

	// Per-command flags. Global flags are accepted here as well, so
	// "fmon scan --config-dir x" works like "fmon --config-dir x scan".
	sub := flag.NewFlagSet("fmon "+cmd, flag.ContinueOnError)
	sub.SetOutput(stderr)
	sub.Usage = func() { fmt.Fprint(stderr, usageText) }
	bindGlobal(sub, &g)

	limit := sub.Int("limit", 100, "history: maximum number of records (0 = unlimited)")
	listFiles := sub.Bool("files", false, "list: show every tracked file")
	clearAll := sub.Bool("all", false, "clear: reset everything")
	clearHist := sub.Bool("history-all", false, "clear: delete the whole history")
	clearPath := sub.String("path", "", "clear: delete history of this file or folder tree")

	maxPositional, known := map[string]int{
		"scan": 0, "init": 0, "clear": 0, "add": 1, "rm": 1, "history": 1, "list": 1,
	}[cmd]
	if !known {
		fmt.Fprintf(stderr, "[fmon] ERROR: unknown command %q\n\n%s", cmd, usageText)
		return exitFatal
	}
	positional, err := parseInterspersed(sub, rest)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitFatal
	}
	given := make(map[string]bool)
	sub.Visit(func(f *flag.Flag) { given[f.Name] = true })
	switch {
	case given["limit"] && cmd != "history":
		fmt.Fprintln(stderr, "[fmon] ERROR: --limit is only valid for the history command")
		return exitFatal
	case given["files"] && cmd != "list":
		fmt.Fprintln(stderr, "[fmon] ERROR: --files is only valid for the list command")
		return exitFatal
	case (given["all"] || given["history-all"] || given["path"]) && cmd != "clear":
		fmt.Fprintln(stderr, "[fmon] ERROR: --all, --history-all and --path are only valid for the clear command")
		return exitFatal
	case len(positional) > maxPositional:
		fmt.Fprintf(stderr, "[fmon] ERROR: unexpected argument %q\n", positional[maxPositional])
		return exitFatal
	case (cmd == "add" || cmd == "rm") && len(positional) != 1:
		fmt.Fprintf(stderr, "[fmon] ERROR: %s requires a path\n", cmd)
		return exitFatal
	case cmd == "list" && len(positional) > 0 && !*listFiles:
		fmt.Fprintln(stderr, "[fmon] ERROR: a path filter for list requires --files")
		return exitFatal
	}
	if cmd == "clear" {
		n := 0
		for _, set := range []bool{*clearAll, *clearHist, *clearPath != ""} {
			if set {
				n++
			}
		}
		if n != 1 {
			fmt.Fprintln(stderr, "[fmon] ERROR: clear requires exactly one of --all, --history-all, --path=<path>")
			return exitFatal
		}
	}

	// ---- environment setup ----------------------------------------------------
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fail := func(err error) int {
		fmt.Fprintf(stderr, "[fmon] ERROR: %v\n", err)
		return exitFatal
	}

	paths, err := config.ResolvePaths(g.configDir)
	if err != nil {
		return fail(err)
	}

	// Decide, BEFORE creating anything, whether fmon has been set up here.
	// Read-only and mutating commands must not create files as a side effect
	// on a machine where fmon was never initialized.
	configExisted, err := fileExists(paths.Config)
	if err != nil {
		return fail(fmt.Errorf("check %s: %w", paths.Config, err))
	}
	dbExisted, err := fileExists(paths.DB)
	if err != nil {
		return fail(fmt.Errorf("check %s: %w", paths.DB, err))
	}

	switch {
	case !configExisted && !dbExisted && cmd != "init" && cmd != "add":
		hint := fmt.Sprintf("No configuration found at %s. Run 'fmon init' to create it.", paths.Config)
		if cmd == "list" || cmd == "history" {
			fmt.Fprintf(stdout, "[fmon] %s\n", hint)
			return exitOK
		}
		return fail(errors.New(hint))

	case !configExisted && dbExisted && (cmd == "scan" || cmd == "add" || cmd == "rm"):
		// fmon.toml is the source of truth for the watched sources. Working
		// without it would silently forget every source stored in the database.
		return fail(fmt.Errorf("%s not found, but a database exists; restore fmon.toml, or run 'fmon init' to start over", paths.Config))
	}

	if err := paths.EnsureDir(); err != nil {
		return fail(err)
	}
	cfg, err := config.Load(paths.Config)
	if err != nil {
		return fail(err)
	}
	logFile, err := os.OpenFile(paths.Log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fail(fmt.Errorf("open log file: %w", err))
	}
	defer logFile.Close()
	logger := slog.New(slog.NewTextHandler(logFile, &slog.HandlerOptions{Level: slog.LevelInfo}))

	database, err := db.Open(ctx, paths.DB)
	if err != nil {
		return fail(err)
	}
	defer database.Close()

	// The database file disappeared while the configuration still lists
	// sources: it was just recreated empty. Ask the next scan to rebuild the
	// baseline and to say so in its report, instead of silently treating the
	// loss of the database as "nothing changed".
	if configExisted && !dbExisted && len(cfg.Sources) > 0 {
		if err := db.SetMeta(ctx, database, db.MetaNeedsBaseline, db.BaselineDBMissing); err != nil {
			return fail(err)
		}
	}

	host, _ := os.Hostname()
	if host == "" {
		host = "unknown"
	}
	app := &monitor.App{
		Paths:    paths,
		Cfg:      cfg,
		DB:       database,
		Log:      logger,
		Stdin:    stdin,
		Stdout:   stdout,
		Stderr:   stderr,
		Verbose:  g.verbose,
		Quiet:    g.quiet,
		Hostname: host,
	}

	// ---- dispatch ---------------------------------------------------------------
	switch cmd {
	case "scan":
		rep, err := app.Scan(ctx)
		if err != nil {
			return fail(err)
		}
		app.PrintScan(rep)
		sinkErrs := app.Notify(ctx, rep)
		if rep.HasErrors() || len(sinkErrs) > 0 {
			return exitNonFatal
		}
		return exitOK

	case "list":
		p := ""
		if len(positional) > 0 {
			p = positional[0]
		}
		err = app.List(ctx, *listFiles, p)
	case "init":
		err = app.Init(ctx, g.yes)
	case "add":
		err = app.Add(ctx, positional[0])
		if err == nil && !configExisted && !g.quiet {
			fmt.Fprintf(stdout, "[fmon] Created configuration at %s\n", paths.Config)
		}
	case "rm":
		err = app.Remove(ctx, positional[0])
	case "history":
		p := ""
		if len(positional) > 0 {
			p = positional[0]
		}
		err = app.History(ctx, p, *limit)
	case "clear":
		switch {
		case *clearAll:
			err = app.Init(ctx, g.yes)
		case *clearHist:
			err = app.ClearHistoryAll(ctx)
		default:
			err = app.ClearHistoryPath(ctx, *clearPath)
		}
	}
	if err != nil {
		if errors.Is(err, monitor.ErrAborted) {
			fmt.Fprintln(stderr, "[fmon] Aborted.")
			return exitFatal
		}
		return fail(err)
	}
	return exitOK
}
