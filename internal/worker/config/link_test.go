package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Zeptiny/Syncr/internal/platform/logging"
)

const (
	testWorkerID = "0b6f8f0e-52a5-4a3f-9a5d-2f2a7c9f1e11"
	testPairing  = "pairing-secret-0123456789-abcdefghij"
	testSecret   = "wsec_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
)

func abs(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(t.TempDir(), name)
}

func activeLink(t *testing.T) LinkConfig {
	t.Helper()
	return LinkConfig{
		Mode:          LinkActive,
		ControllerURL: "https://syncr.example.com",
		WorkerID:      testWorkerID,
		WorkerSecret:  testSecret,
	}
}

func passiveLink(t *testing.T) LinkConfig {
	t.Helper()
	return LinkConfig{
		Mode:          LinkPassive,
		ListenAddr:    ":7443",
		AdvertiseURL:  "https://worker.example.com:7443",
		TLSCertFile:   abs(t, "cert.pem"),
		TLSKeyFile:    abs(t, "key.pem"),
		PairingSecret: testPairing,
	}
}

func TestLinkValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  func(t *testing.T) LinkConfig
		wantErr []string // all must appear; empty means valid
	}{
		{name: "unset is valid", mutate: func(*testing.T) LinkConfig { return LinkConfig{} }},
		{name: "active", mutate: activeLink},
		{name: "active with CA file", mutate: func(t *testing.T) LinkConfig {
			l := activeLink(t)
			l.ControllerCAFile = abs(t, "ca.pem")
			return l
		}},
		{name: "active http loopback", mutate: func(t *testing.T) LinkConfig {
			l := activeLink(t)
			l.ControllerURL = "http://127.0.0.1:8080"
			return l
		}},
		{name: "active http localhost", mutate: func(t *testing.T) LinkConfig {
			l := activeLink(t)
			l.ControllerURL = "http://localhost:8080/syncr"
			return l
		}},
		{name: "active with path prefix", mutate: func(t *testing.T) LinkConfig {
			l := activeLink(t)
			l.ControllerURL = "https://example.com/syncr"
			return l
		}},
		{name: "passive", mutate: passiveLink},
		{name: "passive with worker id", mutate: func(t *testing.T) LinkConfig {
			l := passiveLink(t)
			l.WorkerID = testWorkerID
			return l
		}},
		{name: "passive listen on host", mutate: func(t *testing.T) LinkConfig {
			l := passiveLink(t)
			l.ListenAddr = "10.0.0.5:7443"
			return l
		}},

		{name: "unknown mode", mutate: func(*testing.T) LinkConfig { return LinkConfig{Mode: "both"} },
			wantErr: []string{"link.mode", `"both"`}},
		{name: "fields without mode", mutate: func(*testing.T) LinkConfig { return LinkConfig{ControllerURL: "https://x"} },
			wantErr: []string{"link.mode", "not set"}},
		{name: "secret without mode", mutate: func(*testing.T) LinkConfig { return LinkConfig{PairingSecret: testPairing} },
			wantErr: []string{"link.mode"}},

		{name: "active empty", mutate: func(*testing.T) LinkConfig { return LinkConfig{Mode: LinkActive} },
			wantErr: []string{"link.controller_url: required", "link.worker_id: required", "link.worker_secret: required"}},
		{name: "active http remote", mutate: func(t *testing.T) LinkConfig {
			l := activeLink(t)
			l.ControllerURL = "http://syncr.example.com"
			return l
		}, wantErr: []string{"plain http", "localhost"}},
		{name: "active url with credentials", mutate: func(t *testing.T) LinkConfig {
			l := activeLink(t)
			l.ControllerURL = "https://user:pw@syncr.example.com"
			return l
		}, wantErr: []string{"credentials"}},
		{name: "active url with query", mutate: func(t *testing.T) LinkConfig {
			l := activeLink(t)
			l.ControllerURL = "https://syncr.example.com/?a=b"
			return l
		}, wantErr: []string{"query"}},
		{name: "active url without scheme", mutate: func(t *testing.T) LinkConfig {
			l := activeLink(t)
			l.ControllerURL = "syncr.example.com"
			return l
		}, wantErr: []string{"link.controller_url"}},
		{name: "active bad worker id", mutate: func(t *testing.T) LinkConfig {
			l := activeLink(t)
			l.WorkerID = "not-a-uuid"
			return l
		}, wantErr: []string{"link.worker_id", "UUID"}},
		{name: "active relative CA file", mutate: func(t *testing.T) LinkConfig {
			l := activeLink(t)
			l.ControllerCAFile = "ca.pem"
			return l
		}, wantErr: []string{"link.controller_ca_file", "absolute"}},
		{name: "active with passive fields", mutate: func(t *testing.T) LinkConfig {
			l := activeLink(t)
			l.ListenAddr = ":7443"
			l.PairingSecret = testPairing
			return l
		}, wantErr: []string{"link.listen_addr, link.pairing_secret", "not valid in active mode"}},

		{name: "passive empty", mutate: func(*testing.T) LinkConfig { return LinkConfig{Mode: LinkPassive} },
			wantErr: []string{"link.listen_addr", "link.advertise_url", "link.tls_cert_file", "link.tls_key_file", "link.pairing_secret"}},
		{name: "passive listen without port", mutate: func(t *testing.T) LinkConfig {
			l := passiveLink(t)
			l.ListenAddr = "0.0.0.0"
			return l
		}, wantErr: []string{"link.listen_addr", "host:port"}},
		{name: "passive listen port range", mutate: func(t *testing.T) LinkConfig {
			l := passiveLink(t)
			l.ListenAddr = ":70000"
			return l
		}, wantErr: []string{"invalid port"}},
		{name: "passive advertise http", mutate: func(t *testing.T) LinkConfig {
			l := passiveLink(t)
			l.AdvertiseURL = "http://localhost:7443"
			return l
		}, wantErr: []string{"link.advertise_url", "https"}},
		{name: "passive short pairing secret", mutate: func(t *testing.T) LinkConfig {
			l := passiveLink(t)
			l.PairingSecret = "short"
			return l
		}, wantErr: []string{"link.pairing_secret", "32 to 512"}},
		{name: "passive pairing secret with space", mutate: func(t *testing.T) LinkConfig {
			l := passiveLink(t)
			l.PairingSecret = testPairing + " x"
			return l
		}, wantErr: []string{"printable ASCII without spaces"}},
		{name: "passive pairing secret non-ASCII", mutate: func(t *testing.T) LinkConfig {
			l := passiveLink(t)
			l.PairingSecret = testPairing + "é"
			return l
		}, wantErr: []string{"printable ASCII"}},
		{name: "passive relative cert", mutate: func(t *testing.T) LinkConfig {
			l := passiveLink(t)
			l.TLSCertFile = "cert.pem"
			return l
		}, wantErr: []string{"link.tls_cert_file", "absolute"}},
		{name: "passive with active fields", mutate: func(t *testing.T) LinkConfig {
			l := passiveLink(t)
			l.ControllerURL = "https://syncr.example.com"
			l.WorkerSecret = testSecret
			return l
		}, wantErr: []string{"link.controller_url, link.worker_secret", "not valid in passive mode"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			l := tt.mutate(t)
			err := l.Validate()
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want an error containing %q", tt.wantErr)
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestLinkWrongSecretsNeverEchoed(t *testing.T) {
	t.Parallel()
	l := passiveLink(t)
	l.PairingSecret = "bad secret with spaces and more than enough characters"
	err := l.Validate()
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "bad secret") {
		t.Errorf("validation error leaks the secret: %v", err)
	}
}

func TestLinkFromYAMLAndEnv(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")

	t.Run("active file", func(t *testing.T) {
		t.Parallel()
		cfg, err := load(t, fmt.Sprintf(`
link:
  mode: active
  controller_url: https://syncr.example.com
  controller_ca_file: %q
  worker_id: %s
  worker_secret: %s
`, filepath.Join(dir, "ca.pem"), testWorkerID, testSecret), nil)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Link.Mode != LinkActive || cfg.Link.WorkerSecret.Reveal() != testSecret || cfg.Link.ControllerURL != "https://syncr.example.com" {
			t.Errorf("link = %+v", cfg.Link)
		}
	})

	t.Run("env overrides file", func(t *testing.T) {
		t.Parallel()
		cfg, err := load(t, fmt.Sprintf(`
link:
  mode: active
  controller_url: https://old.example.com
  worker_id: %s
  worker_secret: old-secret
`, testWorkerID), map[string]string{
			"SYNCR_CONTROLLER_URL": "https://new.example.com",
			"SYNCR_WORKER_SECRET":  "from-env",
		})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Link.ControllerURL != "https://new.example.com" || cfg.Link.WorkerSecret.Reveal() != "from-env" {
			t.Errorf("link = %+v", cfg.Link)
		}
	})

	t.Run("passive from env only", func(t *testing.T) {
		t.Parallel()
		cfg, err := load(t, "", map[string]string{
			"SYNCR_LINK_MODE":      "passive",
			"SYNCR_LISTEN_ADDR":    ":7443",
			"SYNCR_ADVERTISE_URL":  "https://w.example.com:7443",
			"SYNCR_TLS_CERT_FILE":  cert,
			"SYNCR_TLS_KEY_FILE":   key,
			"SYNCR_PAIRING_SECRET": testPairing,
		})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Link.Mode != LinkPassive || cfg.Link.PairingSecret.Reveal() != testPairing || cfg.Link.TLSKeyFile != key {
			t.Errorf("link = %+v", cfg.Link)
		}
	})

	t.Run("invalid reported with the platform prefix", func(t *testing.T) {
		t.Parallel()
		_, err := load(t, "", map[string]string{"SYNCR_LINK_MODE": "active"})
		if err == nil || !strings.Contains(err.Error(), "link.controller_url") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("unknown link key rejected", func(t *testing.T) {
		t.Parallel()
		_, err := load(t, "link:\n  mode: active\n  insecure_skip_verify: true\n", nil)
		if err == nil || !strings.Contains(err.Error(), "insecure_skip_verify") {
			t.Fatalf("err = %v, want the unknown key rejected (there is no skip-verify option)", err)
		}
	})
}

func TestConfigRedactsSecretsWhenLogged(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.Link = activeLink(t)
	cfg.Link.PairingSecret = "" // not valid in active mode; irrelevant for printing

	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		if out := fmt.Sprintf(format, cfg); strings.Contains(out, testSecret) {
			t.Errorf("fmt %s leaks the worker secret: %s", format, out)
		}
	}
	for _, f := range []logging.Format{logging.FormatText, logging.FormatJSON} {
		var buf bytes.Buffer
		log, err := logging.New(&buf, logging.Config{Level: slog.LevelInfo, Format: f})
		if err != nil {
			t.Fatal(err)
		}
		log.Info("loaded", "config", cfg, "link", cfg.Link, "secret", cfg.Link.WorkerSecret, "pairing_secret", testPairing)
		out := buf.String()
		if strings.Contains(out, testSecret) || strings.Contains(out, testPairing) {
			t.Errorf("%s log leaks a secret: %s", f, out)
		}
		if !strings.Contains(out, "redacted") {
			t.Errorf("%s log has no placeholder: %s", f, out)
		}
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tests := map[string]LinkConfig{
		"active": {
			Mode: LinkActive, ControllerURL: "https://syncr.example.com", ControllerCAFile: filepath.Join(dir, "ca.pem"),
			WorkerID: testWorkerID, WorkerSecret: testSecret,
		},
		"passive": {
			Mode: LinkPassive, ListenAddr: ":7443", AdvertiseURL: "https://w.example.com:7443",
			TLSCertFile: filepath.Join(dir, "cert.pem"), TLSKeyFile: filepath.Join(dir, "key.pem"),
			PairingSecret: testPairing, WorkerID: testWorkerID,
		},
		"unset": {},
	}
	for name, link := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := Default()
			cfg.DataDir = filepath.Join(dir, "data")
			cfg.LocalRoots = []string{filepath.Join(dir, "a"), filepath.Join(dir, "b")}
			cfg.Tools.Download = true
			cfg.Link = link
			body, err := Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(string(body), "# Syncr worker configuration") {
				t.Errorf("missing header: %s", body)
			}
			if strings.Contains(string(body), "[redacted]") {
				t.Fatalf("secrets were redacted in the file:\n%s", body)
			}
			path := filepath.Join(t.TempDir(), "worker.yaml")
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := load2(path)
			if err != nil {
				t.Fatalf("Load(Marshal(cfg)): %v\n%s", err, body)
			}
			if !reflect.DeepEqual(got, cfg) {
				t.Errorf("round trip changed the config\n got %+v\nwant %+v\nfile:\n%s", got, cfg, body)
			}
			if got.Link != cfg.Link {
				t.Errorf("link round trip: got %+v want %+v", got.Link, cfg.Link)
			}
		})
	}
}

func TestMarshalRefusesInvalid(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.Link = LinkConfig{Mode: LinkActive}
	if _, err := Marshal(cfg); err == nil {
		t.Fatal("Marshal accepted an invalid configuration")
	}
}
