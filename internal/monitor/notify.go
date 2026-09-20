package monitor

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/smtp"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rguziy/fmon/internal/config"
	"github.com/rguziy/fmon/internal/models"
)

// Render formats the report as text.
//
// The "summary" level contains the header, alerts, notices and per-source
// counts; "full" additionally lists every change. All paths are quoted with
// strconv.Quote: file names are attacker-controlled, and quoting neutralizes
// embedded newlines and control characters (log/message injection) while
// keeping readable Unicode intact.
func (r *Report) Render(detail string) string {
	var b strings.Builder
	added, modified, deleted := r.Counts()

	fmt.Fprintf(&b, "[fmon] host=%s time=%s changes=%d (added %d, modified %d, deleted %d)\n",
		r.Host, r.Time.Format(time.RFC3339), len(r.Changes), added, modified, deleted)
	for _, s := range r.Alerts {
		b.WriteString(s + "\n")
	}
	for _, s := range r.Notices {
		b.WriteString(s + "\n")
	}
	if len(r.Errors) > 0 {
		fmt.Fprintf(&b, "WARNING: %d non-fatal error(s) occurred during the scan; see fmon.log\n", len(r.Errors))
	}

	type counts struct{ added, modified, deleted int }
	per := make(map[string]*counts)
	for _, c := range r.Changes {
		n := per[c.Source]
		if n == nil {
			n = &counts{}
			per[c.Source] = n
		}
		switch c.Event {
		case models.EventAdded:
			n.added++
		case models.EventModified:
			n.modified++
		case models.EventDeleted:
			n.deleted++
		}
	}
	sources := make([]string, 0, len(per))
	for s := range per {
		sources = append(sources, s)
	}
	sort.Strings(sources)
	for _, s := range sources {
		n := per[s]
		fmt.Fprintf(&b, "SOURCE %s added=%d modified=%d deleted=%d\n", strconv.Quote(s), n.added, n.modified, n.deleted)
	}

	if detail == config.DetailFull {
		for _, l := range changeLines(r.Changes) {
			b.WriteString(l + "\n")
		}
	}
	return b.String()
}

// changeLines formats changes as one line each, sorted by path. Paths are
// quoted (see Render). Used by both the notifications and the scan output.
func changeLines(changes []models.Change) []string {
	sorted := append([]models.Change(nil), changes...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })

	lines := make([]string, 0, len(sorted))
	for _, c := range sorted {
		q := strconv.Quote(c.Path)
		switch c.Event {
		case models.EventAdded:
			lines = append(lines, fmt.Sprintf("ADDED    %s size=%d", q, c.NewSize))
		case models.EventModified:
			lines = append(lines, fmt.Sprintf("MODIFIED %s size %d -> %d", q, c.OldSize, c.NewSize))
		case models.EventDeleted:
			lines = append(lines, fmt.Sprintf("DELETED  %s", q))
		}
	}
	return lines
}

func isChangeLine(l string) bool {
	return strings.HasPrefix(l, "ADDED ") || strings.HasPrefix(l, "MODIFIED ") || strings.HasPrefix(l, "DELETED ")
}

// cutUTF8 shortens s to at most n bytes without splitting a UTF-8 sequence.
func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 {
		r, size := utf8.DecodeLastRuneInString(s)
		if r != utf8.RuneError || size != 1 {
			break
		}
		s = s[:len(s)-1]
	}
	return s
}

// truncateMessage limits msg to maxBytes at a line boundary and, when lines
// were dropped, appends "... and N more changes (see fmon history)".
func truncateMessage(msg string, maxBytes int) string {
	if len(msg) <= maxBytes {
		return msg
	}
	const reserve = 80 // room for the trailer line
	budget := maxBytes - reserve
	if budget < 0 {
		budget = 0
	}

	lines := strings.Split(strings.TrimRight(msg, "\n"), "\n")
	var b strings.Builder
	kept := 0
	for _, l := range lines {
		if b.Len()+len(l)+1 > budget {
			break
		}
		b.WriteString(l)
		b.WriteByte('\n')
		kept++
	}
	if kept == 0 && len(lines) > 0 { // even the header does not fit: cut it
		b.WriteString(cutUTF8(lines[0], budget))
		b.WriteByte('\n')
		kept = 1
	}

	omitted := 0
	for _, l := range lines[kept:] {
		if isChangeLine(l) {
			omitted++
		}
	}
	if omitted > 0 {
		fmt.Fprintf(&b, "... and %d more changes (see fmon history)\n", omitted)
	} else {
		b.WriteString("... (message truncated)\n")
	}
	return b.String()
}

// Notify dispatches the consolidated report to every configured sink: the
// local log, SMTP and each notification script. Nothing is sent when the
// report is empty. A failing sink never prevents the others from running; all
// failures are returned.
func (a *App) Notify(ctx context.Context, rep *Report) []error {
	if rep.Empty() {
		return nil
	}

	var errs []error
	fail := func(sink string, err error) {
		err = fmt.Errorf("notification sink %s failed: %w", sink, err)
		errs = append(errs, err)
		fmt.Fprintf(a.stderr(), "[fmon] ERROR: %v\n", err)
		a.logger().Error(err.Error())
	}

	// 1. Local log file.
	if err := a.appendLog(rep.Render(a.Cfg.Log.Detail)); err != nil {
		fail("log", err)
	}

	// 2. E-mail.
	if a.Cfg.SMTP.Enabled {
		if err := a.sendMail(ctx, rep); err != nil {
			fail("smtp", err)
		}
	}

	// 3. Scripts: one invocation each, with its own detail level.
	for _, sc := range a.Cfg.Scripts {
		if err := a.runScript(ctx, rep, sc); err != nil {
			fail("script "+sc.Path, err)
		}
	}
	return errs
}

