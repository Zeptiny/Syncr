package devseed

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// demoRepoPassword is the password of every demo repository (a public demo
// value: the repositories do not exist).
//
//nolint:gosec // G101: a public demo value
const demoRepoPassword = "syncr-demo-restic"

// Demo repository names.
const (
	repoPhotos    = "Photos vault"
	repoNASDocs   = "NAS documents"
	repoOffice    = "Office laptop"
	repoOldLaptop = "Old laptop"
	repoRetired   = "Retired NAS"
	repoAlice     = "Alice's backups"
)

// demoRepo is a seeded restic repository and the snapshots its (pretend)
// backups left in it.
type demoRepo struct {
	owner       int
	name        string
	description string
	// remote is the demo remote's name, backend its rclone backend (how
	// the canned probes recognize it) and path the path on the remote.
	remote, backend, path string
	// age is how long before the seed it was created.
	age time.Duration

	// The snapshots: count of them, every apart, the newest newestAgo
	// before the seed, taken on host of paths with tags.
	count     int
	every     time.Duration
	newestAgo time.Duration
	host      string
	paths     []string
	tags      []string
	// processed and fileCount are about what each backup read.
	processed int64
	fileCount int64
	// tree lists the files a snapshot holds, as tree paths.
	tree func() []demoFile

	// refreshedAgo is how long before the seed the snapshot cache was
	// filled.
	refreshedAgo time.Duration
	// gone: the repository's data was deleted during the history (its
	// row is soft-deleted and nothing is at its location any more).
	gone bool
}

// key identifies the repository's location for the canned probes.
func (d demoRepo) key() string { return d.backend + ":" + d.path }

// demoRepos are the repositories, in creation order.
func demoRepos() []demoRepo {
	h, m, day := time.Hour, time.Minute, 24*time.Hour
	return []demoRepo{
		{owner: ownerAdmin, name: repoPhotos, description: "Nightly backup of the photo library.",
			remote: rB2, backend: "b2", path: "restic/photos", age: 34 * day,
			count: 30, every: day, newestAgo: 9*h + 40*m, host: "nas-01", paths: []string{"/srv/photos"}, tags: []string{"photos"},
			processed: 120 * gib, fileCount: 48211, tree: func() []demoFile { return photoFiles("/srv/photos") }, refreshedAgo: 35 * m},
		{owner: ownerAdmin, name: repoNASDocs, description: "Documents on the NAS.",
			remote: rMinIO, backend: "s3", path: "restic/nas-docs", age: 21 * day,
			count: 14, every: day, newestAgo: 11 * h, host: "nas-01", paths: []string{"/srv/docs"}, tags: []string{"docs"},
			processed: 3*gib + 400*mib, fileCount: 4120, tree: func() []demoFile { return documentFiles("/srv/docs") }, refreshedAgo: 2 * h},
		{owner: ownerAdmin, name: repoOffice, description: "Ada's documents on the office PC.",
			remote: rStorage, backend: "sftp", path: "backups/office", age: 12 * day,
			count: 10, every: day, newestAgo: 4 * h, host: "OFFICE-PC", paths: []string{`C:\Users\ada\Documents`}, tags: []string{"office"},
			processed: 2*gib + 410*mib, fileCount: 3166,
			tree:         func() []demoFile { return documentFiles("/C/Users/ada/Documents") },
			refreshedAgo: 3 * h},
		{owner: ownerAdmin, name: repoOldLaptop, description: "The laptop that was replaced in the spring.",
			remote: rStorage, backend: "sftp", path: "backups/old-laptop", age: 29 * day,
			count: 6, every: 7 * day, newestAgo: 2 * day, host: "old-laptop", paths: []string{"/backup/old-laptop"}, tags: []string{"laptop"},
			processed: 18 * gib, fileCount: 21044, tree: func() []demoFile { return documentFiles("/backup/old-laptop") }, refreshedAgo: 26 * h},
		{owner: ownerAdmin, name: repoRetired, description: "Shares of the NAS that was retired.",
			remote: rMinIO, backend: "s3", path: "restic/retired-nas", age: 33 * day, gone: true},
		{owner: ownerAlice, name: repoAlice, description: "",
			remote: rAliceB2, backend: "b2", path: "restic", age: 15 * day,
			count: 8, every: day, newestAgo: 13 * h, host: "nas-01", paths: []string{"/srv/home/alice"}, tags: nil,
			processed: 1*gib + 700*mib, fileCount: 2306,
			tree: func() []demoFile {
				return []demoFile{{"/srv/home/alice/thesis.pdf", 12 * mib}, {"/srv/home/alice/notes.md", 9 * kib}}
			},
			refreshedAgo: 5 * h},
	}
}

