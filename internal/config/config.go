// Package config loads, validates and atomically writes fmon.toml and resolves
// the locations of fmon's files.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/pelletier/go-toml/v2"
)

// Detail levels for notification messages.
const (
	DetailSummary = "summary"
	DetailFull    = "full"
)

// SMTP transport security modes.
const (
	SecurityStartTLS = "starttls"
	SecurityTLS      = "tls"
	SecurityNone     = "none"
)

// ConfigDirEnv overrides the configuration directory.
const ConfigDirEnv = "FMON_CONFIG_DIR"

// Paths holds the locations of all files fmon reads and writes.
type Paths struct {
	Dir    string // configuration directory
	Config string // fmon.toml
	DB     string // fmon.db
	Log    string // fmon.log
}

// ResolvePaths determines the configuration directory using this precedence:
//
//  1. the explicit flagDir argument (--config-dir),
//  2. the FMON_CONFIG_DIR environment variable,
//  3. os.UserConfigDir()/fmon (~/.config/fmon on Linux),
//  4. the home directory reported by os/user, because cron may run with an
//     empty $HOME.
//
// The directory is not created here; see EnsureDir.
func ResolvePaths(flagDir string) (Paths, error) {
	dir := flagDir
	if dir == "" {
		dir = os.Getenv(ConfigDirEnv)
	}
	if dir == "" {
		if base, err := os.UserConfigDir(); err == nil && base != "" {
			dir = filepath.Join(base, "fmon")
		}
	}
	if dir == "" {
		u, err := user.Current()
		if err != nil || u.HomeDir == "" {
			return Paths{}, errors.New("cannot determine the configuration directory: set --config-dir or " + ConfigDirEnv)
		}
		dir = filepath.Join(u.HomeDir, ".config", "fmon")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Paths{}, fmt.Errorf("resolve configuration directory: %w", err)
	}
	return Paths{
		Dir:    abs,
		Config: filepath.Join(abs, "fmon.toml"),
		DB:     filepath.Join(abs, "fmon.db"),
		Log:    filepath.Join(abs, "fmon.log"),
	}, nil
}

// EnsureDir creates the configuration directory (mode 0700) if needed.
func (p Paths) EnsureDir() error {
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return fmt.Errorf("create configuration directory: %w", err)
	}
	return nil
}

// LogConfig configures the local log sink.
type LogConfig struct {
	Detail string `toml:"detail"`
}

// SMTPConfig configures the e-mail sink.
type SMTPConfig struct {
	Enabled      bool     `toml:"enabled"`
	Host         string   `toml:"host"`
	Port         int      `toml:"port"`
	Security     string   `toml:"security"`
	Username     string   `toml:"username"`
	PasswordEnv  string   `toml:"password_env"`
	PasswordFile string   `toml:"password_file"`
	From         string   `toml:"from"`
	To           []string `toml:"to"`
	Detail       string   `toml:"detail"`
}

// ScriptConfig configures one notification script.
type ScriptConfig struct {
	Path            string `toml:"path"`
	Detail          string `toml:"detail"`
	Timeout         string `toml:"timeout"`
	MaxMessageBytes int    `toml:"max_message_bytes"`
}

// TimeoutDuration returns the parsed script timeout. Validate guarantees that
// it parses.
func (s ScriptConfig) TimeoutDuration() time.Duration {
	d, err := time.ParseDuration(s.Timeout)
	if err != nil || d <= 0 {
		return 30 * time.Second
	}
	return d
}

// Config is the content of fmon.toml.
type Config struct {
	Sources []string       `toml:"sources"`
	Exclude []string       `toml:"exclude"`
	Log     LogConfig      `toml:"log"`
	SMTP    SMTPConfig     `toml:"smtp"`
	Scripts []ScriptConfig `toml:"script,omitempty"` // omitted when empty so a hand-added [[script]] table stays valid
}