func (a *App) appendLog(text string) error {
	f, err := os.OpenFile(a.Paths.Log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(text + "\n"); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// ---- scripts --------------------------------------------------------------------

func (a *App) runScript(ctx context.Context, rep *Report, sc config.ScriptConfig) error {
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(sc.Path); err == nil && info.Mode().Perm()&0o022 != 0 {
			a.warnf("notification script %q is group/world-writable; anyone able to modify it can run code as this user", sc.Path)
		}
	}

	msg := strings.TrimRight(truncateMessage(rep.Render(sc.Detail), sc.MaxMessageBytes), "\n")

	runCtx, cancel := context.WithTimeout(ctx, sc.TimeoutDuration())
	defer cancel()

	// The message is passed as a single argument (never through a shell), so
	// it cannot be interpreted as commands.
	cmd := exec.CommandContext(runCtx, sc.Path, msg)
	cmd.WaitDelay = 5 * time.Second
	added, modified, deleted := rep.Counts()
	cmd.Env = append(os.Environ(),
		"FMON_TOTAL="+strconv.Itoa(len(rep.Changes)),
		"FMON_ADDED="+strconv.Itoa(added),
		"FMON_MODIFIED="+strconv.Itoa(modified),
		"FMON_DELETED="+strconv.Itoa(deleted),
		"FMON_MISSING_SOURCES="+strconv.Itoa(rep.MissingSources),
		"FMON_TIMESTAMP="+rep.Time.Format(time.RFC3339),
		"FMON_HOSTNAME="+rep.Host,
		"FMON_DETAIL="+sc.Detail,
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Run(); err != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("timed out after %s", sc.TimeoutDuration())
		}
		if tail := strings.TrimSpace(cutUTF8(out.String(), 500)); tail != "" {
			return fmt.Errorf("%w (output: %s)", err, strconv.Quote(tail))
		}
		return err
	}
	return nil
}

// ---- e-mail ---------------------------------------------------------------------

// headerSafe strips CR/LF so configuration or host values cannot inject
// additional mail headers.
func headerSafe(s string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}

func (a *App) sendMail(ctx context.Context, rep *Report) error {
	cfg := a.Cfg.SMTP

	password, err := cfg.Password()
	if err != nil {
		return err
	}

	subject := fmt.Sprintf("[fmon] %s: %d change(s)", headerSafe(rep.Host), len(rep.Changes))
	if len(rep.Alerts) > 0 {
		subject = fmt.Sprintf("[fmon] ALERT on %s: %d change(s)", headerSafe(rep.Host), len(rep.Changes))
	}
	body := rep.Render(cfg.Detail)

	dialer := &net.Dialer{Timeout: 30 * time.Second}
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))

	var conn net.Conn
	if cfg.Security == config.SecurityTLS {
		td := &tls.Dialer{NetDialer: dialer, Config: &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}}
		conn, err = td.DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("connect to %s: %w", addr, err)
	}
	// Bound the whole conversation so a stalled server cannot hang the scan.
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))

	c, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("smtp handshake: %w", err)
	}
	defer c.Close()

	if cfg.Security == config.SecurityStartTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("server does not offer STARTTLS; refusing to send unencrypted (use security = \"none\" only for trusted local relays)")
		}
		if err := c.StartTLS(&tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("starttls: %w", err)
		}
	}
	if cfg.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", cfg.Username, password, cfg.Host)); err != nil {
			return fmt.Errorf("authenticate: %w", err)
		}
	}

	if err := c.Mail(headerSafe(cfg.From)); err != nil {
		return fmt.Errorf("MAIL FROM: %w", err)
	}
	for _, to := range cfg.To {
		if err := c.Rcpt(headerSafe(to)); err != nil {
			return fmt.Errorf("RCPT TO %s: %w", to, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("DATA: %w", err)
	}

	var head bytes.Buffer
	fmt.Fprintf(&head, "From: %s\r\n", headerSafe(cfg.From))
	fmt.Fprintf(&head, "To: %s\r\n", headerSafe(strings.Join(cfg.To, ", ")))
	fmt.Fprintf(&head, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", subject))
	fmt.Fprintf(&head, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	head.WriteString("MIME-Version: 1.0\r\n")
	head.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	head.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
	if _, err := w.Write(head.Bytes()); err != nil {
		return fmt.Errorf("write message: %w", err)
	}
	// Quoted-printable keeps every line short (RFC 5322 limit) even when a
	// path is very long, and copes with non-ASCII file names.
	qp := quotedprintable.NewWriter(w)
	if _, err := qp.Write([]byte(body)); err != nil {
		return fmt.Errorf("write message body: %w", err)
	}
	if err := qp.Close(); err != nil {
		return fmt.Errorf("write message body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("finish message: %w", err)
	}
	return c.Quit()
}
