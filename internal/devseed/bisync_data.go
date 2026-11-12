package devseed

import (
	"fmt"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/cronexpr"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/link"
)

// The two-way sync of the demo: nas-01's shared folder and a folder on
// Cloudflare R2, kept in step every two hours (docs/adr/0018-two-way-sync.md).
const (
	// sShared is the schedule's name.
	sShared = "Shared folder ⇄ R2"
	// sharedLocal is Folder A (path1), on nas-01; sharedRemotePath is
	// Folder B (path2), on rR2.
	sharedLocal      = "/srv/shared"
	sharedRemotePath = "shared"
	// sharedBucket is rR2's bucket (its root).
	sharedBucket = "ada-shared"
)

// The schedule's expression and time zone.
const (
	sharedCron     = "0 */2 * * *"
	sharedTimezone = "Europe/Berlin"
)

// The two-way sync's history, counted back from the seed: the last run at
// or before sharedResyncAgo hit an unreadable file (RESYNC_REQUIRED), and
// the resync run by hand sharedResyncAfter later fixed it
// (sharedResyncRun); in the last sharedSafetyWindow its runs stop at the
// delete limit (SAFETY_ABORT), waiting for someone to decide.
const (
	sharedResyncAgo    = 7 * time.Hour
	sharedResyncAfter  = 25 * time.Minute
	sharedSafetyWindow = 4 * time.Hour
)

// sharedResyncRun is when the run that needed a resync was due: the
// schedule's last fire at or before sharedResyncAgo before the seed.
func sharedResyncRun(seed time.Time) time.Time {
	e, err := cronexpr.Parse(sharedCron, sharedTimezone)
	if err != nil {
		panic(err) // a constant expression
	}
	limit := seed.Add(-sharedResyncAgo)
	last := limit.Add(-3 * time.Hour)
	for t := e.Next(last); !t.After(limit); t = e.Next(t) {
		last = t
	}
	return last
}

// Messages of the two-way sync's failed runs, as the worker words them
// (internal/worker/runner/rclone, bisync_log.go) from rclone 1.75.2's.
const (
	sharedSideUnavailable = "Folder A (path1) is empty; refusing to sync it with the other side (is the drive mounted and the path right?)"
	sharedResyncRequired  = "a resync is required: failed to open source object: open " + sharedLocal + "/scans/2026-11-contract.pdf: permission denied; " +
		"run it once with resync to rebuild the pair's state from both sides as they are now"
	sharedSafetyAbort = `safety check stopped the two-way sync: too many deletes (>50%, 731 of 1268) on Path1 "` + sharedLocal + `/"; ` +
		"if these changes are intended, run it once with force"
	// sharedConflictWarning is the warning of the run with conflicts.
	sharedConflictWarning = `2 files changed on both sides; both versions were kept, renamed with the suffix "conflict"`
)

// sharedConflicts are the files changed on both sides in the run with
// conflicts, as the output lists them (relative to the two sides).
func sharedConflicts() []string { return []string{"budget-2026.xlsx", "notes/meeting-notes.md"} }

// sharedFiles is the shared folder's content below root (the same on both
// sides): documents, scans and photos, and the two versions each of the
// files of the run with conflicts.
func sharedFiles(root string) []demoFile {
	files := []demoFile{
		{root + "/budget-2026.xlsx", 420 * kib},
		{root + "/report.docx", 240 * kib},
		{root + "/notes/meeting-notes.md", 12 * kib},
		{root + "/notes/packing-list.md", 3 * kib},
		{root + "/scans/2026-10-invoice.pdf", 640 * kib},
		{root + "/scans/2026-11-contract.pdf", 2*mib + 300*kib},
		{root + "/photos/2026/11/IMG_2201.jpg", 3*mib + 200*kib},
		{root + "/photos/2026/11/IMG_2202.jpg", 3*mib + 90*kib},
	}
	for _, c := range sharedConflicts() {
		files = append(files, demoFile{root + "/" + c + ".conflict1", 10 * kib}, demoFile{root + "/" + c + ".conflict2", 11 * kib})
	}
	return files
}

// sharedDef is the two-way sync's definition (with resync, the run by
// hand that rebuilds its state).
func sharedDef(resync bool) defFunc {
	return func(s *Seed) (*syncrv1.JobDefinition, error) {
		id, err := s.remoteID(rR2)
		return &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_RcloneBisync{RcloneBisync: &syncrv1.RcloneBisyncDefinition{
			Path1: &syncrv1.JobLocation{Path: sharedLocal},
			Path2: &syncrv1.JobLocation{RemoteId: id, Path: sharedRemotePath},
			Options: &syncrv1.RcloneBisyncOptions{
				CreateEmptySrcDirs: true,
				Filters: []*syncrv1.FilterRule{
					{Action: syncrv1.FilterAction_FILTER_ACTION_EXCLUDE, Pattern: "*.tmp"},
					{Action: syncrv1.FilterAction_FILTER_ACTION_EXCLUDE, Pattern: ".~lock.*#"},
				},
			},
			Resync: resync,
		}}}, err
	}
}

// sharedOutput is the output of a successful run of the two-way sync.
func sharedOutput(reason syncrv1.BisyncResyncReason, conflicts []string) *syncrv1.JobOutput {
	return &syncrv1.JobOutput{Output: &syncrv1.JobOutput_RcloneBisync{RcloneBisync: &syncrv1.RcloneBisyncOutput{
		ResyncReason: reason, Conflicts: int64(len(conflicts)), ConflictPaths: conflicts,
	}}}
}

