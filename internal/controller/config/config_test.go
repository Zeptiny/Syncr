package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	platformconfig "github.com/Zeptiny/Syncr/internal/platform/config"
	"github.com/Zeptiny/Syncr/internal/platform/logging"
	"github.com/Zeptiny/Syncr/internal/platform/redact"
)

// testKey is 32 zero bytes in base64 (a test value, not a secret).
const testKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

func TestDefaultIsValid(t *testing.T) {
	t.Parallel()
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"loopback addr", func(c *Config) { c.HTTP.Addr = "127.0.0.1:9000" }, ""},
		{"ephemeral port", func(c *Config) { c.HTTP.Addr = "localhost:0" }, ""},
		{"missing port", func(c *Config) { c.HTTP.Addr = "localhost" }, "http.addr"},
		{"bad port", func(c *Config) { c.HTTP.Addr = ":http-alt" }, "invalid port"},
		{"port out of range", func(c *Config) { c.HTTP.Addr = ":70000" }, "invalid port"},
		{"zero timeout", func(c *Config) { c.HTTP.ShutdownTimeout = 0 }, "shutdown_timeout"},
		{"bad log format", func(c *Config) { c.Log.Format = "xml" }, "format"},
		{"secrets key", func(c *Config) { c.Secrets.Key = testKey }, ""},
		{"rotation", func(c *Config) {
			c.Secrets = Secrets{Key: testKey, KeyID: 2, PreviousKeys: []string{"1:" + testKey}}
		}, ""},
		{"short key", func(c *Config) { c.Secrets.Key = "c2hvcnQ=" }, "secrets.key: key is 5 bytes"},
		{"key not base64", func(c *Config) { c.Secrets.Key = "not base64!" }, "secrets.key: key is not valid base64"},
		{"previous without key", func(c *Config) { c.Secrets.PreviousKeys = []string{"1:" + testKey} }, "needs secrets.key"},
		{"previous key id clash", func(c *Config) {
			c.Secrets = Secrets{Key: testKey, KeyID: 1, PreviousKeys: []string{"1:" + testKey}}
		}, "used twice"},
		{"database url", func(c *Config) { c.Database.URL = "postgres://u:p@db:5432/syncr?sslmode=disable" }, ""},
		{"database postgresql scheme", func(c *Config) { c.Database.URL = "postgresql://db/syncr" }, ""},
		{"database socket host", func(c *Config) { c.Database.URL = "postgres:///syncr?host=/var/run/postgresql" }, ""},
		{"database dsn", func(c *Config) { c.Database.URL = "host=db dbname=syncr" }, ""},
		{"database wrong scheme", func(c *Config) { c.Database.URL = "mysql://u:p@db/x" }, "database.url: scheme"},
		{"database no host", func(c *Config) { c.Database.URL = "postgres://u:p@/syncr" }, "database.url: missing host"},
		{"database garbage", func(c *Config) { c.Database.URL = "not a url" }, "database.url: want"},
		{"database bad url", func(c *Config) { c.Database.URL = "postgres://u:p@db:port/x" }, "database.url: not a valid URL"},
		{"database zero conns", func(c *Config) { c.Database.MaxConns = 0 }, "database.max_conns"},
		{"database zero timeout", func(c *Config) { c.Database.ConnectTimeout = 0 }, "database.connect_timeout"},
		{"previous key malformed", func(c *Config) {
			c.Secrets = Secrets{Key: testKey, KeyID: 1, PreviousKeys: []string{testKey}}
		}, "previous_keys[0]: want <id>:<base64 key>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := Default()
			tt.mutate(&cfg)
			err := cfg.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("Validate() = %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("Validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "controller.yaml")
	content := "http:\n  addr: \"127.0.0.1:9000\"\n  shutdown_timeout: 30s\nlog:\n  level: debug\n  format: json\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"SYNCR_HTTP_ADDR": ":7000", "SYNCR_LOG_FORMAT": "text"}

	cfg := Default()
	err := platformconfig.Load(path, func(k string) (string, bool) { v, ok := env[k]; return v, ok }, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.Addr != ":7000" || cfg.HTTP.ShutdownTimeout != 30*time.Second || cfg.HTTP.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("HTTP = %+v", cfg.HTTP)
	}
	if cfg.Log.Format != logging.FormatText || cfg.Log.Level.String() != "DEBUG" {
		t.Errorf("Log = %+v", cfg.Log)
	}
}

func TestLoadDatabase(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "controller.yaml")
	content := "database:\n  url: postgres://syncr:from-file@db/syncr\n  max_conns: 4\n  connect_timeout: 3s\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"SYNCR_DATABASE_URL": "postgres://syncr:hunter2@db:5432/syncr?sslmode=require", "SYNCR_DATABASE_MAX_CONNS": "25"}

	cfg := Default()
	if cfg.Database.Configured() {
		t.Fatal("the default configuration must not name a database")
	}
	err := platformconfig.Load(path, func(k string) (string, bool) { v, ok := env[k]; return v, ok }, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	db := cfg.Database
	if !db.Configured() || db.URL.Reveal() != env["SYNCR_DATABASE_URL"] || db.MaxConns != 25 || db.ConnectTimeout != 3*time.Second {
		t.Errorf("Database = %+v", db)
	}
	// The URL carries the password: no formatting of the config may show it.
	for _, out := range []string{fmt.Sprintf("%v", cfg), fmt.Sprintf("%+v", cfg), fmt.Sprintf("%#v", cfg)} {
		if strings.Contains(out, "hunter2") {
			t.Errorf("config formatting leaks the database password: %s", out)
		}
	}
}

func TestDatabaseErrorsDoNotEchoTheURL(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"postgres://u:hunter2@db:port/x", "mysql://u:hunter2@db/x", "hunter2 is not a url"} {
		cfg := Default()
		cfg.Database.URL = redact.Secret(raw)
		err := cfg.Validate()
		if err == nil {
			t.Fatalf("%q validated", raw)
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("error echoes the URL: %v", err)
		}
	}
}
