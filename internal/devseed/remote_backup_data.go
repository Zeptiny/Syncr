package devseed

import (
	"slices"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/link"
)

// Backups of a folder of a remote (ResticBackupDefinition.remote_source):
// the repository they fill, its nightly mirror schedule on nas-01 (which
// can mirror and mount), and a few runs by hand: a mount, an empty folder
// that was allowed to be empty (no snapshot), and a mount on backup-host,
// which cannot mount (tool_unavailable).

// Names of the demo backups of a remote.
const (
	// repoCloud is the repository the backups of Nextcloud fill.
	repoCloud = "Nextcloud photos"
	// sCloudBackup is the nightly mirror backup of Nextcloud's photos.
	sCloudBackup = "Nightly Nextcloud photos backup"
	// cloudFolder is the backed-up folder on the Nextcloud remote.
	cloudFolder = "Photos"
	// cloudMirrorPath is the mirror folder on nas-01 that restic records
	// as the snapshots' path (<data_dir>/sources/mirror/<key>).
	cloudMirrorPath = "/var/lib/syncr/sources/mirror/5d1e0c3b9a7f4e2d8c6b0a1f3e5d7c9b"
)

// cloudRepo is the demo repository of the backups of Nextcloud: its
// snapshots carry the remote's tag (sourceTagged).
func cloudRepo() demoRepo {
	h, m, day := time.Hour, time.Minute, 24*time.Hour
	return demoRepo{owner: ownerAdmin, name: repoCloud, description: "The phone photos on Nextcloud, read through a mirror on nas-01.",
		remote: rMinIO, backend: "s3", path: "restic/nextcloud-photos", age: 16 * day, source: rNextcloud,
		count: 12, every: day, newestAgo: 6*h + 10*m, host: "nas-01", paths: []string{cloudMirrorPath}, tags: []string{"phone"},
		processed: 41 * gib, fileCount: 9120, tree: func() []demoFile { return photoFiles("") }, refreshedAgo: 50 * m}
}

// sourceTagged returns d with its source remote's tag
// (link.RemoteSourceTag) after its own tags, as the controller adds it to
// a backup of a remote; d itself when it has no source or ids lacks it.
func (d demoRepo) sourceTagged(ids map[string]domain.RemoteID) demoRepo {
	id, ok := ids[d.source]
	if d.source == "" || !ok {
		return d
	}
	d.tags = append(slices.Clone(d.tags), link.RemoteSourceTag(id))
	return d
}

// remoteBackupDef backs up folder of the remote named remote into the
// repository repo with method; mirrors leave out caches and thumbnails.
func remoteBackupDef(repo, remote, folder string, method syncrv1.ResticRemoteSourceMethod, allowEmpty bool) defFunc {
	return func(s *Seed) (*syncrv1.JobDefinition, error) {
		id, err := s.repoID(repo)
		if err != nil {
			return nil, err
		}
		rid, err := s.remoteID(remote)
		src := &syncrv1.ResticRemoteSourceDefinition{RemoteId: rid, Path: folder, Method: method, AllowEmpty: allowEmpty,
			Filters: []*syncrv1.FilterRule{
				{Action: syncrv1.FilterAction_FILTER_ACTION_EXCLUDE, Pattern: ".thumbnails/**"},
				{Action: syncrv1.FilterAction_FILTER_ACTION_EXCLUDE, Pattern: "*.tmp"},
			}}
		if method == syncrv1.ResticRemoteSourceMethod_RESTIC_REMOTE_SOURCE_METHOD_MIRROR {
			src.Transfers = proto.Int32(8)
		}
		return &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticBackup{ResticBackup: &syncrv1.ResticBackupDefinition{
			RepositoryId: id, Tags: []string{"phone"}, ExcludeCaches: true, RemoteSource: src,
		}}}, err
	}
}

// mirrorStats are the mirror's counters of the n-th run: the first
// downloads everything, later ones what changed.
func mirrorStats(n int, processed, files int64) *syncrv1.RemoteSourceStats {
	st := &syncrv1.RemoteSourceStats{Method: syncrv1.ResticRemoteSourceMethod_RESTIC_REMOTE_SOURCE_METHOD_MIRROR,
		MirroredBytes: addedBytes(n, processed), MirroredFiles: varied(n+5, 2, 40), DeletedFiles: varied(n+2, 0, 4),
		MirrorElapsed: durationpb.New(time.Duration(varied(n, 40, 300)) * time.Second)}
	if n == 0 {
		st.MirroredBytes, st.MirroredFiles, st.DeletedFiles = processed, files, 0
		st.MirrorElapsed = durationpb.New(47 * time.Minute)
	}
	return st
}