// sharedFileCount is about how many files the shared folder holds.
const sharedFileCount = 1268

// sharedFailed is the outcome of a run of the two-way sync that rclone
// stopped with code after run.
func sharedFailed(run time.Duration, code syncrv1.JobErrorCode, msg string, exit int, checks int64) runOutcome {
	return failedRun(run, link.ErrorCodeName(code), msg, exit, jobs.Stats{Checks: checks, TotalFiles: sharedFileCount, Errors: 1})
}

// demoBisyncSchedules are the two-way sync's schedule: every two hours on
// nas-01, with about three days of runs that show each of the kind's
// states: the first run, a resync (FIRST_RUN); quiet runs; a run with two
// conflicts; a run that found Folder A empty (SIDE_UNAVAILABLE); a run that
// needs a resync (RESYNC_REQUIRED), followed by the resync run by hand
// (demoBisyncJobs); and, in the last hours, runs stopped by the delete
// limit (SAFETY_ABORT).
func demoBisyncSchedules(seed time.Time) []demoSchedule {
	h, m, s, d := time.Hour, time.Minute, time.Second, 24*time.Hour
	resyncRun := sharedResyncRun(seed)
	return []demoSchedule{{
		owner: ownerAdmin, name: sShared, description: "Keep the NAS's shared folder and the R2 copy the laptop syncs with in step.",
		cron: sharedCron, timezone: sharedTimezone, worker: "nas-01",
		missed: schedules.MissedPolicy{Action: schedules.MissedRunLate, MaxDelay: h}, overlap: schedules.OverlapSkip,
		links: []demoLink{{cDiscord, []domain.NotificationEvent{domain.EventFailed, domain.EventMissed}}},
		job:   demoJob{owner: ownerAdmin, def: sharedDef(false)},
		age:   3*d + 4*h,
		outcome: func(n int, at time.Time) runOutcome {
			switch {
			case at.After(seed.Add(-sharedSafetyWindow)):
				// Most of the folder was moved away on the NAS; every run
				// stops until someone runs it once with force (or puts the
				// files back).
				return sharedFailed(14*s, syncrv1.JobErrorCode_JOB_ERROR_CODE_SAFETY_ABORT, sharedSafetyAbort, 1, 2*sharedFileCount)
			case !at.Before(resyncRun) && at.Before(resyncRun.Add(m)):
				// The run before the resync run by hand (launched within a
				// few seconds of its fire, after other schedules' runs).
				return sharedFailed(38*s, syncrv1.JobErrorCode_JOB_ERROR_CODE_RESYNC_REQUIRED, sharedResyncRequired, 7, 2*sharedFileCount)
			case n == 0:
				// The pair's first run on nas-01: a resync, which uploads
				// the folder (a union of both sides).
				return runOutcome{action: launch, status: domain.JobSucceeded, run: 9*m + 12*s,
					stats:  done(3*gib+410*mib, sharedFileCount-14, 2*sharedFileCount),
					output: sharedOutput(syncrv1.BisyncResyncReason_BISYNC_RESYNC_REASON_FIRST_RUN, nil)}
			case n == 6:
				o := succeeded(41*s, 4*mib+400*kib, 6, 2*sharedFileCount)
				o.warnings, o.output = []string{sharedConflictWarning},
					sharedOutput(syncrv1.BisyncResyncReason_BISYNC_RESYNC_REASON_UNSPECIFIED, sharedConflicts())
				return o
			case n == 13:
				// The USB drive behind the shared folder was not mounted.
				return sharedFailed(6*s, syncrv1.JobErrorCode_JOB_ERROR_CODE_SIDE_UNAVAILABLE, sharedSideUnavailable, 7, 0)
			}
			o := succeeded(time.Duration(varied(n, 18, 95))*s, varied(n, 1, 60)*mib+varied(n+2, 0, 900)*kib, varied(n, 0, 24), 2*sharedFileCount)
			o.stats.Deletes = varied(n+1, 0, 4)
			o.output = sharedOutput(syncrv1.BisyncResyncReason_BISYNC_RESYNC_REASON_UNSPECIFIED, nil)
			return o
		},
	}}
}

// demoBisyncJobs are the two-way sync's jobs run by hand: the resync that
// followed its RESYNC_REQUIRED run, once the unreadable file was fixed.
// seed is the seed's start.
func demoBisyncJobs(seed time.Time) []demoJob {
	m, s := time.Minute, time.Second
	return []demoJob{
		{owner: ownerAdmin, worker: "nas-01", def: sharedDef(true),
			status: domain.JobSucceeded, created: seed.Sub(sharedResyncRun(seed).Add(sharedResyncAfter)), queue: 2 * s, run: 2*m + 47*s, samples: 12,
			stats:  done(2*mib+300*kib, 1, 2*sharedFileCount),
			output: sharedOutput(syncrv1.BisyncResyncReason_BISYNC_RESYNC_REASON_REQUESTED, nil)},
	}
}

// remoteID returns a seeded remote's ID as a definition names it.
func (s *Seed) remoteID(name string) (string, error) {
	id, ok := s.Remotes[name]
	if !ok {
		return "", fmt.Errorf("unknown demo remote %q", name)
	}
	return id.String(), nil
}
