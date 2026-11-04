package config

import (
	"log/slog"
	"testing"

	platformconfig "github.com/Zeptiny/Syncr/internal/platform/config"
)

func TestLoadFromEnv(t *testing.T) {
	t.Parallel()
	env := map[string]string{"SYNCR_LOG_LEVEL": "debug", "SYNCR_LOG_FORMAT": "json"}
	cfg := Default()
	if err := platformconfig.Load("", func(k string) (string, bool) { v, ok := env[k]; return v, ok }, &cfg); err != nil {
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
