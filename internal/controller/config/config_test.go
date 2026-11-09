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
		{"no read/write timeouts", func(c *Config) { c.HTTP.ReadTimeout, c.HTTP.WriteTimeout = 0, 0 }, ""},
		{"negative idle timeout", func(c *Config) { c.HTTP.IdleTimeout = -time.Second }, "idle_timeout must not be negative"},
		{"tls pair", func(c *Config) { c.HTTP.TLSCertFile, c.HTTP.TLSKeyFile = "/c.pem", "/k.pem" }, ""},
		{"tls cert without key", func(c *Config) { c.HTTP.TLSCertFile = "/c.pem" }, "set together"},
		{"tls key without cert", func(c *Config) { c.HTTP.TLSKeyFile = "/k.pem" }, "set together"},
		{"proxy header", func(c *Config) {
			c.HTTP.ClientIPHeader, c.HTTP.TrustedProxies = "X-Forwarded-For", []string{"10.0.0.0/8", "192.0.2.1", "::1"}
		}, ""},
		{"proxy header without proxies", func(c *Config) { c.HTTP.ClientIPHeader = "X-Real-IP" }, "needs http.trusted_proxies"},
		{"proxies without header", func(c *Config) { c.HTTP.TrustedProxies = []string{"10.0.0.1"} }, "needs http.client_ip_header"},
		{"bad proxy", func(c *Config) {
			c.HTTP.ClientIPHeader, c.HTTP.TrustedProxies = "X-Real-IP", []string{"10.0.0.0/33"}
		}, "trusted_proxies[0]"},
		{"bad header name", func(c *Config) {
			c.HTTP.ClientIPHeader, c.HTTP.TrustedProxies = "X Real IP", []string{"10.0.0.1"}
		}, "not a valid header name"},
		{"previous key malformed", func(c *Config) {
			c.Secrets = Secrets{Key: testKey, KeyID: 1, PreviousKeys: []string{testKey}}
		}, "previous_keys[0]: want <id>:<base64 key>"},
		{"public url", func(c *Config) { c.HTTP.PublicURL = "https://syncr.example.com" }, ""},
		{"public url with path", func(c *Config) { c.HTTP.PublicURL = "https://example.com/syncr/" }, ""},
		{"public url http", func(c *Config) { c.HTTP.PublicURL = "http://127.0.0.1:8080" }, ""},
		{"public url relative", func(c *Config) { c.HTTP.PublicURL = "syncr.example.com" }, "http.public_url"},
		{"public url scheme", func(c *Config) { c.HTTP.PublicURL = "ftp://syncr.example.com" }, "http.public_url"},
		{"public url no host", func(c *Config) { c.HTTP.PublicURL = "https://" }, "has no host"},
		{"public url query", func(c *Config) { c.HTTP.PublicURL = "https://syncr.example.com/?a=b" }, "query"},
		{"public url credentials", func(c *Config) { c.HTTP.PublicURL = "https://u:p@syncr.example.com" }, "credentials"},
		{"cookie always", func(c *Config) { c.Auth.CookieSecure = CookieSecureAlways }, ""},
		{"cookie never", func(c *Config) { c.Auth.CookieSecure = CookieSecureNever }, ""},
		{"cookie unknown", func(c *Config) { c.Auth.CookieSecure = "sometimes" }, "auth.cookie_secure"},
		{"cookie empty", func(c *Config) { c.Auth.CookieSecure = "" }, "auth.cookie_secure"},
		{"short idle timeout", func(c *Config) { c.Auth.SessionIdleTimeout = time.Second }, "auth.session_idle_timeout"},
		{"short max age", func(c *Config) { c.Auth.SessionMaxAge = 59 * time.Second }, "auth.session_max_age"},
		{"setup code", func(c *Config) { c.Auth.SetupCode = "ABCDE-FGHIJ-KLMNO-PQRS2" }, ""},
		{"setup code 16", func(c *Config) { c.Auth.SetupCode = "abcd-efgh-ijkl-mnop" }, ""},
		{"setup code short", func(c *Config) { c.Auth.SetupCode = "ABCDE-FGHIJ-KLMNO" }, "at least 16 characters"},
		{"setup code control", func(c *Config) { c.Auth.SetupCode = "ABCDEFGHIJKLMNOP\x00" }, "control characters"},
		{"download base url", func(c *Config) { c.Workers.DownloadBaseURL = "https://mirror.example.com/syncr" }, ""},
		{"download base url empty", func(c *Config) { c.Workers.DownloadBaseURL = "" }, "workers.download_base_url"},
		{"download base url scheme", func(c *Config) { c.Workers.DownloadBaseURL = "file:///srv" }, "workers.download_base_url"},
		{"lost after", func(c *Config) { c.Workers.LostAfter = 24 * time.Hour }, ""},
		{"lost after minimum", func(c *Config) { c.Workers.LostAfter = MinLostAfter }, ""},
		{"lost after too short", func(c *Config) { c.Workers.LostAfter = time.Minute }, "workers.lost_after must be at least 5m0s"},
		{"lost after unset", func(c *Config) { c.Workers.LostAfter = 0 }, "workers.lost_after"},
		{"seed with memory store", func(c *Config) { c.Dev.Seed, c.Dev.MemoryStore = true, true }, ""},
		{"seed without memory store", func(c *Config) { c.Dev.Seed = true }, "dev.seed needs dev.memory_store"},
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

