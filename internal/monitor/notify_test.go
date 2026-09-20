package monitor

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rguziy/fmon/internal/config"
	"github.com/rguziy/fmon/internal/models"
)

func sampleReport() *Report {
	return &Report{
		Time: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Host: "srv",
		Changes: []models.Change{
			{Event: models.EventModified, Path: "/etc/b.conf", Source: "/etc", OldSize: 1, NewSize: 2},
			{Event: models.EventAdded, Path: "/etc/a.conf", Source: "/etc", NewSize: 7},
			{Event: models.EventDeleted, Path: "/opt/x", Source: "/opt"},
		},
		Alerts: []string{`ALERT: Tracked folder "/mnt/share" was deleted from disk!`},
	}
}

func TestRenderSummaryAndFull(t *testing.T) {
	r := sampleReport()

	sum := r.Render(config.DetailSummary)
	for _, want := range []string{
		"[fmon] host=srv time=2026-01-02T03:04:05Z changes=3 (added 1, modified 1, deleted 1)",
		`ALERT: Tracked folder "/mnt/share" was deleted from disk!`,
		`SOURCE "/etc" added=1 modified=1 deleted=0`,
		`SOURCE "/opt" added=0 modified=0 deleted=1`,
	} {
		if !strings.Contains(sum, want) {
			t.Errorf("summary lacks %q:\n%s", want, sum)
		}
	}
	if strings.Contains(sum, "ADDED    ") {
		t.Errorf("summary must not itemize changes:\n%s", sum)
	}

	full := r.Render(config.DetailFull)
	iA, iB, iX := strings.Index(full, `ADDED    "/etc/a.conf" size=7`), strings.Index(full, `MODIFIED "/etc/b.conf" size 1 -> 2`), strings.Index(full, `DELETED  "/opt/x"`)
	if iA < 0 || iB < 0 || iX < 0 || !(iA < iB && iB < iX) {
		t.Errorf("full ledger missing or not sorted by path:\n%s", full)
	}
}

func TestRenderQuotesHostileFileNames(t *testing.T) {
	r := &Report{Time: time.Now(), Host: "h", Changes: []models.Change{
		{Event: models.EventAdded, Path: "/tmp/evil\n[fmon] ALERT: fake\x1b[31m", Source: "/tmp"},
		{Event: models.EventAdded, Path: "/tmp/файл.txt", Source: "/tmp"},
	}}
	out := r.Render(config.DetailFull)
	if strings.Contains(out, "\x1b") || strings.Count(out, "\n[fmon]") != 0 {
		t.Fatalf("control characters leaked into the report:\n%q", out)
	}
	if !strings.Contains(out, "файл.txt") {
		t.Errorf("readable Unicode must stay readable:\n%s", out)
	}
}

func TestTruncateMessage(t *testing.T) {
	r := &Report{Time: time.Now(), Host: "h"}
	for i := 0; i < 500; i++ {
		r.Changes = append(r.Changes, models.Change{Event: models.EventAdded, Path: fmt.Sprintf("/data/файл-%04d.txt", i), Source: "/data"})
	}
	full := r.Render(config.DetailFull)

	if got := truncateMessage(full, len(full)+10); got != full {
		t.Error("a message that fits must be untouched")
	}
	const limit = 2000
	got := truncateMessage(full, limit)
	if len(got) > limit {
		t.Fatalf("len = %d > %d", len(got), limit)
	}
	if !utf8.ValidString(got) {
		t.Error("truncation produced invalid UTF-8")
	}
	if !strings.HasPrefix(got, "[fmon] host=h") {
		t.Error("header must be kept")
	}
	last := strings.TrimSpace(got[strings.LastIndex(strings.TrimRight(got, "\n"), "\n"):])
	if !strings.HasPrefix(last, "... and ") || !strings.HasSuffix(last, "more changes (see fmon history)") {
		t.Errorf("trailer = %q", last)
	}
	// Every change is either kept or counted in the trailer.
	kept := strings.Count(got, "\nADDED ")
	var omitted int
	fmt.Sscanf(last, "... and %d more changes", &omitted)
	if kept+omitted != 500 {
		t.Errorf("kept %d + omitted %d != 500", kept, omitted)
	}
	// Cut lines are complete (no half-written path).
	for _, l := range strings.Split(strings.TrimSpace(got), "\n") {
		if strings.HasPrefix(l, "ADDED") && !strings.HasSuffix(l, "size=0") {
			t.Errorf("partial line %q", l)
		}
	}
}

