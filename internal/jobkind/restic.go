package jobkind

import (
	"github.com/Zeptiny/Syncr/internal/domain"
)

// ResticRepoKeyPrefix starts the concurrency key of a restic repository:
// every job on the repository holds ResticRepoKey(id), so the jobs of one
// repository run one at a time, oldest first.
const ResticRepoKeyPrefix = "restic-repo:"

// ResticRepoKey returns the concurrency key of jobs on repository id
// ("restic-repo:<uuid>").
func ResticRepoKey(id domain.ResticRepoID) string { return ResticRepoKeyPrefix + id.String() }

// ResticKinds returns the restic job kinds: restic_backup, restic_forget,
// restic_prune, restic_check, restic_restore and restic_delete_data (M5
// design §2.2). Each refers to one of the owner's repositories
// (Refs.RepositoryID), holds its ResticRepoKey (Keyed) and requires the
// worker feature link.FeatureResticV2 (restic_delete_data also
// link.FeatureResticDelete). restic_restore and restic_delete_data are
// Unschedulable; restic_delete_data is SystemOnly.
//
// m5: Track C implements this (internal/jobkind/restic.go and
// restic_delete.go); until then there are no restic kinds.
func ResticKinds() []Kind { return nil }
