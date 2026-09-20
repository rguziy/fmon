package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResolvePathsPrecedence(t *testing.T) {
	flagDir := filepath.Join(t.TempDir(), "flag")
	envDir := filepath.Join(t.TempDir(), "env")

	t.Setenv(ConfigDirEnv, envDir)
	p, err := ResolvePaths(flagDir)
	if err != nil || p.Dir != flagDir {
		t.Fatalf("flag must win: %+v, %v", p, err)
	}
	p, err = ResolvePaths("")
	if err != nil || p.Dir != envDir {
		t.Fatalf("env must be used: %+v, %v", p, err)
	}
	if p.Config != filepath.Join(envDir, "fmon.toml") || p.DB != filepath.Join(envDir, "fmon.db") || p.Log != filepath.Join(envDir, "fmon.log") {
		t.Errorf("unexpected file names: %+v", p)
	}
}

func TestLoadMissingFileGivesDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "fmon.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Log.Detail != DetailFull || cfg.SMTP.Enabled || len(cfg.Sources) != 0 || len(cfg.Exclude) == 0 {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fmon.toml")
	cfg := Default()
	cfg.AddSource("/etc")
	cfg.AddSource("/etc") // duplicate ignored
	cfg.AddSource("/opt/app/config.yml")
	cfg.Scripts = []ScriptConfig{{Path: "/usr/local/bin/notify.sh"}}
	cfg.applyScriptDefaults()
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}

	if runtime.GOOS != "windows" {
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v, want 0600", info.Mode().Perm())
		}
	}
	raw, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(raw), "# fmon configuration") {
		t.Errorf("missing header comment")
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v\n%s", err, raw)
	}
	if len(got.Sources) != 2 || got.Sources[0] != "/etc" {
		t.Errorf("sources = %v", got.Sources)
	}
	if len(got.Scripts) != 1 || got.Scripts[0].Detail != DetailSummary || got.Scripts[0].Timeout != "30s" {
		t.Errorf("script defaults not applied: %+v", got.Scripts)
	}

	got.RemoveSource("/etc")
	got.ClearSources()
	if len(got.Sources) != 0 {
		t.Errorf("ClearSources left %v", got.Sources)
	}
	// Other settings survive a save with no sources.
	if err := got.Save(path); err != nil {
		t.Fatal(err)
	}
	again, err := Load(path)
	if err != nil || len(again.Scripts) != 1 || len(again.Exclude) == 0 {
		t.Fatalf("settings lost: %+v, %v", again, err)
	}
}

func TestLoadRejectsBadConfig(t *testing.T) {
	tests := []struct {
		name, toml, wantErr string
	}{
		{"unknown key", "typo_setting = 1\n", "unknown"},
		{"bad detail", "[log]\ndetail = \"loud\"\n", "detail"},
		{"bad exclude", "exclude = [\"[unclosed\"]\n", "invalid pattern"},
		{"smtp enabled without host", "[smtp]\nenabled = true\nhost = \"\"\n", "host is required"},
		{"smtp auth without tls", "[smtp]\nenabled = true\nsecurity = \"none\"\nusername = \"u\"\n", "authentication requires"},
		{"script without path", "[[script]]\ndetail = \"full\"\n", "path is required"},
		{"script bad timeout", "[[script]]\npath = \"/x\"\ntimeout = \"soon\"\n", "invalid timeout"},
		{"syntax error", "sources = [\n", "fmon.toml"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fmon.toml")
			if err := os.WriteFile(path, []byte(tc.toml), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestSMTPPassword(t *testing.T) {
	t.Setenv("FMON_TEST_PW", "from-env")
	s := SMTPConfig{PasswordEnv: "FMON_TEST_PW"}
	if pw, err := s.Password(); err != nil || pw != "from-env" {
		t.Fatalf("env password = %q, %v", pw, err)
	}

	if runtime.GOOS == "windows" {
		return
	}
	f := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(f, []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s = SMTPConfig{PasswordFile: f, PasswordEnv: "FMON_TEST_PW"}
	if _, err := s.Password(); err == nil {
		t.Fatal("world-readable password file must be rejected")
	}
	if err := os.Chmod(f, 0o600); err != nil {
		t.Fatal(err)
	}
	if pw, err := s.Password(); err != nil || pw != "secret" {
		t.Fatalf("file password = %q, %v", pw, err)
	}
}

// The generated file must stay valid when the user appends a [[script]] table
// by hand, as the example in the file footer suggests.
func TestSavedFileAcceptsAppendedScriptTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fmon.toml")
	if err := Default().Save(path); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "script = ") {
		t.Fatalf("empty script list must not be written as a value:\n%s", raw)
	}
	raw = append(raw, []byte("\n[[script]]\npath = '/usr/local/bin/notify.sh'\n")...)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil || len(cfg.Scripts) != 1 || cfg.Scripts[0].Detail != DetailSummary {
		t.Fatalf("Load = %+v, %v", cfg, err)
	}
}