// defaultMaxMessageBytes is the default cap for the message passed to a script
// as its first argument. Windows limits a whole command line to about 32 KiB.
func defaultMaxMessageBytes() int {
	if runtime.GOOS == "windows" {
		return 30000
	}
	return 100000
}

// Default returns a configuration with all defaults applied.
func Default() *Config {
	return &Config{
		Sources: []string{},
		Exclude: []string{"*.tmp", "*.swp", "**/.git/**"},
		Log:     LogConfig{Detail: DetailFull},
		SMTP: SMTPConfig{
			Enabled:     false,
			Host:        "smtp.example.com",
			Port:        587,
			Security:    SecurityStartTLS,
			PasswordEnv: "FMON_SMTP_PASSWORD",
			From:        "fmon@example.com",
			To:          []string{"admin@example.com"},
			Detail:      DetailFull,
		},
	}
}

// applyScriptDefaults fills in unset per-script settings.
func (c *Config) applyScriptDefaults() {
	for i := range c.Scripts {
		s := &c.Scripts[i]
		if s.Detail == "" {
			s.Detail = DetailSummary
		}
		if s.Timeout == "" {
			s.Timeout = "30s"
		}
		if s.MaxMessageBytes == 0 {
			s.MaxMessageBytes = defaultMaxMessageBytes()
		}
	}
}

