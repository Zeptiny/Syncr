package link

import "github.com/Zeptiny/Syncr/internal/domain"

// Backups of a folder of a remote (ResticBackup.remote_source,
// FeatureResticRemoteSource): the names both sides of the link and the web
// UI agree on.

// RemoteSourceTagPrefix starts the restic tag the controller adds to every
// snapshot of a backup of a remote: the prefix and the source remote's ID
// (RemoteSourceTag). The web UI names the remote by it, and never offers to
// restore such a snapshot to its original location; retention can select
// the snapshots of one remote by it.
const RemoteSourceTagPrefix = "syncr-remote:"

// RemoteSourceTag returns the tag of the snapshots of a backup that reads a
// folder of the remote id, e.g. "syncr-remote:0f8e...": 49 bytes for a
// remote ID.
func RemoteSourceTag(id domain.RemoteID) string { return RemoteSourceTagPrefix + id.String() }

// Phases of a backup of a remote, as workers report them in Progress.phase
// (and the API in JobProgress.phase). Jobs without phases report "".
const (
	// ProgressPhaseMirror: the worker brings the backup's mirror up to date
	// (rclone's counters).
	ProgressPhaseMirror = "mirror"
	// ProgressPhaseBackup: restic backs up the mirror or the mount
	// (restic's counters).
	ProgressPhaseBackup = "backup"
)

// SourceKeyLen is the length of ResticBackupRemoteSource.source_key: 32
// lower-case hex digits, 128 bits of a hash the controller computes. It
// names the worker's folder for the source (sources/mirror/<key> or
// sources/mount/<key> in its data directory), so it is kept short for
// Windows paths.
const SourceKeyLen = 32

// ValidSourceKey reports whether key is a well-formed source key:
// SourceKeyLen lower-case hex digits. The worker refuses a spec with any
// other (INVALID_SPEC) before it touches its data directory, so a key can
// never name a folder outside the worker's sources directory.
func ValidSourceKey(key string) bool {
	if len(key) != SourceKeyLen {
		return false
	}
	for i := range len(key) {
		if c := key[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