// addedBytes is what the n-th backup of processed bytes added: between
// 0.5 and 3 GiB for a 120 GiB source, in proportion for others.
func addedBytes(n int, processed int64) int64 {
	return varied(n, 500, 3000) * (processed / (120 * 1024))
}

// snapshotID is the fixed ID of a repository's n-th snapshot (0 is the
// oldest).
func snapshotID(key string, n int) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "syncr devseed snapshot %s %d", key, n))
	return hex.EncodeToString(sum[:])
}

// snapshots are the repository's snapshots at the seed's start, newest
// first.
func (d demoRepo) snapshots(start time.Time) []restic.Snapshot {
	out := make([]restic.Snapshot, 0, d.count)
	for i := range d.count {
		n := d.count - 1 - i // 0 is the oldest
		at := start.Add(-d.newestAgo - time.Duration(i)*d.every).Truncate(time.Second)
		added := addedBytes(n, d.processed)
		changed := varied(n+3, 0, 9)
		fresh := varied(n+5, 2, 40)
		total := d.fileCount - int64(i)*3
		s := restic.Snapshot{
			ID: snapshotID(d.key(), n), Time: at, Hostname: d.host, Username: "root", ProgramVersion: "restic 0.19.1",
			Paths: d.paths, Tags: d.tags,
			Summary: &restic.SnapshotSummary{
				BackupStart: at, BackupEnd: at.Add(time.Duration(varied(n, 90, 900)) * time.Second),
				FilesNew: fresh, FilesChanged: changed, FilesUnmodified: total - fresh - changed,
				DirsNew: varied(n, 0, 3), DirsChanged: varied(n, 1, 12), DirsUnmodified: 180,
				DataBlobs: varied(n, 40, 900), TreeBlobs: varied(n, 3, 30),
				DataAdded: added, DataAddedPacked: added * 7 / 10,
				TotalFilesProcessed: total, TotalBytesProcessed: d.processed - int64(i)*added/4,
			},
		}
		if n > 0 {
			s.Parent = snapshotID(d.key(), n-1)
		}
		if d.host == "OFFICE-PC" {
			s.Username = `OFFICE-PC\ada`
		}
		out = append(out, s)
	}
	return out
}

// files are the files of the repository's snapshots, as tree paths.
func (d demoRepo) files() []demoFile {
	if d.tree == nil {
		return nil
	}
	return d.tree()
}

// ---------------------------------------------------------------------------
// Definitions and outputs
// ---------------------------------------------------------------------------

// repoID returns a seeded repository's ID as a definition names it.
func (s *Seed) repoID(name string) (string, error) {
	id, ok := s.Repositories[name]
	if !ok {
		return "", fmt.Errorf("unknown demo repository %q", name)
	}
	return id.String(), nil
}

// defFunc builds a job definition over the seeded repositories.
type defFunc func(s *Seed) (*syncrv1.JobDefinition, error)

func backupDef(repo string, vss bool, paths ...string) defFunc {
	return func(s *Seed) (*syncrv1.JobDefinition, error) {
		id, err := s.repoID(repo)
		return &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticBackup{ResticBackup: &syncrv1.ResticBackupDefinition{
			RepositoryId: id, Paths: paths, Excludes: []string{"*.tmp", ".cache"}, UseFsSnapshot: vss, ExcludeCaches: true,
		}}}, err
	}
}

func forgetDef(repo string) defFunc {
	return func(s *Seed) (*syncrv1.JobDefinition, error) {
		id, err := s.repoID(repo)
		return &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticForget{ResticForget: &syncrv1.ResticForgetDefinition{
			RepositoryId: id, Policy: &syncrv1.ResticRetentionPolicy{KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 12}, Prune: true,
		}}}, err
	}
}

