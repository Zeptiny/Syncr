package config

import (
	"errors"
	"fmt"
	"time"
)

// RemoteSourcesConfig configures backups of a folder of a remote
// (restic_backup with a remote source): whether the worker keeps mirrors of
// such folders in its data directory, how long it keeps an unused one, and
// whether it mounts them. The worker reports what it can do in its Hello
// (remote_sources), so the controller sends it only backups it can run.
type RemoteSourcesConfig struct {
	// Mirror allows the mirror method: the worker keeps a copy of each
	// source folder under <data_dir>/sources/mirror, brought up to date
	// with rclone sync before each backup. A copy is as large as its
	// folder (after the backup's filters). On by default; false makes the
	// worker refuse mirrored backups (the controller then sends it none).
	Mirror bool `yaml:"mirror" env:"SYNCR_REMOTE_SOURCES_MIRROR"`

	// MirrorRetention is how long a mirror may stay unused before the
	// worker deletes it (checked at startup and once a day): at least
	// MinMirrorRetention, DefaultMirrorRetention (30 days) by default. A
	// mirror in use is never deleted.
	MirrorRetention time.Duration `yaml:"mirror_retention" env:"SYNCR_REMOTE_SOURCES_MIRROR_RETENTION"`

	// Mount allows the mount method ("auto", the default) or turns it off
	// ("off"). With "auto" the worker checks at startup whether it can
	// mount (FUSE 3 on Linux, WinFsp on Windows, the rights to use them,
	// and a trial mount) and reports why when it cannot.
	Mount MountMode `yaml:"mount" env:"SYNCR_REMOTE_SOURCES_MOUNT"`

	// MountTimeout bounds the wait for a mount to be ready (rclone mount
	// started, the folder mounted): MinMountTimeout to MaxMountTimeout,
	// DefaultMountTimeout by default. A mount that is not ready by then
	// fails its job.
	MountTimeout time.Duration `yaml:"mount_timeout" env:"SYNCR_REMOTE_SOURCES_MOUNT_TIMEOUT"`
}

// MountMode says whether the worker may mount remotes
// (RemoteSourcesConfig.Mount).
type MountMode string

// The mount modes.
const (
	// MountAuto: mount when the machine allows it, as the worker finds out
	// at startup.
	MountAuto MountMode = "auto"
	// MountOff: never mount; the worker reports the mount as unavailable
	// without checking the machine.
	MountOff MountMode = "off"
)

// Defaults and bounds of the remote_sources settings.
const (
	DefaultMirrorRetention = 30 * 24 * time.Hour
	MinMirrorRetention     = time.Hour
	DefaultMountTimeout    = 30 * time.Second
	MinMountTimeout        = 5 * time.Second
	MaxMountTimeout        = 5 * time.Minute
)

// DefaultRemoteSources returns the remote_sources settings used when the
// configuration names none: mirrors on and kept for 30 days unused, mounts
// where the machine allows them, ready within 30 seconds.
func DefaultRemoteSources() RemoteSourcesConfig {
	return RemoteSourcesConfig{
		Mirror:          true,
		MirrorRetention: DefaultMirrorRetention,
		Mount:           MountAuto,
		MountTimeout:    DefaultMountTimeout,
	}
}

// validate reports every invalid remote_sources setting.
func (r RemoteSourcesConfig) validate() error {
	var errs []error
	if r.MirrorRetention < MinMirrorRetention {
		errs = append(errs, fmt.Errorf("remote_sources.mirror_retention: %s is shorter than %s", r.MirrorRetention, MinMirrorRetention))
	}
	switch r.Mount {
	case MountAuto, MountOff:
	default:
		errs = append(errs, fmt.Errorf("remote_sources.mount: %q is not valid (want %q or %q)", r.Mount, MountAuto, MountOff))
	}
	if r.MountTimeout < MinMountTimeout || r.MountTimeout > MaxMountTimeout {
		errs = append(errs, fmt.Errorf("remote_sources.mount_timeout: %s is outside %s..%s", r.MountTimeout, MinMountTimeout, MaxMountTimeout))
	}
	return errors.Join(errs...)
}
