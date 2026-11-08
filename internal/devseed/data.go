package devseed

import (
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/link"
)

// Owners of demo objects.
const (
	ownerAdmin = 0
	ownerAlice = 1
)

// Size units.
const (
	kib = int64(1) << 10
	mib = kib << 10
	gib = mib << 10
)

// demoWorker is a seeded worker.
type demoWorker struct {
	name   string
	mode   domain.WorkerMode
	status domain.WorkerStatus
	// lastSeen is how long ago it last called (online: now; offline:
	// hours ago). Pending workers never did.
	lastSeen    time.Duration
	url         string // passive only
	fingerprint string // passive only
	info        workers.Info
	labels      []string
	// removedAgo removes the worker this long before the seed's start, as
	// part of the history (its schedule is paused when it next runs).
	removedAgo time.Duration
}

// passivePairingSecret is the pairing secret of the demo passive worker (a
// demo value; nothing listens at its URL).
//
//nolint:gosec // G101: a public demo value
const passivePairingSecret = "devseed-pairing-secret-0123456789abcdef"

func tools() []workers.Tool {
	return []workers.Tool{{Name: "rclone", Version: "v1.75.2"}, {Name: "restic", Version: "0.19.1"}}
}

func demoWorkers() []demoWorker {
	return []demoWorker{
		{
			name: "nas-01", mode: domain.WorkerActive, status: domain.WorkerOnline,
			info: workers.Info{
				Version: "2.0.0-dev", OS: "linux", Arch: "amd64", Hostname: "nas-01", Tools: tools(), JobKinds: link.Kinds(),
				MaxConcurrentJobs: 2, LocalRoots: []string{"/srv", "/mnt"},
			},
			labels: []string{"nas", "linux"},
		},
		{
			name: "office-pc", mode: domain.WorkerActive, status: domain.WorkerOffline, lastSeen: 3 * time.Hour,
			info: workers.Info{
				Version: "2.0.0-dev", OS: "windows", Arch: "amd64", Hostname: "OFFICE-PC", Tools: tools(), JobKinds: link.Kinds(),
				MaxConcurrentJobs: 1, LocalRoots: []string{`C:\Users\ada\Documents`, `D:\Backups`},
			},
			labels: []string{"desktop", "windows"},
		},
		{
			name: "backup-host", mode: domain.WorkerPassive, status: domain.WorkerOnline,
			url: "https://backup-host.lan:7443", fingerprint: "5f3a9c1e7b2d4f60a8c3e9b1d7f2a4c6e8b0d2f4a6c8e0b2d4f6a8c0e2b4d6f8",
			info: workers.Info{
				Version: "2.0.0-dev", OS: "linux", Arch: "arm64", Hostname: "backup-host", Tools: tools(), JobKinds: link.Kinds(),
				MaxConcurrentJobs: 4, LocalRoots: []string{"/backup"},
			},
			labels: []string{"offsite"},
		},
		{
			name: "laptop", mode: domain.WorkerActive, status: domain.WorkerPending,
			info: workers.Info{OS: "linux", Arch: "amd64"},
		},
		{
			name: wOldNAS, mode: domain.WorkerActive, status: domain.WorkerOffline, lastSeen: oldNASRemoved,
			info: workers.Info{
				Version: "2.0.0-dev", OS: "linux", Arch: "amd64", Hostname: "old-nas", Tools: tools(), JobKinds: link.Kinds(),
				MaxConcurrentJobs: 1, LocalRoots: []string{"/volume1"},
			},
			labels: []string{"nas", "linux"}, removedAgo: oldNASRemoved,
		},
	}
}

// wOldNAS is the demo worker that was removed (its schedule is paused).
const wOldNAS = "old-nas"

// oldNASRemoved is how long before the seed old-nas was removed.
const oldNASRemoved = 6*24*time.Hour + 9*time.Hour

// demoRemote is a seeded remote; members name earlier remotes of a union.
type demoRemote struct {
	owner   int
	input   remotes.Input
	members []string
}

// Demo remote names used by the jobs.
const (
	rMinIO     = "MinIO archive"
	rStorage   = "Storage box"
	rPhotos    = "Photos"
	rB2        = "B2 offsite"
	rNextcloud = "Nextcloud"
	rDisk1     = "Disk 1"
	rDisk2     = "Disk 2"
	rPool      = "Pooled disks"
	rAliceDocs = "Alice's documents"
	rAliceB2   = "Alice's B2"
)

