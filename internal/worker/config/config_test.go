package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	platformconfig "github.com/Zeptiny/Syncr/internal/platform/config"
)

func load(t *testing.T, yaml string, env map[string]string) (Config, error) {
	t.Helper()
	path := ""
	if yaml != "" {
		path = filepath.Join(t.TempDir(), "worker.yaml")
		if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Default()
	err := platformconfig.Load(path, func(k string) (string, bool) { v, ok := env[k]; return v, ok }, &cfg)
	return cfg, err
}

func TestLoadFromEnv(t *testing.T) {
	t.Parallel()
	env := map[string]string{"SYNCR_LOG_LEVEL": "debug", "SYNCR_LOG_FORMAT": "json"}
	cfg, err := load(t, "", env)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Log.Level != slog.LevelDebug || cfg.Log.Format != "json" {
		t.Errorf("Log = %+v", cfg.Log)
	}

	bad := Default()
	bad.Log.Format = "xml"
	if err := bad.Validate(); err == nil {
		t.Error("Validate() accepted format xml")
	}
}

func TestDefaultsDenyLocalAccessAndDownload(t *testing.T) {
	t.Parallel()
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(cfg.LocalRoots) != 0 || cfg.Tools.Download || cfg.DataDir != "" || cfg.Tools.Dir != "" {
		t.Errorf("Default() = %+v, want empty roots, no download and platform default directories", cfg)
	}
	if cfg.MaxConcurrentJobs != 2 || cfg.ProgressInterval != time.Second {
		t.Errorf("job defaults = %d, %s", cfg.MaxConcurrentJobs, cfg.ProgressInterval)
	}
}

func TestJobSettings(t *testing.T) {
	t.Parallel()
	cfg, err := load(t, "max_concurrent_jobs: 4\nprogress_interval: 2s\n", nil)
	if err != nil || cfg.MaxConcurrentJobs != 4 || cfg.ProgressInterval != 2*time.Second {
		t.Fatalf("from YAML: %d, %s, %v", cfg.MaxConcurrentJobs, cfg.ProgressInterval, err)
	}
	cfg, err = load(t, "max_concurrent_jobs: 4\n", map[string]string{"SYNCR_MAX_CONCURRENT_JOBS": "8", "SYNCR_PROGRESS_INTERVAL": "500ms"})
	if err != nil || cfg.MaxConcurrentJobs != 8 || cfg.ProgressInterval != 500*time.Millisecond {
		t.Fatalf("from env: %d, %s, %v", cfg.MaxConcurrentJobs, cfg.ProgressInterval, err)
	}
}

func TestLoadYAML(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root1, root2 := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	rclone := filepath.Join(dir, "bin", "rclone")
	yaml := "data_dir: " + quote(dir) + "\n" +
		"local_roots:\n  - " + quote(root1) + "\n  - " + quote(root2) + "\n" +
		"tools:\n  rclone_path: " + quote(rclone) + "\n  download: true\n"
	cfg, err := load(t, yaml, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != dir || !slices.Equal(cfg.LocalRoots, []string{root1, root2}) ||
		cfg.Tools.RclonePath != rclone || !cfg.Tools.Download || cfg.Tools.ResticPath != "" {
		t.Errorf("cfg = %+v", cfg)
	}
}

// quote renders s as a YAML single-quoted scalar (Windows paths contain
// backslashes).
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func TestEnvOverridesFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fromFile, fromEnv1, fromEnv2 := filepath.Join(dir, "file"), filepath.Join(dir, "e1"), filepath.Join(dir, "e2")
	yaml := "local_roots: [" + quote(fromFile) + "]\ntools:\n  download: false\n"
	cfg, err := load(t, yaml, map[string]string{
		"SYNCR_LOCAL_ROOTS":     fromEnv1 + " , " + fromEnv2,
		"SYNCR_DATA_DIR":        dir,
		"SYNCR_TOOLS_DIR":       filepath.Join(dir, "t"),
		"SYNCR_RCLONE_PATH":     filepath.Join(dir, "rclone"),
		"SYNCR_RESTIC_PATH":     filepath.Join(dir, "restic"),
		"SYNCR_TOOLS_DOWNLOAD":  "true",
		"SYNCR_UNRELATED_VALUE": "ignored",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.LocalRoots, []string{fromEnv1, fromEnv2}) {
		t.Errorf("LocalRoots = %q, want the environment list", cfg.LocalRoots)
	}
	if cfg.DataDir != dir || cfg.Tools.Dir != filepath.Join(dir, "t") || !cfg.Tools.Download ||
		cfg.Tools.RclonePath != filepath.Join(dir, "rclone") || cfg.Tools.ResticPath != filepath.Join(dir, "restic") {
		t.Errorf("cfg = %+v", cfg)
	}

	// An empty variable clears the list from the file: deny all.
	cfg, err = load(t, yaml, map[string]string{"SYNCR_LOCAL_ROOTS": ""})
	if err != nil || len(cfg.LocalRoots) != 0 {
		t.Errorf("empty SYNCR_LOCAL_ROOTS: roots = %q, err = %v", cfg.LocalRoots, err)
	}

	if _, err := load(t, "", map[string]string{"SYNCR_TOOLS_DOWNLOAD": "maybe"}); err == nil ||
		!strings.Contains(err.Error(), "SYNCR_TOOLS_DOWNLOAD") {
		t.Errorf("bad boolean: err = %v, want it to name the variable", err)
	}
}

func TestUnknownKeysAreRejected(t *testing.T) {
	t.Parallel()
	for _, yaml := range []string{"local_root: []\n", "tools:\n  rclone: x\n"} {
		if _, err := load(t, yaml, nil); err == nil {
			t.Errorf("config %q accepted, want unknown key error", yaml)
		}
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	abs := t.TempDir()
	tests := []struct {
		name   string
		mutate func(*Config)
		want   []string // substrings, all must appear; none means valid
	}{
		{name: "valid", mutate: func(c *Config) {
			c.DataDir = abs
			c.LocalRoots = []string{abs, filepath.Join(abs, "other")}
			c.Tools = ToolsConfig{Dir: abs, RclonePath: filepath.Join(abs, "rclone"), ResticPath: filepath.Join(abs, "restic")}
		}},
		{name: "relative data dir", mutate: func(c *Config) { c.DataDir = "data" }, want: []string{"data_dir", "not an absolute path"}},
		{name: "relative tools dir", mutate: func(c *Config) { c.Tools.Dir = "tools" }, want: []string{"tools.dir"}},
		{name: "relative rclone", mutate: func(c *Config) { c.Tools.RclonePath = "rclone" }, want: []string{"tools.rclone_path"}},
		{name: "relative restic", mutate: func(c *Config) { c.Tools.ResticPath = "./restic" }, want: []string{"tools.restic_path"}},
		{name: "relative root", mutate: func(c *Config) { c.LocalRoots = []string{abs, "data"} }, want: []string{"local_roots[1]", `"data"`}},
		{name: "empty root", mutate: func(c *Config) { c.LocalRoots = []string{""} }, want: []string{"local_roots[0]", "empty"}},
		{name: "NUL in root", mutate: func(c *Config) { c.LocalRoots = []string{abs + "\x00"} }, want: []string{"NUL"}},
		{name: "duplicate root", mutate: func(c *Config) { c.LocalRoots = []string{abs, filepath.Join(abs, "x", "..")} }, want: []string{"local_roots[1]", "more than once"}},
		{name: "bad log format", mutate: func(c *Config) { c.Log.Format = "xml" }, want: []string{"format"}},
		{name: "no job slots", mutate: func(c *Config) { c.MaxConcurrentJobs = 0 }, want: []string{"max_concurrent_jobs", "outside 1..64"}},
		{name: "too many job slots", mutate: func(c *Config) { c.MaxConcurrentJobs = 65 }, want: []string{"max_concurrent_jobs"}},
		{name: "progress too often", mutate: func(c *Config) { c.ProgressInterval = 10 * time.Millisecond }, want: []string{"progress_interval"}},
		{name: "progress too rare", mutate: func(c *Config) { c.ProgressInterval = time.Hour }, want: []string{"progress_interval"}},
		{name: "everything at once", mutate: func(c *Config) {
			c.DataDir = "d"
			c.LocalRoots = []string{"r"}
			c.Tools.Dir = "t"
		}, want: []string{"data_dir", "local_roots[0]", "tools.dir"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := Default()
			tt.mutate(&cfg)
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
