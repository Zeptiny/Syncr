package config

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

type testLog struct {
	Level slog.Level `yaml:"level" env:"SYNCR_TEST_LOG_LEVEL"`
}

type testSecret string

type testConfig struct {
	Addr     string        `yaml:"addr" env:"SYNCR_TEST_ADDR"`
	Debug    bool          `yaml:"debug" env:"SYNCR_TEST_DEBUG"`
	Workers  int           `yaml:"workers" env:"SYNCR_TEST_WORKERS"`
	Limit    uint32        `yaml:"limit" env:"SYNCR_TEST_LIMIT"`
	Ratio    float64       `yaml:"ratio" env:"SYNCR_TEST_RATIO"`
	Timeout  time.Duration `yaml:"timeout" env:"SYNCR_TEST_TIMEOUT"`
	Roots    []string      `yaml:"roots" env:"SYNCR_TEST_ROOTS"`
	Token    testSecret    `yaml:"token" env:"SYNCR_TEST_TOKEN"`
	Log      testLog       `yaml:"log"`
	Untagged string        `yaml:"untagged"`
	internal string
}

func (c *testConfig) Validate() error {
	if c.Addr == "" {
		return errors.New("addr is required")
	}
	return nil
}

func defaults() *testConfig {
	return &testConfig{Addr: ":8080", Workers: 2, Timeout: time.Second, Log: testLog{Level: slog.LevelInfo}, internal: "kept"}
}

func env(vars map[string]string) LookupEnv {
	return func(key string) (string, bool) {
		v, ok := vars[key]
		return v, ok
	}
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadPrecedence(t *testing.T) {
	t.Parallel()
	path := writeFile(t, `
addr: ":9000"
workers: 4
timeout: 30s
roots: ["/data", "/srv"]
log:
  level: debug
untagged: from-file
`)
	cfg := defaults()
	err := Load(path, env(map[string]string{
		"SYNCR_TEST_WORKERS":   "8",
		"SYNCR_TEST_DEBUG":     "true",
		"SYNCR_TEST_LIMIT":     "42",
		"SYNCR_TEST_RATIO":     "0.5",
		"SYNCR_TEST_ROOTS":     " /a, /b ,,",
		"SYNCR_TEST_TOKEN":     "s3cret",
		"SYNCR_TEST_LOG_LEVEL": "warn",
		"UNRELATED":            "ignored",
	}), cfg)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Addr != ":9000" {
		t.Errorf("Addr = %q, want file value", cfg.Addr)
	}
	if cfg.Workers != 8 {
		t.Errorf("Workers = %d, want env value 8", cfg.Workers)
	}
	if !cfg.Debug || cfg.Limit != 42 || cfg.Ratio != 0.5 || cfg.Token != "s3cret" {
		t.Errorf("env scalars not applied: %+v", cfg)
	}
	if cfg.Timeout != 30*time.Second {
		t.Errorf("Timeout = %v, want file value 30s", cfg.Timeout)
	}
	if !slices.Equal(cfg.Roots, []string{"/a", "/b"}) {
		t.Errorf("Roots = %q, want env list", cfg.Roots)
	}
	if cfg.Log.Level != slog.LevelWarn {
		t.Errorf("Log.Level = %v, want env value warn (nested struct, TextUnmarshaler)", cfg.Log.Level)
	}
	if cfg.Untagged != "from-file" {
		t.Errorf("Untagged = %q", cfg.Untagged)
	}
}

func TestLoadDefaultsOnly(t *testing.T) {
	t.Parallel()
	cfg := defaults()
	if err := Load("", nil, cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != ":8080" || cfg.Workers != 2 || cfg.Timeout != time.Second || cfg.internal != "kept" {
		t.Errorf("defaults changed: %+v", cfg)
	}
}

func TestLoadEmptyFileKeepsDefaults(t *testing.T) {
	t.Parallel()
	cfg := defaults()
	if err := Load(writeFile(t, ""), env(nil), cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != ":8080" || cfg.Workers != 2 {
		t.Errorf("defaults changed: %+v", cfg)
	}
}

func TestLoadErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		file    string
		env     map[string]string
		wantErr string
	}{
		{name: "unknown yaml key", file: "listen_addr: x\n", wantErr: "field listen_addr not found"},
		{name: "bad yaml type", file: "workers: many\n", wantErr: "parse"},
		{name: "bad duration env", env: map[string]string{"SYNCR_TEST_TIMEOUT": "soon"}, wantErr: "SYNCR_TEST_TIMEOUT: invalid duration"},
		{name: "bad bool env", env: map[string]string{"SYNCR_TEST_DEBUG": "yes please"}, wantErr: "SYNCR_TEST_DEBUG: invalid boolean"},
		{name: "bad level env", env: map[string]string{"SYNCR_TEST_LOG_LEVEL": "loud"}, wantErr: "SYNCR_TEST_LOG_LEVEL: invalid value"},
		{name: "int overflow env", env: map[string]string{"SYNCR_TEST_LIMIT": "99999999999"}, wantErr: "SYNCR_TEST_LIMIT"},
		{name: "validation", env: map[string]string{"SYNCR_TEST_ADDR": ""}, wantErr: "addr is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := ""
			if tt.file != "" {
				path = writeFile(t, tt.file)
			}
			err := Load(path, env(tt.env), defaults())
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadReportsAllEnvErrors(t *testing.T) {
	t.Parallel()
	err := Load("", env(map[string]string{"SYNCR_TEST_DEBUG": "x", "SYNCR_TEST_WORKERS": "y"}), defaults())
	if err == nil || !strings.Contains(err.Error(), "SYNCR_TEST_DEBUG") || !strings.Contains(err.Error(), "SYNCR_TEST_WORKERS") {
		t.Fatalf("err = %v, want both variables reported", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	t.Parallel()
	err := Load(filepath.Join(t.TempDir(), "missing.yaml"), nil, defaults())
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want os.ErrNotExist", err)
	}
}

type badTag struct {
	Name string `env:"NAME"`
}

func (badTag) Validate() error { return nil }

type badType struct {
	Ch chan int `env:"SYNCR_TEST_CH"`
}

func (badType) Validate() error { return nil }

func TestLoadRejectsBadDeclarations(t *testing.T) {
	t.Parallel()
	if err := Load("", env(nil), &badTag{}); err == nil || !strings.Contains(err.Error(), "must start with SYNCR_") {
		t.Errorf("bad tag: err = %v", err)
	}
	if err := Load("", env(map[string]string{"SYNCR_TEST_CH": "1"}), &badType{}); err == nil || !strings.Contains(err.Error(), "unsupported field type") {
		t.Errorf("bad type: err = %v", err)
	}
	if err := Load("", nil, badTag{}); err == nil || !strings.Contains(err.Error(), "pointer to a struct") {
		t.Errorf("non-pointer: err = %v", err)
	}
}