// remoteBackedUp is a successful backup of a remote: restic's result with
// the source's counters.
func remoteBackedUp(key string, n int, run time.Duration, processed, files int64, src *syncrv1.RemoteSourceStats) runOutcome {
	o := backedUp(key, n, run, processed, files)
	o.output.GetResticBackup().RemoteSource = src
	return o
}

// demoRemoteBackupSchedules is the nightly mirror backup of Nextcloud's
// photos on nas-01 (a backup of a remote targets one worker). One night
// the folder was empty (a sync app had moved the photos away) and the run
// failed with source_empty instead of making an empty snapshot.
func demoRemoteBackupSchedules(time.Time) []demoSchedule {
	h, m, d := time.Hour, time.Minute, 24*time.Hour
	cloud := cloudRepo()
	return []demoSchedule{{
		owner: ownerAdmin, name: sCloudBackup, description: "Back up the phone photos on Nextcloud every night, through a mirror on nas-01.",
		cron: "10 3 * * *", timezone: "Europe/Berlin", worker: "nas-01",
		missed: schedules.MissedPolicy{Action: schedules.MissedRunLate, MaxDelay: 6 * h}, overlap: schedules.OverlapSkip,
		job: demoJob{owner: ownerAdmin, def: remoteBackupDef(repoCloud, rNextcloud, cloudFolder,
			syncrv1.ResticRemoteSourceMethod_RESTIC_REMOTE_SOURCE_METHOD_MIRROR, false)},
		age: 14 * d,
		outcome: func(n int, _ time.Time) runOutcome {
			processed, files := cloud.processed+int64(n)*200*mib, cloud.fileCount+int64(n)*24
			if n == 9 {
				// The worker fails it before restic runs: no exit code.
				return failedRun(6*time.Second, link.ErrorCodeName(syncrv1.JobErrorCode_JOB_ERROR_CODE_SOURCE_EMPTY),
					`the folder "Photos" on the remote is empty, so nothing was backed up (an emptied or wrong folder would push real snapshots out of retention); `+
						"allow an empty folder in the backup's options to back it up anyway", -1, jobs.Stats{})
			}
			run := time.Duration(varied(n, 3, 14))*m + time.Duration(varied(n, 5, 55))*time.Second
			if n == 0 {
				run = 58 * m
			}
			return remoteBackedUp(cloud.key(), n, run, processed, files, mirrorStats(n, processed, files))
		},
	}}
}

// demoRemoteBackupJobs are the backups of a remote run by hand: one
// through a mount of the Storage box's scans on nas-01, one of an empty
// Nextcloud folder that was allowed to be empty (it succeeded without a
// snapshot), and a mount on backup-host, which cannot mount (the
// dispatcher failed it with tool_unavailable and the worker's reason).
func demoRemoteBackupJobs() []demoJob {
	h, m, s, d := time.Hour, time.Minute, time.Second, 24*time.Hour
	cloud := cloudRepo()
	mountDef := remoteBackupDef(repoCloud, rStorage, "Scans", syncrv1.ResticRemoteSourceMethod_RESTIC_REMOTE_SOURCE_METHOD_MOUNT, false)
	mounted := backupOutput(cloud.key()+" scans", 0, 2*gib+300*mib, 1408)
	mounted.GetResticBackup().RemoteSource = &syncrv1.RemoteSourceStats{Method: syncrv1.ResticRemoteSourceMethod_RESTIC_REMOTE_SOURCE_METHOD_MOUNT}
	return []demoJob{
		{owner: ownerAdmin, worker: "nas-01", def: mountDef,
			status: domain.JobSucceeded, created: 5*d + 3*h, queue: 2 * s, run: 9*m + 12*s, samples: 10,
			stats: done(2*gib+300*mib, 1408, 0), output: mounted},
		{owner: ownerAdmin, worker: "nas-01",
			def:    remoteBackupDef(repoCloud, rNextcloud, "Inbox", syncrv1.ResticRemoteSourceMethod_RESTIC_REMOTE_SOURCE_METHOD_MIRROR, true),
			status: domain.JobSucceeded, created: 2*d + 7*h, queue: 2 * s, run: 6 * s, samples: 2,
			warnings: []string{`the folder "Inbox" on the remote is empty and restic makes no empty snapshot, so no snapshot was made`},
			output: &syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticBackup{ResticBackup: &syncrv1.ResticBackupOutput{
				RemoteSource: &syncrv1.RemoteSourceStats{Method: syncrv1.ResticRemoteSourceMethod_RESTIC_REMOTE_SOURCE_METHOD_MIRROR,
					DeletedFiles: 3, MirrorElapsed: durationpb.New(2 * s)},
			}}}},
		{owner: ownerAdmin, worker: "backup-host", def: mountDef,
			status: domain.JobFailed, created: 26 * h, queue: 1 * s,
			err: jobs.JobError{Code: jobs.CodeToolUnavailable, Message: `worker "backup-host" cannot mount a remote: /dev/fuse is missing`}},
	}
}