func checkDef(repo string, percent int32) defFunc {
	return func(s *Seed) (*syncrv1.JobDefinition, error) {
		id, err := s.repoID(repo)
		return &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticCheck{ResticCheck: &syncrv1.ResticCheckDefinition{
			RepositoryId: id, ReadDataPercent: percent,
		}}}, err
	}
}

func restoreDef(repo, snapshot, target string, paths ...string) defFunc {
	return func(s *Seed) (*syncrv1.JobDefinition, error) {
		id, err := s.repoID(repo)
		return &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticRestore{ResticRestore: &syncrv1.ResticRestoreDefinition{
			RepositoryId: id, SnapshotId: snapshot, Target: target, Paths: paths, Verify: true,
			Overwrite: syncrv1.ResticRestoreOverwrite_RESTIC_RESTORE_OVERWRITE_NEVER,
		}}}, err
	}
}

func deleteDataDef(repo string) defFunc {
	return func(s *Seed) (*syncrv1.JobDefinition, error) {
		id, err := s.repoID(repo)
		return &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticDeleteData{ResticDeleteData: &syncrv1.ResticDeleteDataDefinition{
			RepositoryId: id,
		}}}, err
	}
}

// backupOutput is the result of the n-th backup of processed bytes in
// files files.
func backupOutput(key string, n int, processed, files int64) *syncrv1.JobOutput {
	added := addedBytes(n, processed)
	changed, fresh := varied(n+3, 0, 9), varied(n+5, 2, 40)
	return &syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticBackup{ResticBackup: &syncrv1.ResticBackupOutput{
		SnapshotId: snapshotID(key+" job", n), DataAddedBytes: added, DataAddedPackedBytes: added * 7 / 10,
		FilesNew: fresh, FilesChanged: changed, FilesUnmodified: files - fresh - changed,
		DirsNew: varied(n, 0, 3), DirsChanged: varied(n, 1, 12), DirsUnmodified: 180,
		TotalFilesProcessed: files, TotalBytesProcessed: processed,
	}}}
}

// backedUp is the outcome of a successful backup.
func backedUp(key string, n int, run time.Duration, processed, files int64) runOutcome {
	return runOutcome{action: launch, status: domain.JobSucceeded, run: run, stats: done(processed, files, 0),
		output: backupOutput(key, n, processed, files)}
}

func forgetOutput(key string, n int, removed int, kept int32) *syncrv1.JobOutput {
	out := &syncrv1.ResticForgetOutput{KeptCount: kept}
	for i := range removed {
		out.RemovedSnapshotIds = append(out.RemovedSnapshotIds, snapshotID(key+" removed", n*10+i))
	}
	return &syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticForget{ResticForget: out}}
}

func checkOutput(percent int32, errs int64) *syncrv1.JobOutput {
	out := &syncrv1.ResticCheckOutput{ReadDataPercent: percent, NumErrors: errs}
	if errs > 0 {
		out.SuggestRepairIndex = true
		out.BrokenPacks = []string{snapshotID("broken pack", 1), snapshotID("broken pack", 2)}
	}
	return &syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticCheck{ResticCheck: out}}
}

// ---------------------------------------------------------------------------
// Schedules on repositories
// ---------------------------------------------------------------------------

// Demo schedule names on repositories.
const (
	sPhotosBackup = "Nightly photos backup"
	sPhotosForget = "Weekly retention: photos"
	sPhotosCheck  = "Monthly deep check: photos"
	sOfficeBackup = "Office documents backup"
	sOldLaptop    = "Old laptop weekly backup"
)

