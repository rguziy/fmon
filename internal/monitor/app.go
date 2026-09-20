// Package monitor implements fmon's commands and the scan engine.
package monitor

import (
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/rguziy/fmon/internal/config"
)

// App bundles everything the commands and the scanner need. It is built once
// by main and passed around; tests construct it directly with temporary
// directories.
type App struct {
	Paths    config.Paths
	Cfg      *config.Config
	DB       *sql.DB
	Log      *slog.Logger // operational log written to fmon.log
	Stdin    io.Reader
	Stdout   io.Writer
	Stderr   io.Writer
	Verbose  bool // -v: progress details on stderr
	Quiet    bool // -q: no normal (non-error) output on stdout
	Hostname string

	// Now returns the current time; nil means time.Now. Tests override it.
	Now func() time.Time

	// IsTerminal reports whether interactive confirmation is possible;
	// nil means "inspect os.Stdin".
	IsTerminal func() bool
}

func (a *App) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *App) logger() *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (a *App) stdout() io.Writer {
	if a.Stdout != nil {
		return a.Stdout
	}
	return io.Discard
}

func (a *App) stderr() io.Writer {
	if a.Stderr != nil {
		return a.Stderr
	}
	return io.Discard
}

func (a *App) stdinIsTerminal() bool {
	if a.IsTerminal != nil {
		return a.IsTerminal()
	}
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// infof prints a normal, non-error message on stdout unless -q was given.
// Data that a command exists to produce (history, list) is not routed through
// it, so -q never hides requested output.
func (a *App) infof(format string, args ...any) {
	if !a.Quiet {
		fmt.Fprintf(a.stdout(), format, args...)
	}
}

// warnf prints "[fmon] WARNING: ..." to stderr and records it in the log.
func (a *App) warnf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(a.stderr(), "[fmon] WARNING: %s\n", msg)
	a.logger().Warn(msg)
}

// nonFatal records a problem that does not stop the run (for example an
// unreadable file). It is echoed to stderr, logged, and collected in the
// report so the run can finish with exit code 2.
func (a *App) nonFatal(rep *Report, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	rep.Errors = append(rep.Errors, msg)
	fmt.Fprintf(a.stderr(), "[fmon] ERROR: %s\n", msg)
	a.logger().Error(msg)
}

// vlogf prints progress information to stderr when -v is given.
func (a *App) vlogf(format string, args ...any) {
	if a.Verbose {
		fmt.Fprintf(a.stderr(), "[fmon] "+format+"\n", args...)
	}
}