func TestCheckServe(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr []string
	}{
		{"database and key", func(c *Config) { c.Secrets.Key, c.Database.URL = testKey, "postgres://db/syncr" }, nil},
		{"memory store and key", func(c *Config) { c.Secrets.Key, c.Dev.MemoryStore = testKey, true }, nil},
		{"nothing", func(*Config) {}, []string{"secrets.key is required", "database.url is required", "dev.memory_store"}},
		{"no key", func(c *Config) { c.Database.URL = "postgres://db/syncr" }, []string{"secrets.key is required"}},
		{"both stores", func(c *Config) {
			c.Secrets.Key, c.Database.URL, c.Dev.MemoryStore = testKey, "postgres://db/syncr", true
		}, []string{"choose one"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := Default()
			tt.mutate(&cfg)
			err := cfg.CheckServe()
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("CheckServe() = %v", err)
				}
				return
			}
			for _, want := range tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("CheckServe() = %v, want error containing %q", err, want)
				}
			}
		})
	}
}

func TestSetupCodeErrorsDoNotEchoIt(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.Auth.SetupCode = "SHORT-hunter2"
	err := cfg.Validate()
	if err == nil {
		t.Fatal("a short setup code validated")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error echoes the setup code: %v", err)
	}
}

func TestLoadAuthAndWorkers(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "controller.yaml")
	content := "http:\n  public_url: https://from-file.example.com\nauth:\n  cookie_secure: always\n  session_idle_timeout: 12h\n" +
		"workers:\n  download_base_url: https://mirror.example.com/syncr\n  lost_after: 3h\ndev:\n  memory_store: true\n  seed: true\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	const code = "ABCDE-FGHIJ-KLMNO-PQRS2"
	env := map[string]string{
		"SYNCR_PUBLIC_URL": "https://syncr.example.com", "SYNCR_SETUP_CODE": code,
		"SYNCR_AUTH_SESSION_MAX_AGE": "48h", "SYNCR_DEV_SEED": "false", "SYNCR_WORKERS_LOST_AFTER": "6h",
	}
	cfg := Default()
	if err := platformconfig.Load(path, func(k string) (string, bool) { v, ok := env[k]; return v, ok }, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.PublicURL != "https://syncr.example.com" {
		t.Errorf("public url = %q", cfg.HTTP.PublicURL)
	}
	want := Auth{CookieSecure: CookieSecureAlways, SessionIdleTimeout: 12 * time.Hour, SessionMaxAge: 48 * time.Hour, SetupCode: code}
	if cfg.Auth != want {
		t.Errorf("Auth = %+v", cfg.Auth)
	}
	if cfg.Workers.DownloadBaseURL != "https://mirror.example.com/syncr" || cfg.Workers.LostAfter != 6*time.Hour || cfg.Dev.Seed || !cfg.Dev.MemoryStore {
		t.Errorf("Workers = %+v, Dev = %+v", cfg.Workers, cfg.Dev)
	}
	if got := fmt.Sprintf("%v %+v %#v", cfg, cfg, cfg); strings.Contains(got, code) {
		t.Errorf("config formatting leaks the setup code: %s", got)
	}
}

func TestDefaultAuth(t *testing.T) {
	t.Parallel()
	cfg := Default()
	if cfg.Auth.CookieSecure != CookieSecureAuto || cfg.Auth.SessionIdleTimeout != 7*24*time.Hour || cfg.Auth.SessionMaxAge != 30*24*time.Hour || !cfg.Auth.SetupCode.IsZero() {
		t.Errorf("Auth = %+v", cfg.Auth)
	}
	if cfg.Workers.DownloadBaseURL != DefaultDownloadBaseURL || cfg.Workers.LostAfter != time.Hour || cfg.HTTP.PublicURL != "" || cfg.Dev.Seed {
		t.Errorf("Workers = %+v, HTTP.PublicURL = %q, Dev = %+v", cfg.Workers, cfg.HTTP.PublicURL, cfg.Dev)
	}
}

func TestParseTrustedProxies(t *testing.T) {
	t.Parallel()
	got, err := ParseTrustedProxies([]string{"10.1.2.3/8", " 192.0.2.7 ", "::ffff:192.0.2.8", "2001:db8::/32"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.0.0.0/8", "192.0.2.7/32", "192.0.2.8/32", "2001:db8::/32"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Errorf("[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}