// demoResticSchedules are the schedules that run restic jobs: a nightly
// backup, its weekly retention and monthly check, an hourly backup of a
// Windows worker with a filesystem snapshot, and the weekly backup of the
// repository whose data is being deleted (paused by the deletion).
func demoResticSchedules(seed time.Time) []demoSchedule {
	h, m, d := time.Hour, time.Minute, 24*time.Hour
	photos, office, old := demoRepos()[0], demoRepos()[2], demoRepos()[3]
	return []demoSchedule{
		{
			owner: ownerAdmin, name: sPhotosBackup, description: "Back up the photo library every night.",
			cron: "0 2 * * *", timezone: "Europe/Berlin", label: "nas",
			missed: schedules.MissedPolicy{Action: schedules.MissedRunLate, MaxDelay: 6 * h}, overlap: schedules.OverlapSkip,
			links: []demoLink{{cDiscord, []domain.NotificationEvent{domain.EventFailed, domain.EventInterrupted, domain.EventMissed}}},
			job:   demoJob{owner: ownerAdmin, def: backupDef(repoPhotos, false, "/srv/photos")},
			age:   30 * d,
			outcome: func(n int, _ time.Time) runOutcome {
				o := backedUp(photos.key(), n, time.Duration(varied(n, 6, 24))*m, photos.processed+int64(n)*900*mib, photos.fileCount+int64(n)*31)
				if n == 17 {
					o.warnings = []string{"3 items could not be read"}
				}
				return o
			},
		},
		{
			owner: ownerAdmin, name: sPhotosForget, description: "Keep 7 daily, 4 weekly and 12 monthly snapshots, then prune.",
			cron: "0 4 * * 0", timezone: "Europe/Berlin", label: "nas",
			missed: schedules.MissedPolicy{Action: schedules.MissedRunLate, MaxDelay: 12 * h}, overlap: schedules.OverlapQueue,
			job: demoJob{owner: ownerAdmin, def: forgetDef(repoPhotos)},
			age: 30 * d,
			outcome: func(n int, _ time.Time) runOutcome {
				return runOutcome{action: launch, status: domain.JobSucceeded, run: time.Duration(varied(n, 3, 11)) * m,
					output: forgetOutput(photos.key(), n, int(varied(n, 3, 7)), 23)}
			},
		},
		{
			owner: ownerAdmin, name: sPhotosCheck, description: "Read a tenth of the data on the first of the month.",
			cron: "0 5 1 * *", timezone: "Europe/Berlin", label: "nas",
			missed: schedules.MissedPolicy{Action: schedules.MissedRunLate, MaxDelay: 24 * h}, overlap: schedules.OverlapQueue,
			links: []demoLink{{cMail, []domain.NotificationEvent{domain.EventFailed}}},
			job:   demoJob{owner: ownerAdmin, def: checkDef(repoPhotos, 10)},
			age:   33 * d,
			outcome: func(int, time.Time) runOutcome {
				return runOutcome{action: launch, status: domain.JobSucceeded, run: 41 * m, output: checkOutput(10, 0)}
			},
		},
		{
			owner: ownerAdmin, name: sOfficeBackup, description: "Back up Ada's documents while the PC is on, with a filesystem snapshot.",
			cron: "30 * * * *", timezone: "Europe/Berlin", worker: "office-pc",
			missed: schedules.MissedPolicy{Action: schedules.MissedRunLate, MaxDelay: 14 * h}, overlap: schedules.OverlapSkip,
			job: demoJob{owner: ownerAdmin, def: backupDef(repoOffice, true, `C:\Users\ada\Documents`)},
			age: 2*d + 5*h,
			outcome: func(n int, at time.Time) runOutcome {
				if !officeOnline(at, seed) {
					return runOutcome{action: wait} // runs merge until the PC is back
				}
				return backedUp(office.key(), n, time.Duration(varied(n, 40, 160))*time.Second, office.processed+int64(n)*3*mib, office.fileCount+int64(n))
			},
		},
		{
			owner: ownerAdmin, name: sOldLaptop, description: "Weekly backup of the old laptop's last image.",
			cron: "0 6 * * 5", timezone: "UTC", worker: "backup-host",
			missed: schedules.MissedPolicy{Action: schedules.MissedRunLate, MaxDelay: 12 * h}, overlap: schedules.OverlapSkip,
			job: demoJob{owner: ownerAdmin, def: backupDef(repoOldLaptop, false, "/backup/old-laptop")},
			age: 27 * d,
			outcome: func(n int, _ time.Time) runOutcome {
				return backedUp(old.key(), n, time.Duration(varied(n, 9, 22))*m, old.processed, old.fileCount)
			},
		},
	}
}

// ---------------------------------------------------------------------------
// Restic jobs run by hand, and the live state
// ---------------------------------------------------------------------------