// demoRemotes are the remotes, in creation order (union members first).
// The credentials are demo values for hosts that do not exist.
//
//nolint:gosec // G101: public demo credentials
func demoRemotes() []demoRemote {
	return []demoRemote{
		{owner: ownerAdmin, input: remotes.Input{Name: rMinIO, Type: "s3",
			Options: map[string]string{"provider": "Minio", "endpoint": "https://minio.lan:9000", "region": "us-east-1",
				"access_key_id": "syncr-demo", "bucket": "archive", "force_path_style": "true"},
			Secrets: map[string]string{"secret_access_key": "syncr-demo-minio-secret"}}},
		{owner: ownerAdmin, input: remotes.Input{Name: rStorage, Type: "sftp",
			Options: map[string]string{"host": "storage.example.net", "port": "23", "user": "u123456", "path": "/home/backups"},
			Secrets: map[string]string{"pass": "syncr-demo-sftp-password"}}},
		{owner: ownerAdmin, input: remotes.Input{Name: rPhotos, Type: "local", Options: map[string]string{"path": "/srv/photos"}}},
		{owner: ownerAdmin, input: remotes.Input{Name: rB2, Type: "b2",
			Options: map[string]string{"account": "0012a3b4c5d6e7f0000000001", "bucket": "syncr-offsite"},
			Secrets: map[string]string{"key": "K001syncrDemoApplicationKey"}}},
		{owner: ownerAdmin, input: remotes.Input{Name: rNextcloud, Type: "webdav",
			Options: map[string]string{"url": "https://cloud.example.com/remote.php/dav/files/ada", "vendor": "nextcloud", "user": "ada"},
			Secrets: map[string]string{"pass": "syncr-demo-webdav-password"}}},
		{owner: ownerAdmin, input: remotes.Input{Name: rDisk1, Type: "local", Options: map[string]string{"path": "/mnt/disk1"}}},
		{owner: ownerAdmin, input: remotes.Input{Name: rDisk2, Type: "local", Options: map[string]string{"path": "/mnt/disk2"}}},
		{owner: ownerAdmin, input: remotes.Input{Name: rPool, Type: "union",
			Options: map[string]string{"action_policy": "epall", "create_policy": "mfs", "search_policy": "ff"}}, members: []string{rDisk1, rDisk2}},
		{owner: ownerAlice, input: remotes.Input{Name: rAliceDocs, Type: "local", Options: map[string]string{"path": "/srv/home/alice"}}},
		{owner: ownerAlice, input: remotes.Input{Name: rAliceB2, Type: "b2",
			Options: map[string]string{"account": "0012a3b4c5d6e7f0000000002", "bucket": "alice-backup"},
			Secrets: map[string]string{"key": "K001syncrDemoAliceKey"}}},
	}
}

// loc is a job location: a remote by name ("" for the worker's
// filesystem) and a path.
type loc struct {
	remote, path string
}

// demoJob is a seeded job. Times are durations before the seed's start:
// created is when it was queued; a job that ran started after queue and
// ran for run (the running job is still running).
type demoJob struct {
	owner   int
	worker  string
	mode    syncrv1.RcloneTransferMode
	src     loc
	dst     loc
	options *syncrv1.RcloneTransferOptions

	status  domain.JobStatus
	created time.Duration
	queue   time.Duration // queued -> started (dispatched halfway)
	run     time.Duration // started -> finished

	// stats are the final (or, while running, total) counters.
	stats    jobs.Stats
	err      jobs.JobError
	warnings []string
	exitCode int
	// cancelled: a cancel was requested (Finish then records cancelled).
	cancelled bool
	// samples is about how many progress samples a finished job stores
	// (default 60).
	samples int
}

const (
	copyMode = syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_COPY
	syncMode = syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_SYNC
	moveMode = syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_MOVE
)

func ptr[T any](v T) *T { return &v }

// ptrOptions are transfer options with transfers parallel transfers.
func ptrOptions(transfers int32, checksum bool) *syncrv1.RcloneTransferOptions {
	return &syncrv1.RcloneTransferOptions{Transfers: ptr(transfers), Checksum: checksum}
}