func TestNotifyEmptyReportSendsNothing(t *testing.T) {
	e := newEnv(t)
	if errs := e.app.Notify(context.Background(), &Report{Time: time.Now()}); len(errs) != 0 {
		t.Fatal(errs)
	}
	if _, err := os.Stat(e.app.Paths.Log); err == nil {
		t.Error("nothing may be logged for an empty report")
	}
}

func TestNotifyLogSink(t *testing.T) {
	e := newEnv(t)
	e.app.Cfg.Log.Detail = config.DetailFull
	if errs := e.app.Notify(context.Background(), sampleReport()); len(errs) != 0 {
		t.Fatal(errs)
	}
	b, err := os.ReadFile(e.app.Paths.Log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `ADDED    "/etc/a.conf"`) {
		t.Errorf("log = %q", b)
	}
}

func TestNotifyScriptGetsMessageAndEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test")
	}
	e := newEnv(t)
	dir := t.TempDir()
	outFile := filepath.Join(dir, "out.txt")
	summary := filepath.Join(dir, "summary.sh")
	full := filepath.Join(dir, "full.sh")
	script := "#!/bin/sh\nprintf '%s\\n' \"$1\" > " + outFile + ".$FMON_DETAIL\nenv | grep '^FMON_' | sort > " + outFile + ".$FMON_DETAIL.env\n"
	for _, p := range []string{summary, full} {
		if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e.app.Cfg.Scripts = []config.ScriptConfig{
		{Path: summary, Detail: config.DetailSummary, Timeout: "10s", MaxMessageBytes: 100000},
		{Path: full, Detail: config.DetailFull, Timeout: "10s", MaxMessageBytes: 100000},
	}
	rep := sampleReport()
	rep.MissingSources = 1
	if errs := e.app.Notify(context.Background(), rep); len(errs) != 0 {
		t.Fatal(errs)
	}

	s, _ := os.ReadFile(outFile + ".summary")
	f, _ := os.ReadFile(outFile + ".full")
	if !strings.Contains(string(s), `SOURCE "/etc"`) || strings.Contains(string(s), "ADDED    ") {
		t.Errorf("summary script got %q", s)
	}
	if !strings.Contains(string(f), `ADDED    "/etc/a.conf"`) {
		t.Errorf("full script got %q", f)
	}
	envOut, _ := os.ReadFile(outFile + ".full.env")
	for _, want := range []string{"FMON_TOTAL=3", "FMON_ADDED=1", "FMON_MODIFIED=1", "FMON_DELETED=1", "FMON_MISSING_SOURCES=1", "FMON_HOSTNAME=srv", "FMON_DETAIL=full", "FMON_TIMESTAMP=2026-01-02T03:04:05Z"} {
		if !strings.Contains(string(envOut), want) {
			t.Errorf("environment lacks %s:\n%s", want, envOut)
		}
	}
}

func TestNotifyScriptFailureAndTimeoutDoNotBlockOthers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script test")
	}
	e := newEnv(t)
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.sh")
	slow := filepath.Join(dir, "slow.sh")
	good := filepath.Join(dir, "good.sh")
	marker := filepath.Join(dir, "ran")
	_ = os.WriteFile(bad, []byte("#!/bin/sh\necho boom >&2\nexit 3\n"), 0o755)
	_ = os.WriteFile(slow, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755)
	_ = os.WriteFile(good, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755)
	e.app.Cfg.Scripts = []config.ScriptConfig{
		{Path: bad, Detail: "summary", Timeout: "10s", MaxMessageBytes: 1000},
		{Path: slow, Detail: "summary", Timeout: "300ms", MaxMessageBytes: 1000},
		{Path: good, Detail: "summary", Timeout: "10s", MaxMessageBytes: 1000},
	}

	start := time.Now()
	errs := e.app.Notify(context.Background(), sampleReport())
	if time.Since(start) > 10*time.Second {
		t.Fatal("the slow script was not stopped by its timeout")
	}
	if len(errs) != 2 {
		t.Fatalf("errors = %v, want 2 (bad + slow)", errs)
	}
	if !strings.Contains(errs[0].Error(), "boom") || !strings.Contains(errs[1].Error(), "timed out") {
		t.Errorf("unexpected error texts: %v", errs)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("the third script must still run after earlier failures")
	}
}