// Ages of the live restic jobs and of the deletions.
const (
	retiredDeletedAgo  = 2*24*time.Hour + 3*time.Hour
	oldLaptopCheckAgo  = 9 * time.Minute
	oldLaptopDeleteAgo = 100 * time.Second
)

// demoResticJobs are the restic jobs run by hand, oldest first: checks
// (one failed), a restore, backups of the NAS documents, and the live
// state: a backup that is running with another waiting behind it on the
// same repository, and a check that is running on the repository whose
// data deletion waits for it.
func demoResticJobs() []demoJob {
	h, m, s, d := time.Hour, time.Minute, time.Second, 24*time.Hour
	docs, alice := demoRepos()[1], demoRepos()[5]
	docsBackup := func(n int, created, run time.Duration) demoJob {
		return demoJob{owner: ownerAdmin, worker: "nas-01", def: backupDef(repoNASDocs, false, "/srv/docs"),
			status: domain.JobSucceeded, created: created, queue: 2 * s, run: run,
			stats: done(docs.processed+int64(n)*40*mib, docs.fileCount+int64(n)*6, 0), samples: 12,
			output: backupOutput(docs.key(), n, docs.processed+int64(n)*40*mib, docs.fileCount+int64(n)*6)}
	}
	return []demoJob{
		{owner: ownerAdmin, worker: "nas-01", def: checkDef(repoNASDocs, 0),
			status: domain.JobSucceeded, created: 19*d + 4*h, queue: 2 * s, run: 2*m + 40*s, samples: 6, output: checkOutput(0, 0)},
		docsBackup(0, 13*d+7*h, 3*m+12*s),
		{owner: ownerAdmin, worker: "backup-host", def: checkDef(repoPhotos, 100),
			status: domain.JobFailed, created: 11*d + 2*h, queue: 3 * s, run: 3*h + 5*m, samples: 6, exitCode: 1,
			err:    jobs.JobError{Code: "execution_failed", Message: "the check found 2 error(s)"},
			output: checkOutput(100, 2)},
		{owner: ownerAdmin, worker: "nas-01", def: restoreDef(repoPhotos, snapshotID(demoRepos()[0].key(), 21), "/srv/restore", "/srv/photos/2026/03"),
			status: domain.JobSucceeded, created: 8*d + 6*h, queue: 2 * s, run: 1*m + 50*s, samples: 8,
			stats: done(31*mib, 9, 0),
			output: &syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticRestore{ResticRestore: &syncrv1.ResticRestoreOutput{
				TotalFiles: 14, FilesRestored: 14, TotalBytes: 31 * mib, BytesRestored: 31 * mib,
			}}}},
		docsBackup(1, 6*d+8*h, 2*m+48*s),
		{owner: ownerAlice, worker: "nas-01", def: backupDef(repoAlice, false, "/srv/home/alice"),
			status: domain.JobSucceeded, created: 4*d + 13*h, queue: 4 * s, run: 1*m + 31*s, samples: 8,
			stats:  done(alice.processed, alice.fileCount, 0),
			output: backupOutput(alice.key(), 0, alice.processed, alice.fileCount)},
		{owner: ownerAdmin, worker: "nas-01", def: checkDef(repoOffice, 5),
			status: domain.JobSucceeded, created: 3*d + 1*h, queue: 2 * s, run: 6*m + 3*s, samples: 6, output: checkOutput(5, 0)},
		docsBackup(2, 1*d+11*h+20*m, 2*m+55*s),
		{owner: ownerAdmin, worker: "backup-host", def: checkDef(repoOldLaptop, 0), static: true,
			status: domain.JobRunning, created: oldLaptopCheckAgo, queue: 2 * s},
		{owner: ownerAdmin, worker: "nas-01", def: backupDef(repoNASDocs, false, "/srv/docs"), static: true,
			status: domain.JobRunning, created: 7 * m, queue: 2 * s,
			stats: jobs.Stats{TotalBytes: 46 * gib, TotalFiles: 4160}},
		{owner: ownerAdmin, worker: "backup-host", def: backupDef(repoNASDocs, false, "/srv/docs"),
			status: domain.JobQueued, created: 3 * m},
	}
}
