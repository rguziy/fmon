# 📜 fmon

[![CI](https://github.com/rguziy/fmon/actions/workflows/ci.yml/badge.svg)](https://github.com/rguziy/fmon/actions/workflows/ci.yml)

**fmon** is a lightweight File Integrity Monitoring (FIM) tool for Linux, macOS
and Windows. It takes a snapshot of the files and folders you care about,
compares it with the previous snapshot, and tells you what was **added**,
**modified** or **deleted**, in a single consolidated notification.

There is no daemon and no file watcher. You run `fmon scan` from cron or a
systemd timer, and fmon exits when it is done.

- Pure Go, static binaries (`CGO_ENABLED=0`), no runtime dependencies
- Runs on Linux (amd64, arm64, ARMv5/v6/v7), Windows and macOS (Intel and Apple Silicon)
- Fast: a cheap size + mtime check first, xxHash64 only for files that look changed
- Memory safe: files are hashed as a stream, so multi-gigabyte files need no extra memory
- One notification per scan (never one per file), sent to a log file, e-mail and/or your own scripts
- Persistent audit history in an embedded SQLite database

## 📚 Contents

- [📜 fmon](#-fmon)
  - [📚 Contents](#-contents)
  - [📖 How it works](#-how-it-works)
  - [📦 Installation](#-installation)
    - [Download a release](#download-a-release)
    - [With Go](#with-go)
    - [From source](#from-source)
  - [🚀 Quick start](#-quick-start)
  - [💻 Commands](#-commands)
    - [Adding sources](#adding-sources)
    - [Listing what is watched](#listing-what-is-watched)
    - [Scan output](#scan-output)
  - [🔧 Configuration](#-configuration)
  - [🔔 Notifications](#-notifications)
    - [Sinks](#sinks)
    - [Script interface](#script-interface)
    - [Log rotation](#log-rotation)
  - [⏰ Scheduling](#-scheduling)
  - [🚦 Exit codes](#-exit-codes)
  - [🔍 Behavior details](#-behavior-details)
    - [A watched source disappears](#a-watched-source-disappears)
    - [Unreadable files and folders](#unreadable-files-and-folders)
    - [History](#history)
    - [Upgrades](#upgrades)
  - [🔒 Security notes](#-security-notes)
  - [📁 Files and locations](#-files-and-locations)
  - [🔨 Building from source](#-building-from-source)
  - [📄 License](#-license)

## 📖 How it works

`fmon scan` performs one snapshot run:

1. **Stat pass.** For every regular file in a watched source, the current size
   and modification time are compared with the stored values. If both match,
   the file is skipped without being read.
2. **Hash pass.** If size or mtime differ, the file is stream-hashed with
   xxHash64. If the hash is unchanged (for example after `touch`), the
   metadata is refreshed silently. If the hash differs, a `MODIFIED` event is
   recorded.
3. Files that are new inside a watched folder are `ADDED`; files that
   disappeared are `DELETED`.

All changes of a run are collected first. Then they are recorded in the
history, printed to the terminal together with the scan statistics (see
[Scan output](#scan-output)), and sent as **one** consolidated report to every
configured sink. When nothing changed, nothing is sent; the terminal shows only
the statistics line (or nothing at all with `-q`).

Only regular files are tracked. Symbolic links, block/character devices, named
pipes and sockets are ignored completely.

## 📦 Installation

### Download a release

Download the archive for your platform from the
[Releases](https://github.com/rguziy/fmon/releases) page, verify it against
`SHA256SUMS`, and unpack it:

```sh
sha256sum -c --ignore-missing SHA256SUMS
unzip fmon_1.0.0_linux_amd64.zip
sudo install -m 0755 fmon /usr/local/bin/fmon
```

Available archives: `linux_amd64`, `linux_arm64`, `linux_armv5`, `linux_armv6`,
`linux_armv7`, `windows_amd64`, `darwin_amd64`, `darwin_arm64`.

### With Go

```sh
go install github.com/rguziy/fmon/cmd/fmon@latest
```

### From source

See [Building from source](#-building-from-source).

## 🚀 Quick start

```sh
# 0. Optional: create the configuration and database explicitly
fmon init

# 1. Start watching a folder and a single file (this indexes their current state)
fmon add /etc
fmon add /opt/app/config.yml

# 2. See what is watched
fmon list

# 3. Take a snapshot and compare
fmon scan

# 4. Look at the change history
fmon history
fmon history /etc --limit 20
```

On a machine where fmon has never been set up, `fmon add` and `fmon init`
create the configuration directory, `fmon.toml` and the database. Every other
command creates nothing and tells you what to do instead:

```
$ fmon scan
[fmon] ERROR: No configuration found at /root/.config/fmon/fmon.toml. Run 'fmon init' to create it.
```

Then schedule `fmon scan` (see [Scheduling](#-scheduling)) and configure a
notification sink in `fmon.toml`.

## 💻 Commands

```
fmon [global flags] <command> [arguments]
```

| Command | Description |
|---|---|
| `fmon` (no command) | Print the list of commands (not an error, exit code 0). |
| `fmon scan [--full] [path...]` | Take a snapshot, record changes, print them and the statistics, notify. `fmon --scan` is an alias for a plain `fmon scan` with no arguments. `--full` hashes every file, bypassing the size+mtime shortcut. With one or more paths (each an exact watched source, as shown by `fmon list`): scan only those; with none: scan every watched source. |
| `fmon list [--files] [path]` | List the watched files and folders. With `--files`: every tracked file (hash, size, mtime, path), optionally limited to a file or folder tree. |
| `fmon add <path>` | Watch a file or folder and index its current contents as the baseline. |
| `fmon rm <path>` | Stop watching an exact watched source. History is kept. |
| `fmon history [path] [--limit N]` | Show recorded changes in chronological order: oldest first, newest last. With a path: that file, or everything below that folder. `--limit N` shows the **last** N records (default 100, `0` means unlimited). |
| `fmon clear --history-all` | Delete the whole change history. |
| `fmon clear --path=<path>` | Delete history records of a file or a folder tree. |
| `fmon clear --all` / `fmon init` | Create the configuration if it does not exist; otherwise reset everything: database, watched sources and history. Other `fmon.toml` settings are kept. |
| `fmon version` | Print the version (also `--version`). |

Global flags (accepted before or after the command):

| Flag | Description |
|---|---|
| `--config-dir <dir>` | Configuration directory. See [Files and locations](#-files-and-locations). |
| `-v` | Verbose progress on stderr (per-source file counts). |
| `-q`, `--quiet` | No normal output on stdout: `scan` prints nothing, `add`/`rm`/`init`/`clear` print no confirmations. Warnings and errors still go to stderr. Intended for cron and timers. |
| `--yes` | Skip the confirmation prompt of `init` / `clear --all`. |

`init` and `clear --all` ask for confirmation (`[y/N]`) when there is data to
delete. When stdin is not a terminal, they refuse to continue unless `--yes` is
given.

### Adding sources

- `fmon add <path>` accepts a regular file or a directory. The path is made
  absolute and symbolic links are resolved; the resolved path is what gets
  stored (fmon tells you when it differs from what you typed).
- **Overlapping sources are not allowed.** If the path equals, lies inside, or
  contains an already watched source, fmon prints a warning and adds nothing:

  ```
  [fmon] WARNING: /etc/ssh overlaps watched source /etc; nothing added
  ```

- Folders are indexed recursively. Files matching an [exclude pattern](#-configuration)
  are skipped.

### Listing what is watched

`fmon list` shows every watched source without opening `fmon.toml`:

```
$ fmon list
TYPE    STATUS   FILES  SIZE      ADDED                      PATH
folder  active   1234   45.2 MiB  2026-09-20T12:00:00+03:00  "/etc"
folder  missing  87     3.4 MiB   2026-09-20T12:01:10+03:00  "/mnt/share"
file    active   1      1.1 KiB   2026-09-20T12:00:05+03:00  "/opt/app/config.yml"

3 source(s), 1322 file(s) tracked, 48.7 MiB
Exclude patterns: "*.tmp" "*.swp" "**/.git/**"
```

`STATUS` is `active`, or `missing` when the path is gone from disk. Two more
values reveal that `fmon.toml` and the database disagree (for example after a
manual edit): `pending` means the source is listed in `fmon.toml` but not
indexed yet (the next scan indexes it), and `orphaned` means it is in the
database but no longer in `fmon.toml` (the next scan removes it).

`fmon list --files` prints one line per tracked file: hash, size, modification
time and the quoted path. `fmon list --files /etc/ssh` limits it to that file
or folder tree.

### Full scans (`--full`)

By default a scan trusts the size + modification time of a file: if both are
unchanged since the last scan, the file is not read. This is fast, but it can
miss content that changed while size and mtime stayed the same — for example
bits flipped by a failing disk or a corrupted filesystem, if the write never
went through a normal `write()` that would update the file's metadata.

`fmon scan --full` hashes every regular file in every watched source,
regardless of its stored metadata, so this kind of silent corruption is still
caught. It is slower (every byte of every tracked file is read from disk), so
schedule it separately from the fast, frequent scan — daily or weekly rather
than every few minutes — and expect it to take noticeably longer on a large
archive.

```cron
# Fast check every 15 minutes
*/15 * * * *  /usr/local/bin/fmon scan -q
# Full re-hash once a week, catches silent corruption the fast check cannot
0 3 * * 1     /usr/local/bin/fmon scan --full -q
```

### Scanning selected sources

`fmon scan` normally goes through every watched source. Add one or more paths
to scan only those, leaving the rest untouched for this run — each path must
be the exact path of a watched source, as `fmon list` shows it (a path merely
inside a source, or a source you never added, is rejected):

```sh
fmon scan /mnt/usb/photo
fmon scan --full /mnt/usb/photo /mnt/nas/films
```

This is mainly useful to spread `--full` re-hashes of several large,
independent archives across different nights instead of running them all back
to back:

```cron
# Fast check across everything, daily
0  13 * * *   /usr/local/bin/fmon scan -q
# Full re-hash, one archive per night
0  3  * * 0   /usr/local/bin/fmon scan --full -q /mnt/usb/photo
0  3  * * 1   /usr/local/bin/fmon scan --full -q /mnt/nas/films
0  3  * * 2   /usr/local/bin/fmon scan --full -q /mnt/nas/music
```

A run restricted this way still reconciles `fmon.toml` against the database
for every source beforehand (so `fmon list` stays accurate), only the
diffing/hashing step is limited to the paths given.

### Scan output

`fmon scan` prints the alerts, every change, and a statistics line. Paths are
quoted, exactly as in notifications:

```
$ fmon scan
ALERT: Tracked folder "/mnt/share" was deleted from disk!
ADDED    "/etc/new.conf" size=120
MODIFIED "/etc/passwd" size 1834 -> 1861
DELETED  "/opt/app/old.bin"

Scan finished in 1.2s: 3 source(s) (1 missing), 12345 file(s) scanned (17 hashed), 1 added, 1 modified, 1 deleted, 0 error(s)
```

When nothing changed, only the statistics remain:

```
No changes. Scan finished in 0.4s: 3 source(s), 12345 file(s) scanned (0 hashed), 0 added, 0 modified, 0 deleted, 0 error(s)
```

"Scanned" counts every regular file that was examined; "hashed" counts those
whose size or mtime changed (or every file, with `--full`), so their content
had to be read. This output is independent of the notification sinks (it
always lists every change). Add `-q` to suppress it, which is what you want in
cron.

## 🔧 Configuration

fmon reads `fmon.toml` from its configuration directory. The file is created
the first time the source list changes (`fmon add`, `rm`, `init`). You can edit
it by hand at any time.

```toml
# Watched files and folders. Managed by "fmon add" / "fmon rm".
sources = ["/etc", "/opt/app/config.yml"]

# Global exclude patterns, applied to files found inside watched folders.
#  - a pattern without "/" matches the base name:  "*.tmp"
#  - a pattern with "/" matches the full absolute path (doublestar globbing,
#    "**" spans directories): "**/.git/**", "/var/log/**"
exclude = ["*.tmp", "*.swp", "**/.git/**"]

[log]
detail = "full"              # "summary" | "full"

[smtp]
enabled = false
host = "smtp.example.com"
port = 587
security = "starttls"        # "starttls" | "tls" | "none"
username = ""
password_env = "FMON_SMTP_PASSWORD"   # environment variable holding the password
password_file = ""           # or a file (mode 0600) holding the password; wins over password_env
from = "fmon@example.com"
to = ["admin@example.com"]
detail = "full"

# Repeat the table for several scripts. Each script gets its own message.
[[script]]
path = "/usr/local/bin/notify.sh"
detail = "summary"           # "summary" | "full"
timeout = "30s"
max_message_bytes = 100000   # 30000 by default on Windows
```

Notes:

- `fmon.toml` is the source of truth for `sources`. At the start of every scan
  fmon reconciles the database with it: a source you added by hand is
  baselined silently, a source you deleted by hand is removed from the database.
- Unknown settings are rejected, so typos do not go unnoticed.
- fmon rewrites the file (atomically, mode `0600`) whenever the source list
  changes. All settings are preserved, **but hand-written comments are not**.
  Keep notes elsewhere.
- Exclude patterns apply only to files discovered inside watched folders. A
  file added explicitly with `fmon add <file>` is always tracked. Directories
  that match a pattern are skipped entirely. When you extend the list, files
  that become excluded are forgotten silently (no `DELETED` events).
- On Windows, write patterns with forward slashes (`C:/Users/me/**/*.tmp`).

## 🔔 Notifications

Every scan that finds changes, alerts or notices produces **one** report:

```
[fmon] host=srv01 time=2026-09-20T12:00:00+03:00 changes=3 (added 1, modified 1, deleted 1)
ALERT: Tracked folder "/mnt/share" was deleted from disk!
SOURCE "/etc" added=1 modified=1 deleted=0
SOURCE "/opt/app" added=0 modified=0 deleted=1
ADDED    "/etc/new.conf" size=120
MODIFIED "/etc/passwd" size 1834 -> 1861
DELETED  "/opt/app/old.bin"
```

Every sink has its own `detail` level:

- `summary`: the header, alerts, notices and per-source counts;
- `full`: the same plus the itemized list of every change, sorted by path.

File names are attacker-controlled, so all paths are quoted like Go strings
(`"..."` with escapes). A file named `x\n[fmon] ALERT: fake` cannot forge extra
lines in your logs, mails or chat messages. Readable Unicode names stay
readable.

### Sinks

1. **Log file** (`fmon.log` in the configuration directory). The report is
   appended; fmon's operational messages (warnings, errors) go there too. Use
   logrotate with `copytruncate` (see below) to keep it small.
2. **SMTP** when `[smtp] enabled = true`. `starttls` requires the server to
   offer STARTTLS and refuses to send unencrypted otherwise; `tls` is implicit
   TLS (usually port 465); `none` is for trusted local relays and cannot use
   authentication. Passwords come from `password_file` (must not be readable by
   group/others) or from the environment variable named by `password_env`.
3. **Scripts** (`[[script]]`, any number). fmon runs each script directly (no
   shell) with the rendered report as the **first argument (`$1`)**.

### Script interface

- The message is truncated at a line boundary to `max_message_bytes` and ends
  with `... and N more changes (see fmon history)` when lines were dropped.
  The Linux limit for a single argument is about 128 KB. For chat services
  choose a smaller limit (Telegram messages are at most 4096 characters, so
  `max_message_bytes = 3500` is a safe choice).
- A script is stopped after `timeout`.
- These environment variables are set in addition to the inherited environment:

| Variable | Meaning |
|---|---|
| `FMON_TOTAL` | Number of changes |
| `FMON_ADDED`, `FMON_MODIFIED`, `FMON_DELETED` | Counts per event type |
| `FMON_MISSING_SOURCES` | Watched sources currently missing from disk |
| `FMON_TIMESTAMP` | Scan time, RFC 3339 |
| `FMON_HOSTNAME` | Host name |
| `FMON_DETAIL` | `summary` or `full` for this script |

- A failing or timed-out sink is logged and does not stop the other sinks. The
  run then ends with exit code 2.
- fmon warns if a script is group/world-writable, because the script runs with
  the privileges of whoever runs fmon (often root).

Example Telegram script:

```sh
#!/bin/sh
# /usr/local/bin/fmon-telegram.sh   (chmod 700)
TOKEN="123456:ABC..."
CHAT_ID="-1001234567890"
curl -fsS -m 20 "https://api.telegram.org/bot${TOKEN}/sendMessage" \
     --data-urlencode "chat_id=${CHAT_ID}" \
     --data-urlencode "text=$1" >/dev/null
```

```toml
[[script]]
path = "/usr/local/bin/fmon-telegram.sh"
detail = "summary"
max_message_bytes = 3500
```

### Log rotation

```
# /etc/logrotate.d/fmon
/root/.config/fmon/fmon.log {
    weekly
    rotate 8
    compress
    missingok
    notifempty
    copytruncate
}
```

## ⏰ Scheduling

fmon does not watch files continuously. Schedule the scan.

**cron.** `fmon scan` prints its result, and cron mails any output. Either
add `-q` (fmon then prints nothing on stdout; warnings and errors still reach
stderr) or set `MAILTO=""` in the crontab to discard mail. `-q` is the more
flexible option: you keep real problems visible while the routine statistics
line stays out of your mailbox.

```cron
*/30 * * * * FMON_CONFIG_DIR=/root/.config/fmon /usr/local/bin/fmon scan -q
```

**systemd timer** (the journal keeps the scan output, so `-q` is optional here):

```ini
# /etc/systemd/system/fmon.service
[Unit]
Description=fmon file integrity scan

[Service]
Type=oneshot
Environment=FMON_CONFIG_DIR=/root/.config/fmon
ExecStart=/usr/local/bin/fmon scan
```

```ini
# /etc/systemd/system/fmon.timer
[Unit]
Description=Run fmon every 30 minutes

[Timer]
OnBootSec=5min
OnUnitActiveSec=30min
Persistent=true

[Install]
WantedBy=timers.target
```

```sh
systemctl enable --now fmon.timer
```

Two runs never interleave: a second `fmon scan` that starts while another is
running waits up to 5 seconds for it, then exits with
`another fmon run is in progress` (exit code 1).

## 🚦 Exit codes

| Code | Meaning |
|---|---|
| `0` | The scan completed. Changes are **not** an error, so a timer does not treat a scan that found changes as failed. |
| `1` | Fatal error: invalid configuration, database problem, another run in progress, bad usage. |
| `2` | The scan completed, but with non-fatal errors: unreadable files or directories, or a notification sink that failed. |

## 🔍 Behavior details

### A watched source disappears

If a watched folder (or file) vanishes, for example an unmounted NFS share or
USB disk, fmon does **not** throw away its baseline:

- On the first scan that notices it, the source is marked `missing`, one
  `DELETED` record for the source path is written to the history, and
  `ALERT: Tracked folder "<path>" was deleted from disk!` becomes part of the
  scan's single consolidated report. All other sources are still scanned.
- On the following runs, only a warning is printed to stderr (and logged), with
  no notification, until you resolve it:

  ```
  [fmon] WARNING: Configured folder "/mnt/share" does not exist. Please remove it via fmon rm
  ```

- If the path comes back, a `NOTICE` is reported and the files are compared
  with the retained baseline, so unchanged files are not reported as new.
- `fmon rm <path>` removes the source and its stored file states. The history
  is kept.

### Unreadable files and folders

Permission errors are reported on stderr and in the log, counted as non-fatal
errors (exit code 2), and never turn into `DELETED` events: the stored state of
an unreadable file, or of everything below an unreadable directory, is kept.

A file that changes while it is being hashed is caught on the next scan,
because its metadata is recorded from before the hash was computed.

### History

`fmon history` prints chronologically, the newest record last, so the end of the
output is always the latest activity:

```
2026-09-20T12:00:03+03:00  MODIFIED  "/etc/passwd"  hash 0123456789abcdef -> fedcba9876543210  size 1834 -> 1861
```

Hashes are the 64-bit xxHash of the content in hex. Removing a watched source
never deletes its history. There is no automatic retention: trim it yourself
with `fmon clear --path=<path>` or `fmon clear --history-all`.

### Upgrades

The database schema is versioned and migrated forward automatically; the
history is never dropped. If a future release has to rebuild the file
baseline, the next scan does so without reporting every file as new, and adds
this line to its report:

```
NOTICE: baseline rebuilt after upgrade; changes since the previous scan are not reported
```

A database written by a newer fmon than the running binary is refused with a
clear error instead of being modified.

## 🔒 Security notes

fmon is designed to be fast and simple. Know what it does and does not protect
against.

- **xxHash64 is not cryptographic.** It is excellent at detecting accidental
  and ordinary changes (config edits, package updates, corruption). It does not
  resist an attacker who deliberately crafts a colliding file.
- **The stat shortcut can be bypassed.** A file whose size and mtime were
  restored (`touch -r`) after modification is skipped by the first pass. The
  same shortcut can also miss corruption from a failing disk if the metadata
  is not updated. Run `fmon scan --full` periodically (see
  [Full scans (`--full`)](#full-scans---full)) to hash everything and catch this.
- **No tamper protection.** Anyone who can write `fmon.db`, `fmon.toml` or the
  fmon binary can hide changes. For serious use, run fmon as a dedicated
  privileged user, keep the configuration directory private (fmon creates it
  with mode `0700` and files with `0600`), and ship notifications and the log
  to a place the monitored host cannot rewrite.
- **Deleting `fmon.db` or `fmon.toml` is not a silent way to hide changes.**
  If the database file disappears while `fmon.toml` still lists sources, fmon
  recreates it, rebuilds the baseline, and puts a
  `NOTICE: database was missing and has been recreated ...` line into the
  report (log, e-mail, scripts and terminal). If `fmon.toml` disappears but the
  database remains, `scan`, `add` and `rm` refuse to run until you restore the
  file or run `fmon init`, because the file is the source of truth for the
  watched sources. This is a tripwire, not tamper-proofing: an attacker who can
  write these files can still hide changes (see above).
- **Scripts run with fmon's privileges.** Keep them owned by the user running
  fmon and not writable by others. The message is passed as a single argument,
  never through a shell.
- **Secrets.** Prefer `password_file` (mode `0600`) or an environment variable
  over storing an SMTP password in a script.

## 📁 Files and locations

The configuration directory is chosen in this order:

1. `--config-dir <dir>`
2. the `FMON_CONFIG_DIR` environment variable
3. the user configuration directory (`~/.config/fmon` on Linux,
   `~/Library/Application Support/fmon` on macOS, `%AppData%\fmon` on Windows)
4. the home directory reported by the operating system, for the case where cron
   starts fmon without `$HOME`

Nothing is created until you run `fmon init` or `fmon add`; other commands on a
machine where fmon has never been set up only print a hint.

The directory contains:

| File | Purpose |
|---|---|
| `fmon.toml` | Configuration (mode 0600) |
| `fmon.db` | SQLite database: sources, file states, history (plus `-wal` / `-shm` files while in use) |
| `fmon.log` | Reports and operational messages |

## 🔨 Building from source

Requirements: Go 1.24 or newer. For `make all` also the `zip` tool.

```sh
git clone https://github.com/rguziy/fmon.git
cd fmon
go mod tidy          # first time only, to fetch and pin dependencies
make                 # show the list of make targets (same as: make list)
make test            # go vet + go test
make build           # host build into dist/fmon
make all             # cross-compile every target, zip archives + SHA256SUMS in dist/
make linux-armv7     # a single target
make VERSION=1.0.1 all
```

Targets: `linux-amd64`, `linux-arm64`, `linux-armv5`, `linux-armv6`,
`linux-armv7`, `windows-amd64`, `darwin-amd64`, `darwin-arm64`. Every build is
`CGO_ENABLED=0`, uses `-trimpath`, and embeds the version through
`-ldflags "-X github.com/rguziy/fmon/internal/version.Version=..."`.

Dependencies: [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite)
(pure Go SQLite, no CGO),
[cespare/xxhash](https://github.com/cespare/xxhash),
[pelletier/go-toml](https://github.com/pelletier/go-toml) and
[bmatcuk/doublestar](https://github.com/bmatcuk/doublestar). Everything else is
the standard library.

Project layout:

```
cmd/fmon/               CLI entry point, flag parsing, signal handling
internal/config/        fmon.toml loading, validation, atomic saving
internal/db/            SQLite access, forward-only migrations, queries
internal/hasher/        streaming xxHash64
internal/models/        shared data structures
internal/monitor/       commands (init/add/rm/history/clear), scanner, notifications
internal/version/       version string
```

## 📄 License

MIT, see [LICENSE](LICENSE).