// Load reads and validates the configuration file. A missing file yields the
// default configuration (nothing is written to disk).
func Load(path string) (*Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields() // catch typos in setting names
	if err := dec.Decode(cfg); err != nil {
		var strict *toml.StrictMissingError
		var derr *toml.DecodeError
		switch {
		case errors.As(err, &strict):
			return nil, fmt.Errorf("%s: unknown setting(s):\n%s", path, strict.String())
		case errors.As(err, &derr):
			return nil, fmt.Errorf("%s: %s", path, derr.String())
		}
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	cfg.applyScriptDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

func validDetail(v string) bool { return v == DetailSummary || v == DetailFull }

// Validate checks the configuration for errors.
func (c *Config) Validate() error {
	if !validDetail(c.Log.Detail) {
		return fmt.Errorf("[log] detail must be %q or %q, got %q", DetailSummary, DetailFull, c.Log.Detail)
	}
	for _, p := range c.Exclude {
		if !doublestar.ValidatePattern(p) {
			return fmt.Errorf("exclude: invalid pattern %q", p)
		}
	}

	if !validDetail(c.SMTP.Detail) {
		return fmt.Errorf("[smtp] detail must be %q or %q, got %q", DetailSummary, DetailFull, c.SMTP.Detail)
	}
	if c.SMTP.Enabled {
		s := c.SMTP
		switch s.Security {
		case SecurityStartTLS, SecurityTLS, SecurityNone:
		default:
			return fmt.Errorf("[smtp] security must be %q, %q or %q, got %q",
				SecurityStartTLS, SecurityTLS, SecurityNone, s.Security)
		}
		if strings.TrimSpace(s.Host) == "" {
			return errors.New("[smtp] host is required when smtp is enabled")
		}
		if s.Port < 1 || s.Port > 65535 {
			return fmt.Errorf("[smtp] port %d is out of range", s.Port)
		}
		if strings.TrimSpace(s.From) == "" || len(s.To) == 0 {
			return errors.New("[smtp] from and to are required when smtp is enabled")
		}
		if s.Username != "" && s.Security == SecurityNone {
			return errors.New("[smtp] authentication requires security = \"starttls\" or \"tls\"")
		}
	}

	for i, s := range c.Scripts {
		if strings.TrimSpace(s.Path) == "" {
			return fmt.Errorf("[[script]] #%d: path is required", i+1)
		}
		if !validDetail(s.Detail) {
			return fmt.Errorf("[[script]] %s: detail must be %q or %q, got %q", s.Path, DetailSummary, DetailFull, s.Detail)
		}
		if d, err := time.ParseDuration(s.Timeout); err != nil || d <= 0 {
			return fmt.Errorf("[[script]] %s: invalid timeout %q (examples: \"30s\", \"2m\")", s.Path, s.Timeout)
		}
		if s.MaxMessageBytes < 200 {
			return fmt.Errorf("[[script]] %s: max_message_bytes must be at least 200", s.Path)
		}
	}
	return nil
}

// Password returns the SMTP password from password_file (which must not be
// group/world accessible on Unix) or, failing that, from the environment
// variable named by password_env. An empty result means "no password".
func (s SMTPConfig) Password() (string, error) {
	if s.PasswordFile != "" {
		if runtime.GOOS != "windows" {
			info, err := os.Stat(s.PasswordFile)
			if err != nil {
				return "", fmt.Errorf("smtp password_file: %w", err)
			}
			if info.Mode().Perm()&0o077 != 0 {
				return "", fmt.Errorf("smtp password_file %s must not be accessible by group/others (chmod 600)", s.PasswordFile)
			}
		}
		b, err := os.ReadFile(s.PasswordFile)
		if err != nil {
			return "", fmt.Errorf("smtp password_file: %w", err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	if s.PasswordEnv != "" {
		return os.Getenv(s.PasswordEnv), nil
	}
	return "", nil
}

// ---- source list helpers ------------------------------------------------------

// HasSource reports whether path is listed in sources.
func (c *Config) HasSource(path string) bool {
	for _, s := range c.Sources {
		if s == path {
			return true
		}
	}
	return false
}

// AddSource appends path to sources unless it is already present.
func (c *Config) AddSource(path string) {
	if !c.HasSource(path) {
		c.Sources = append(c.Sources, path)
	}
}

// RemoveSource removes path from sources.
func (c *Config) RemoveSource(path string) {
	out := c.Sources[:0]
	for _, s := range c.Sources {
		if s != path {
			out = append(out, s)
		}
	}
	c.Sources = out
}

// ClearSources removes all sources but keeps every other setting.
func (c *Config) ClearSources() { c.Sources = []string{} }

// ---- writing ------------------------------------------------------------------

const fileHeader = `# fmon configuration
#
# This file is regenerated by fmon whenever the source list changes
# (fmon add / rm / init). Settings are preserved, but hand-written comments
# are NOT: keep notes elsewhere.
#
# sources : watched files and folders. Manage with "fmon add" / "fmon rm".
# exclude : global patterns applied to files found inside watched folders.
#           A pattern without "/" matches the file or directory base name
#           ("*.tmp"); a pattern with "/" is matched against the full absolute
#           path using doublestar globbing ("**/node_modules/**").
# [log], [smtp], [[script]]: notification sinks. "detail" is "summary"
#           (counts only) or "full" (itemized list of every change).
#
`

const fileFooter = `
# Example notification script (repeat the table for more scripts). The report
# is passed as the first argument ($1); details are also exported through
# FMON_* environment variables.
#
# [[script]]
# path = "/usr/local/bin/notify.sh"
# detail = "summary"          # "summary" | "full"
# timeout = "30s"
# max_message_bytes = 100000  # message is truncated at a line boundary
`

// Save writes the configuration atomically (temporary file in the same
// directory, fsync, rename) with mode 0600.
func (c *Config) Save(path string) error {
	// Make sure empty lists are written as [] rather than omitted.
	if c.Sources == nil {
		c.Sources = []string{}
	}
	if c.Exclude == nil {
		c.Exclude = []string{}
	}
	if c.SMTP.To == nil {
		c.SMTP.To = []string{}
	}

	body, err := toml.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode configuration: %w", err)
	}
	data := append([]byte(fileHeader), body...)
	data = append(data, fileFooter...)
	return writeFileAtomic(path, data, 0o600)
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".fmon-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(perm); err != nil && runtime.GOOS != "windows" {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}
