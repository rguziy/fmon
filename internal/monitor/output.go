package monitor

import (
	"fmt"
	"time"
)

// PrintScan writes the result of a scan to stdout: alerts and notices, every
// change (one line each), and a statistics line. It prints nothing with -q.
//
// This is the interactive view and is independent of the notification sinks:
// it always lists every change, whatever "detail" the sinks are set to.
// Errors and warnings are not repeated here, they already went to stderr.
func (a *App) PrintScan(rep *Report) {
	if a.Quiet {
		return
	}
	w := a.stdout()

	printed := 0
	for _, s := range rep.Alerts {
		fmt.Fprintln(w, s)
		printed++
	}
	for _, s := range rep.Notices {
		fmt.Fprintln(w, s)
		printed++
	}
	for _, l := range changeLines(rep.Changes) {
		fmt.Fprintln(w, l)
		printed++
	}
	if printed > 0 {
		fmt.Fprintln(w)
	}

	added, modified, deleted := rep.Counts()
	prefix := ""
	if rep.Empty() {
		prefix = "No changes. "
	}
	missing := ""
	if rep.MissingSources > 0 {
		missing = fmt.Sprintf(" (%d missing)", rep.MissingSources)
	}
	fmt.Fprintf(w, "%sScan finished in %s: %d source(s)%s, %d file(s) scanned (%d hashed), %d added, %d modified, %d deleted, %d error(s)\n",
		prefix, rep.Duration.Round(time.Millisecond), rep.Sources, missing,
		rep.FilesScanned, rep.FilesHashed, added, modified, deleted, len(rep.Errors))
}
