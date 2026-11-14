package config

import (
	"fmt"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/Zeptiny/Syncr/internal/platform/logging"
)

// fileHeader starts every file written by Marshal.
const fileHeader = `# Syncr worker configuration, written by "syncr-worker setup".
# This file contains secrets in clear text: keep it readable only by the
# account that runs the worker. See docs/worker-config.md.
`

// document is the on-disk form of Config. It mirrors Config with plain string
// secrets, because redact.Secret deliberately refuses to encode itself.
type document struct {
	Log               logging.Config `yaml:"log"`
	DataDir           string         `yaml:"data_dir,omitempty"`
	LocalRoots        []string       `yaml:"local_roots,omitempty"`
	Tools             *ToolsConfig   `yaml:"tools,omitempty"`
	MaxConcurrentJobs int            `yaml:"max_concurrent_jobs"`
	ProgressInterval  time.Duration  `yaml:"progress_interval"`
	// Hooks keep their order by name (yaml sorts map keys).
	Hooks map[string]HookConfig `yaml:"hooks,omitempty"`
	// RemoteSources is written only when it differs from the defaults.
	RemoteSources *RemoteSourcesConfig `yaml:"remote_sources,omitempty"`
	Link          linkDocument         `yaml:"link"`
}

type linkDocument struct {
	Mode             LinkMode `yaml:"mode,omitempty"`
	WorkerID         string   `yaml:"worker_id,omitempty"`
	ControllerURL    string   `yaml:"controller_url,omitempty"`
	ControllerCAFile string   `yaml:"controller_ca_file,omitempty"`
	WorkerSecret     string   `yaml:"worker_secret,omitempty"`
	ListenAddr       string   `yaml:"listen_addr,omitempty"`
	AdvertiseURL     string   `yaml:"advertise_url,omitempty"`
	TLSCertFile      string   `yaml:"tls_cert_file,omitempty"`
	TLSKeyFile       string   `yaml:"tls_key_file,omitempty"`
	PairingSecret    string   `yaml:"pairing_secret,omitempty"`
}

// Marshal encodes cfg as a YAML configuration file that Load reads back to an
// equal Config. Secrets are written in clear text: the caller must store the
// result with restrictive permissions (see internal/worker/atomicfile). cfg
// is validated first, so a file that Load would reject is never written.
func Marshal(cfg Config) ([]byte, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config: refusing to write an invalid configuration: %w", err)
	}
	doc := document{
		Log:               cfg.Log,
		DataDir:           cfg.DataDir,
		LocalRoots:        cfg.LocalRoots,
		MaxConcurrentJobs: cfg.MaxConcurrentJobs,
		ProgressInterval:  cfg.ProgressInterval,
		Hooks:             cfg.Hooks,
		Link: linkDocument{
			Mode:             cfg.Link.Mode,
			WorkerID:         cfg.Link.WorkerID,
			ControllerURL:    cfg.Link.ControllerURL,
			ControllerCAFile: cfg.Link.ControllerCAFile,
			WorkerSecret:     cfg.Link.WorkerSecret.Reveal(),
			ListenAddr:       cfg.Link.ListenAddr,
			AdvertiseURL:     cfg.Link.AdvertiseURL,
			TLSCertFile:      cfg.Link.TLSCertFile,
			TLSKeyFile:       cfg.Link.TLSKeyFile,
			PairingSecret:    cfg.Link.PairingSecret.Reveal(),
		},
	}
	if cfg.Tools != (ToolsConfig{}) {
		doc.Tools = &cfg.Tools
	}
	if cfg.RemoteSources != DefaultRemoteSources() {
		doc.RemoteSources = &cfg.RemoteSources
	}
	body, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("config: encode: %w", err)
	}
	return append([]byte(fileHeader), body...), nil
}
