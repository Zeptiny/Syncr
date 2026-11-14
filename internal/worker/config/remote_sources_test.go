package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRemoteSourcesDefaults(t *testing.T) {
	t.Parallel()
	want := RemoteSourcesConfig{Mirror: true, MirrorRetention: 720 * time.Hour, Mount: MountAuto, MountTimeout: 30 * time.Second}
	if got := Default().RemoteSources; got != want {
		t.Errorf("Default().RemoteSources = %+v, want %+v", got, want)
	}
	if got := DefaultRemoteSources(); got != want {
		t.Errorf("DefaultRemoteSources() = %+v, want %+v", got, want)
	}
	// A file without the block keeps the defaults.
	cfg, err := load(t, "max_concurrent_jobs: 3\n", nil)
	if err != nil || cfg.RemoteSources != want {
		t.Errorf("without remote_sources: %+v, %v", cfg.RemoteSources, err)
	}
}

func TestRemoteSourcesFromYAMLAndEnv(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		yaml string
		env  map[string]string
		want RemoteSourcesConfig
	}{
		{
			name: "every key",
			yaml: "remote_sources:\n  mirror: false\n  mirror_retention: 48h\n  mount: off\n  mount_timeout: 1m\n",
			want: RemoteSourcesConfig{Mirror: false, MirrorRetention: 48 * time.Hour, Mount: MountOff, MountTimeout: time.Minute},
		},
		{
			// The keys a file leaves out keep their defaults.
			name: "one key",
			yaml: "remote_sources:\n  mount: \"off\"\n",
			want: RemoteSourcesConfig{Mirror: true, MirrorRetention: DefaultMirrorRetention, Mount: MountOff, MountTimeout: DefaultMountTimeout},
		},
		{
			name: "environment over the file",
			yaml: "remote_sources:\n  mirror: false\n  mount: off\n",
			env: map[string]string{
				"SYNCR_REMOTE_SOURCES_MIRROR":           "true",
				"SYNCR_REMOTE_SOURCES_MIRROR_RETENTION": "2h",
				"SYNCR_REMOTE_SOURCES_MOUNT":            "auto",
				"SYNCR_REMOTE_SOURCES_MOUNT_TIMEOUT":    "5s",
			},
			want: RemoteSourcesConfig{Mirror: true, MirrorRetention: 2 * time.Hour, Mount: MountAuto, MountTimeout: 5 * time.Second},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := load(t, tt.yaml, tt.env)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.RemoteSources != tt.want {
				t.Errorf("RemoteSources = %+v, want %+v", cfg.RemoteSources, tt.want)
			}
		})
	}

	for _, yaml := range []string{"remote_sources:\n  mirrors: true\n", "remote_sources:\n  mount_dir: /mnt\n"} {
		if _, err := load(t, yaml, nil); err == nil {
			t.Errorf("config %q accepted, want an unknown key error", yaml)
		}
	}
	if _, err := load(t, "", map[string]string{"SYNCR_REMOTE_SOURCES_MOUNT_TIMEOUT": "soon"}); err == nil ||
		!strings.Contains(err.Error(), "SYNCR_REMOTE_SOURCES_MOUNT_TIMEOUT") {
		t.Errorf("bad duration: err = %v, want it to name the variable", err)
	}
}

func TestRemoteSourcesValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*RemoteSourcesConfig)
		want   []string // substrings, all must appear; none means valid
	}{
		{name: "defaults", mutate: func(*RemoteSourcesConfig) {}},
		{name: "everything off", mutate: func(r *RemoteSourcesConfig) { r.Mirror, r.Mount = false, MountOff }},
		{name: "shortest retention", mutate: func(r *RemoteSourcesConfig) { r.MirrorRetention = time.Hour }},
		{name: "long retention", mutate: func(r *RemoteSourcesConfig) { r.MirrorRetention = 10 * 365 * 24 * time.Hour }},
		{name: "shortest mount timeout", mutate: func(r *RemoteSourcesConfig) { r.MountTimeout = 5 * time.Second }},
		{name: "longest mount timeout", mutate: func(r *RemoteSourcesConfig) { r.MountTimeout = 5 * time.Minute }},
		{name: "retention too short", mutate: func(r *RemoteSourcesConfig) { r.MirrorRetention = time.Hour - time.Second },
			want: []string{"remote_sources.mirror_retention", "shorter than 1h0m0s"}},
		{name: "no retention", mutate: func(r *RemoteSourcesConfig) { r.MirrorRetention = 0 }, want: []string{"remote_sources.mirror_retention"}},
		{name: "empty mount mode", mutate: func(r *RemoteSourcesConfig) { r.Mount = "" }, want: []string{"remote_sources.mount", `want "auto" or "off"`}},
		{name: "unknown mount mode", mutate: func(r *RemoteSourcesConfig) { r.Mount = "on" }, want: []string{"remote_sources.mount", `"on"`}},
		{name: "mount mode in capitals", mutate: func(r *RemoteSourcesConfig) { r.Mount = "AUTO" }, want: []string{"remote_sources.mount"}},
		{name: "mount timeout too short", mutate: func(r *RemoteSourcesConfig) { r.MountTimeout = 5*time.Second - time.Millisecond },
			want: []string{"remote_sources.mount_timeout", "outside 5s..5m0s"}},
		{name: "mount timeout too long", mutate: func(r *RemoteSourcesConfig) { r.MountTimeout = 5*time.Minute + time.Second },
			want: []string{"remote_sources.mount_timeout"}},
		{name: "everything at once", mutate: func(r *RemoteSourcesConfig) { r.MirrorRetention, r.Mount, r.MountTimeout = 0, "maybe", 0 },
			want: []string{"remote_sources.mirror_retention", "remote_sources.mount:", "remote_sources.mount_timeout"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := Default()
			tt.mutate(&cfg.RemoteSources)
			err := cfg.Validate()
			if len(tt.want) == 0 {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Validate() = nil, want an error")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err, w)
				}
			}
		})
	}
}

// Setup writes the configuration with Marshal: remote_sources settings an
// operator changed must survive a rewrite, and the defaults add no block.
func TestMarshalKeepsRemoteSources(t *testing.T) {
	t.Parallel()
	body, err := Marshal(Default())
	if err != nil || strings.Contains(string(body), "remote_sources") {
		t.Errorf("Marshal(Default()) = %v\n%s\nwant no remote_sources key", err, body)
	}

	cfg := Default()
	cfg.RemoteSources = RemoteSourcesConfig{Mirror: false, MirrorRetention: 72 * time.Hour, Mount: MountOff, MountTimeout: 90 * time.Second}
	body, err = Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "worker.yaml")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := load2(path)
	if err != nil {
		t.Fatalf("Load(Marshal(cfg)): %v\n%s", err, body)
	}
	if got.RemoteSources != cfg.RemoteSources {
		t.Errorf("round trip changed remote_sources\n got %+v\nwant %+v\nfile:\n%s", got.RemoteSources, cfg.RemoteSources, body)
	}

	cfg.RemoteSources.Mount = "sometimes"
	if _, err := Marshal(cfg); err == nil {
		t.Error("Marshal accepted an invalid mount mode")
	}
}