// done are the counters of a transfer that completed: everything
// transferred.
func done(bytes, files, checks int64) jobs.Stats {
	return jobs.Stats{Bytes: bytes, TotalBytes: bytes, Files: files, TotalFiles: files, Checks: checks}
}

// Ages of the manual jobs whose notifications the seed leaves retrying
// (the email) or dead (the webhook), and sending (the email).
const (
	retryingJobAgo = 25 * time.Minute
	sendingJobAgo  = 6 * time.Minute
)

// demoJobs are the jobs run by hand, oldest first. Exactly one is running.
func demoJobs() []demoJob {
	h, m, s, d := time.Hour, time.Minute, time.Second, 24*time.Hour
	return []demoJob{
		{owner: ownerAdmin, worker: "nas-01", mode: syncMode, src: loc{rPhotos, ""}, dst: loc{rB2, "photos"},
			status: domain.JobSucceeded, created: 6*d + 2*h, queue: 3 * s, run: 2*h + 14*m,
			stats: done(212*gib+318*mib, 48211, 48211), exitCode: 0,
			options: &syncrv1.RcloneTransferOptions{Transfers: ptr[int32](8), Checksum: true}},
		{owner: ownerAdmin, worker: "backup-host", mode: copyMode, src: loc{rNextcloud, ""}, dst: loc{rStorage, "nextcloud"},
			status: domain.JobSucceeded, created: 5*d + 23*h, queue: 2 * s, run: 18*m + 40*s,
			stats: done(14*gib+120*mib, 9306, 11240), exitCode: 0},
		{owner: ownerAdmin, worker: "office-pc", mode: copyMode, src: loc{"", `C:\Users\ada\Documents`}, dst: loc{rMinIO, "office-pc/documents"},
			status: domain.JobFailed, created: 5*d + 4*h, queue: 4 * s, run: 2*m + 11*s,
			stats: jobs.Stats{Bytes: 312 * mib, TotalBytes: 2*gib + 400*mib, Files: 410, TotalFiles: 3150, Errors: 1},
			err:   jobs.JobError{Code: "execution_failed", Message: "rclone exited with status 1: failed to copy: AccessDenied: Access Denied (status code: 403)"}, exitCode: 1},
		{owner: ownerAdmin, worker: "nas-01", mode: syncMode, src: loc{rPhotos, ""}, dst: loc{rMinIO, "photos"},
			status: domain.JobSucceeded, created: 4*d + 3*h, queue: 2 * s, run: 47*m + 5*s,
			stats: done(38*gib+12*mib, 5120, 48211), exitCode: 0,
			warnings: []string{"3 files changed during the transfer and were copied again", "skipped 1 symlink: /srv/photos/latest (--links is off)"}},
		{owner: ownerAlice, worker: "nas-01", mode: copyMode, src: loc{rAliceDocs, ""}, dst: loc{rAliceB2, "documents"},
			status: domain.JobSucceeded, created: 3*d + 20*h, queue: 5 * s, run: 6*m + 2*s,
			stats: done(1*gib+700*mib, 2210, 2210), exitCode: 0},
		{owner: ownerAdmin, worker: "laptop", mode: copyMode, src: loc{"", "/home/ada/projects"}, dst: loc{rStorage, "laptop/projects"},
			status: domain.JobFailed, created: 3*d + 6*h, queue: 15 * m,
			err: jobs.JobError{Code: "dispatch_timeout", Message: `not dispatched within 15m0s: worker "laptop" has never connected`}, exitCode: -1},
		{owner: ownerAdmin, worker: "backup-host", mode: syncMode, src: loc{rStorage, "nextcloud"}, dst: loc{rPool, "nextcloud"},
			status: domain.JobSucceeded, created: 2*d + 22*h, queue: 2 * s, run: 25*m + 31*s,
			stats: done(14*gib+160*mib, 214, 9310), exitCode: 0,
			options: &syncrv1.RcloneTransferOptions{TrackRenames: true, FastList: true}},
		{owner: ownerAdmin, worker: "nas-01", mode: moveMode, src: loc{"", "/srv/incoming"}, dst: loc{rPhotos, "inbox"},
			status: domain.JobCancelled, created: 2*d + 9*h, queue: 3 * s, run: 4*m + 12*s, cancelled: true,
			stats: jobs.Stats{Bytes: 2*gib + 100*mib, TotalBytes: 6 * gib, Files: 640, TotalFiles: 1810, Deletes: 640},
			err:   jobs.JobError{Code: "cancelled", Message: "cancelled by ada"}, exitCode: -1,
			options: &syncrv1.RcloneTransferOptions{DeleteEmptySrcDirs: true}},
		{owner: ownerAdmin, worker: "nas-01", mode: syncMode, src: loc{rPhotos, ""}, dst: loc{rMinIO, "photos"},
			status: domain.JobInterrupted, created: 2*d + 1*h, queue: 2 * s, run: 9*m + 48*s,
			stats: jobs.Stats{Bytes: 5*gib + 900*mib, TotalBytes: 41 * gib, Files: 820, TotalFiles: 5400, Checks: 30110},
			err:   jobs.JobError{Code: "worker_restarted", Message: "the worker restarted while the job was running"}, exitCode: -1},
		{owner: ownerAdmin, worker: "nas-01", mode: syncMode, src: loc{rPhotos, ""}, dst: loc{rMinIO, "photos"},
			status: domain.JobSucceeded, created: 2*d + 50*m, queue: 2 * s, run: 52*m + 9*s,
			stats: done(41*gib+300*mib, 5400, 48211), exitCode: 0},
		{owner: ownerAlice, worker: "office-pc", mode: copyMode, src: loc{"", `D:\Backups\alice`}, dst: loc{rAliceB2, "pc"},
			status: domain.JobFailed, created: 1*d + 19*h, queue: 3 * s,
			err: jobs.JobError{Code: "path_not_allowed", Message: `D:\Backups\alice: path is outside the worker's local_roots`}, exitCode: -1},
		{owner: ownerAdmin, worker: "backup-host", mode: copyMode, src: loc{rB2, "photos/2024"}, dst: loc{"", "/backup/restore-test"},
			status: domain.JobSucceeded, created: 1*d + 11*h, queue: 2 * s, run: 3*m + 40*s,
			stats: done(9*gib+40*mib, 1302, 1302), exitCode: 0,
			options: &syncrv1.RcloneTransferOptions{DryRun: true}},
		{owner: ownerAdmin, worker: "office-pc", mode: syncMode, src: loc{"", `D:\Backups`}, dst: loc{rStorage, "office-pc"},
			status: domain.JobInterrupted, created: 1*d + 2*h, queue: 6 * s, run: 31*m + 3*s,
			stats: jobs.Stats{Bytes: 12 * gib, TotalBytes: 30 * gib, Files: 1980, TotalFiles: 4800, Checks: 4800},
			err:   jobs.JobError{Code: "worker_lost", Message: "the worker no longer holds the job and reported no result"}, exitCode: -1},
		{owner: ownerAdmin, worker: "nas-01", mode: copyMode, src: loc{rNextcloud, "Documents"}, dst: loc{rMinIO, "nextcloud/documents"},
			status: domain.JobSucceeded, created: 20 * h, queue: 2 * s, run: 7*m + 30*s,
			stats: done(3*gib+512*mib, 4021, 4021), exitCode: 0},
		{owner: ownerAdmin, worker: "nas-01", mode: copyMode, src: loc{rNextcloud, "Shared"}, dst: loc{rMinIO, "nextcloud/shared"},
			status: domain.JobFailed, created: 14 * h, queue: 2 * s, run: 1*m + 2*s,
			stats:    jobs.Stats{Bytes: 88 * mib, TotalBytes: 900 * mib, Files: 61, TotalFiles: 702, Errors: 3},
			err:      jobs.JobError{Code: "execution_failed", Message: "rclone exited with status 7: 3 errors: couldn't list directory: 401 Unauthorized"},
			warnings: []string{"retrying after a low-level error (attempt 2 of 3)"}, exitCode: 7},
		{owner: ownerAlice, worker: "nas-01", mode: copyMode, src: loc{rAliceDocs, ""}, dst: loc{rAliceB2, "documents"},
			status: domain.JobSucceeded, created: 12 * h, queue: 4 * s, run: 1*m + 15*s,
			stats: done(240*mib, 96, 2306), exitCode: 0},
		{owner: ownerAdmin, worker: "backup-host", mode: syncMode, src: loc{rStorage, "nextcloud"}, dst: loc{rPool, "nextcloud"},
			status: domain.JobSucceeded, created: 9 * h, queue: 2 * s, run: 2*m + 51*s,
			stats: done(420*mib, 37, 9347), exitCode: 0,
			options: &syncrv1.RcloneTransferOptions{TrackRenames: true, FastList: true}},
		{owner: ownerAdmin, worker: "office-pc", mode: copyMode, src: loc{"", `C:\Users\ada\Documents`}, dst: loc{rMinIO, "office-pc/documents"},
			status: domain.JobSucceeded, created: 7 * h, queue: 3 * s, run: 11*m + 6*s,
			stats: done(2*gib+410*mib, 3166, 3166), exitCode: 0,
			warnings: []string{`skipped "C:\Users\ada\Documents\Outlook\ada.ost": the file is in use by another process`}},
		{owner: ownerAdmin, worker: "nas-01", mode: syncMode, src: loc{rPhotos, ""}, dst: loc{rB2, "photos"},
			status: domain.JobSucceeded, created: 5 * h, queue: 2 * s, run: 38*m + 20*s,
			stats: done(17*gib+800*mib, 2280, 50491), exitCode: 0,
			options: &syncrv1.RcloneTransferOptions{Transfers: ptr[int32](8), Checksum: true, BandwidthLimitBytesPerSecond: ptr(40 * mib)}},
		{owner: ownerAdmin, worker: "nas-01", mode: copyMode, src: loc{rPool, "media"}, dst: loc{rStorage, "media"},
			status: domain.JobCancelled, created: 3 * h, cancelled: true, exitCode: -1},
		{owner: ownerAdmin, worker: "backup-host", mode: copyMode, src: loc{rNextcloud, "Photos"}, dst: loc{"", "/backup/nextcloud-photos"},
			status: domain.JobSucceeded, created: 2 * h, queue: 2 * s, run: 14*m + 44*s,
			stats: done(6*gib+222*mib, 1890, 1890), exitCode: 0},
		{owner: ownerAdmin, worker: "nas-01", mode: copyMode, src: loc{rNextcloud, "Shared"}, dst: loc{rMinIO, "nextcloud/shared"},
			status: domain.JobFailed, created: retryingJobAgo, queue: 2 * s, run: 48 * s,
			stats: jobs.Stats{Bytes: 21 * mib, TotalBytes: 900 * mib, Files: 14, TotalFiles: 702, Errors: 1},
			err:   jobs.JobError{Code: "execution_failed", Message: "rclone exited with status 7: couldn't list directory: 401 Unauthorized"}, exitCode: 7},
		{owner: ownerAdmin, worker: "nas-01", mode: syncMode, src: loc{rPhotos, ""}, dst: loc{rMinIO, "photos"},
			status: domain.JobRunning, created: 12 * m, queue: 3 * s,
			stats:   jobs.Stats{TotalBytes: 380 * gib, TotalFiles: 52340},
			options: &syncrv1.RcloneTransferOptions{Transfers: ptr[int32](8), Checkers: ptr[int32](16), Checksum: true}},
		{owner: ownerAlice, worker: "office-pc", mode: copyMode, src: loc{"", `D:\Backups\alice`}, dst: loc{rAliceB2, "pc"},
			status: domain.JobQueued, created: 4 * m},
		{owner: ownerAdmin, worker: "backup-host", mode: copyMode, src: loc{rNextcloud, "Calendar"}, dst: loc{rStorage, "calendar"},
			status: domain.JobInterrupted, created: sendingJobAgo, queue: 2 * s, run: 50 * s,
			stats: jobs.Stats{Bytes: 3 * mib, TotalBytes: 40 * mib, Files: 12, TotalFiles: 150},
			err:   jobs.JobError{Code: "worker_lost", Message: "the worker no longer holds the job and reported no result"}, exitCode: -1},
		{owner: ownerAdmin, worker: "backup-host", mode: copyMode, src: loc{rNextcloud, ""}, dst: loc{rStorage, "nextcloud"},
			status: domain.JobDispatched, created: 1 * m, queue: 4 * s},
		{owner: ownerAdmin, worker: "laptop", mode: copyMode, src: loc{"", "/home/ada/projects"}, dst: loc{rStorage, "laptop/projects"},
			status: domain.JobQueued, created: 30 * s},
	}
}
