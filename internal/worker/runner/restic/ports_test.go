package restic

import (
	"github.com/Zeptiny/Syncr/internal/worker/localfs"
	"github.com/Zeptiny/Syncr/internal/worker/rcloneenv"
)

// The real implementations satisfy the runners' ports. They are checked here
// (and wired in cmd/syncr-worker) rather than in the implementing packages,
// which must not import the runners.
var (
	_ RemoteEnv  = (*rcloneenv.Builder)(nil)
	_ PathPolicy = (*localfs.Policy)(nil)
)
