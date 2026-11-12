package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	platformconfig "github.com/Zeptiny/Syncr/internal/platform/config"
)

func TestDefaultJobs(t *testing.T) {
	t.Parallel()
	cfg := Default()
	if cfg.Jobs.DefaultMaxRuntime != 0 || cfg.Jobs.SampleRetention != 30*24*time.Hour || cfg.Jobs.KeepSamples() {
		t.Errorf("Jobs = %+v, want no default time limit and 30 days of samples", cfg.Jobs)
	}
}

func TestValidateJobs(t *testing.T) {
	t.Parallel()
	day := 24 * time.Hour
	tests := []struct {
		name    string
		jobs    Jobs
		wantErr string
	}{
		{"defaults", defaultJobs(), ""},
		{"no limit, keep samples", Jobs{}, ""},
		{"shortest limit", Jobs{DefaultMaxRuntime: time.Minute, SampleRetention: time.Hour}, ""},
		{"longest limit", Jobs{DefaultMaxRuntime: 366 * day}, ""},
		{"six hours", Jobs{DefaultMaxRuntime: 6 * time.Hour, SampleRetention: 90 * day}, ""},
		{"limit too short", Jobs{DefaultMaxRuntime: 59 * time.Second}, "jobs.default_max_runtime"},
		{"limit too long", Jobs{DefaultMaxRuntime: 366*day + time.Second}, "jobs.default_max_runtime"},
		{"negative limit", Jobs{DefaultMaxRuntime: -time.Minute}, "jobs.default_max_runtime"},
		{"retention too short", Jobs{SampleRetention: 59 * time.Minute}, "jobs.sample_retention must be 0 (keep every sample) or at least 1h0m0s"},
		{"negative retention", Jobs{SampleRetention: -time.Hour}, "jobs.sample_retention"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := Default()
			cfg.Jobs = tt.jobs
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

func TestLoadJobs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		yaml      string
		env       map[string]string
		want      Jobs
		wantKeep  bool
		wantError string
	}{
		{name: "file", yaml: "jobs:\n  default_max_runtime: 6h\n  sample_retention: 168h\n",
			want: Jobs{DefaultMaxRuntime: 6 * time.Hour, SampleRetention: 168 * time.Hour}},
		{name: "environment wins", yaml: "jobs:\n  default_max_runtime: 6h\n",
			env:  map[string]string{"SYNCR_JOBS_DEFAULT_MAX_RUNTIME": "8784h", "SYNCR_JOBS_SAMPLE_RETENTION": "0"},
			want: Jobs{DefaultMaxRuntime: 366 * 24 * time.Hour}, wantKeep: true},
		{name: "unknown key", yaml: "jobs:\n  max_runtime: 6h\n", wantError: "max_runtime"},
		{name: "not a duration", yaml: "", env: map[string]string{"SYNCR_JOBS_SAMPLE_RETENTION": "30d"}, wantError: "SYNCR_JOBS_SAMPLE_RETENTION"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "controller.yaml")
			if err := os.WriteFile(path, []byte(tt.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := Default()
			err := platformconfig.Load(path, func(k string) (string, bool) { v, ok := tt.env[k]; return v, ok }, &cfg)
			if err == nil {
				err = cfg.Validate()
			}
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("Load = %v, want an error mentioning %q", err, tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Jobs != tt.want || cfg.Jobs.KeepSamples() != tt.wantKeep {
				t.Errorf("Jobs = %+v (keep %v), want %+v (keep %v)", cfg.Jobs, cfg.Jobs.KeepSamples(), tt.want, tt.wantKeep)
			}
		})
	}
}