// fakeSMTP is a minimal SMTP server that accepts one message.
type fakeSMTP struct {
	addr string
	msg  chan string
	from chan string
	rcpt chan string
}

func startFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s := &fakeSMTP{addr: ln.Addr().String(), msg: make(chan string, 1), from: make(chan string, 1), rcpt: make(chan string, 4)}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		w := func(l string) { fmt.Fprintf(conn, "%s\r\n", l) }
		w("220 fake ESMTP")
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				w("250 fake")
			case strings.HasPrefix(cmd, "MAIL FROM"):
				s.from <- strings.TrimSpace(line)
				w("250 ok")
			case strings.HasPrefix(cmd, "RCPT TO"):
				s.rcpt <- strings.TrimSpace(line)
				w("250 ok")
			case cmd == "DATA":
				w("354 go")
				var sb strings.Builder
				for {
					l, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if l == ".\r\n" {
						break
					}
					sb.WriteString(l)
				}
				s.msg <- sb.String()
				w("250 queued")
			case cmd == "QUIT":
				w("221 bye")
				return
			default:
				w("250 ok")
			}
		}
	}()
	return s
}

func TestNotifySMTP(t *testing.T) {
	e := newEnv(t)
	srv := startFakeSMTP(t)
	host, portStr, _ := net.SplitHostPort(srv.addr)
	var port int
	fmt.Sscanf(portStr, "%d", &port)
	e.app.Cfg.SMTP = config.SMTPConfig{
		Enabled: true, Host: host, Port: port, Security: config.SecurityNone,
		From: "fmon@example.com", To: []string{"a@example.com", "b@example.com"}, Detail: config.DetailFull,
	}
	rep := sampleReport()
	rep.Changes = append(rep.Changes, models.Change{Event: models.EventAdded, Path: "/tmp/файл.txt", Source: "/tmp"})

	if errs := e.app.Notify(context.Background(), rep); len(errs) != 0 {
		t.Fatal(errs)
	}
	select {
	case msg := <-srv.msg:
		for _, want := range []string{
			"From: fmon@example.com\r\n", "To: a@example.com, b@example.com\r\n",
			"Content-Transfer-Encoding: quoted-printable", "Subject: ",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("message lacks %q:\n%s", want, msg)
			}
		}
		if !strings.Contains(msg, "ALERT") { // the subject is Q-encoded only if non-ASCII; it is ASCII here
			t.Errorf("subject should announce the alert:\n%s", msg)
		}
		for _, l := range strings.Split(msg, "\r\n") {
			if len(l) > 78 {
				t.Errorf("line longer than 78 chars: %q", l)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no message received")
	}
	if len(srv.rcpt) != 2 {
		t.Errorf("recipients = %d, want 2", len(srv.rcpt))
	}
}

func TestNotifySMTPStartTLSRequired(t *testing.T) {
	e := newEnv(t)
	srv := startFakeSMTP(t) // does not offer STARTTLS
	host, portStr, _ := net.SplitHostPort(srv.addr)
	var port int
	fmt.Sscanf(portStr, "%d", &port)
	e.app.Cfg.SMTP = config.SMTPConfig{
		Enabled: true, Host: host, Port: port, Security: config.SecurityStartTLS,
		From: "f@example.com", To: []string{"a@example.com"}, Detail: config.DetailSummary,
	}
	errs := e.app.Notify(context.Background(), sampleReport())
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "STARTTLS") {
		t.Fatalf("errors = %v; unencrypted delivery must be refused", errs)
	}
}
