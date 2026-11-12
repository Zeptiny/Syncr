package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestNew(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		cfg   Config
		check func(t *testing.T, out string)
	}{
		{
			name: "json",
			cfg:  Config{Level: slog.LevelInfo, Format: FormatJSON},
			check: func(t *testing.T, out string) {
				t.Helper()
				var rec map[string]any
				if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rec); err != nil {
					t.Fatalf("not one JSON object: %v: %s", err, out)
				}
				if rec["msg"] != "hello" || rec["password"] != "[redacted]" || rec["job_id"] != "j1" {
					t.Errorf("unexpected record: %v", rec)
				}
			},
		},
		{
			name: "text",
			cfg:  Config{Level: slog.LevelInfo, Format: FormatText},
			check: func(t *testing.T, out string) {
				t.Helper()
				for _, want := range []string{"msg=hello", "password=[redacted]", "job_id=j1"} {
					if !strings.Contains(out, want) {
						t.Errorf("output %q does not contain %q", out, want)
					}
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			logger, err := New(&buf, tt.cfg)
			if err != nil {
				t.Fatal(err)
			}
			logger.Debug("filtered")
			logger.Info("hello", "password", "hunter2", "job_id", "j1")
			out := buf.String()
			if strings.Contains(out, "filtered") {
				t.Errorf("debug line logged at info level: %s", out)
			}
			if strings.Contains(out, "hunter2") {
				t.Errorf("secret leaked: %s", out)
			}
			tt.check(t, out)
		})
	}
}

func TestConfigValidate(t *testing.T) {
	t.Parallel()
	if err := DefaultConfig().Validate(); err != nil {
		t.Errorf("default config invalid: %v", err)
	}
	_, err := New(&bytes.Buffer{}, Config{Format: "xml"})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("New with format xml: err = %v, want ErrInvalidConfig", err)
	}
}
