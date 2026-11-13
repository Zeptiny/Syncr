package devseed

import (
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Demo contact names (the admin's).
const (
	cDiscord = "Ops Discord"
	cMail    = "Admin mail"
	cHA      = "Home Assistant"
)

// demoContact is a seeded contact. The targets do not exist: a seeded
// controller's channels only log what they would send (notify.DryRun).
type demoContact struct {
	owner     int
	name      string
	channel   string
	options   map[string]string
	secrets   map[string]string
	jobEvents []domain.NotificationEvent
}

// demoContacts are the contacts, one of each built-in channel.
//
//nolint:gosec // G101: public demo values for hosts that do not exist
func demoContacts() []demoContact {
	return []demoContact{
		{owner: ownerAdmin, name: cDiscord, channel: "discord",
			options: map[string]string{"username": "Syncr"},
			secrets: map[string]string{"webhook_url": "https://discord.com/api/webhooks/1234567890123456789/syncr-demo-discord-token"}},
		{owner: ownerAdmin, name: cMail, channel: "email",
			options: map[string]string{"host": "smtp.example.net", "port": "587", "security": "starttls", "username": "syncr@example.net",
				"from": "Syncr <syncr@example.net>", "to": "ada@example.net, ops@example.net"},
			secrets:   map[string]string{"password": "syncr-demo-smtp-password"},
			jobEvents: []domain.NotificationEvent{domain.EventFailed, domain.EventInterrupted}},
		{owner: ownerAdmin, name: cHA, channel: "webhook",
			options: map[string]string{"format": "syncr"},
			secrets: map[string]string{"url": "http://homeassistant.lan:8123/api/webhook/syncr-demo-hook",
				"signing_secret": "whsec_c3luY3ItZGVtby1zaWduaW5nLXNlY3JldA=="},
			jobEvents: []domain.NotificationEvent{domain.EventFailed}},
	}
}

// Demo schedule names.
const (
	sNightly = "Nightly photos → B2"
	sOffice  = "Office docs hourly"
	sWeekly  = "Weekly archive"
	sOffsite = "Offsite mirror"
	sOldNAS  = "Old NAS sync"
	sAlice   = "Alice's documents"
)

// demoLink links a demo schedule to a demo contact.
type demoLink struct {
	contact string
	events  []domain.NotificationEvent
}

// demoSchedule is a seeded schedule with its history: it was created age
// before the seed, and every run up to the seed is decided by outcome.
type demoSchedule struct {
	owner       int
	name        string
	description string
	cron        string
	timezone    string
	// worker targets one worker by name; label targets a label.
	worker, label string
	missed        schedules.MissedPolicy
	overlap       schedules.Overlap
	links         []demoLink
	// job is the definition each run launches (owner, mode, locations,
	// options; the worker comes from the target).
	job demoJob
	age time.Duration
	// pausedAgo pauses the schedule (by its owner) this long before the
	// seed.
	pausedAgo time.Duration
	// manualAgo adds a "Run now" this long before the seed (0: none).
	manualAgo time.Duration
	// outcome decides the n-th run (0 is the oldest) when it is open at
	// tick time at.
	outcome func(n int, at time.Time) runOutcome
}

// runAction is what the seed's scheduler does with an open run.
type runAction int

const (
	// launch starts the run's job now.
	launch runAction = iota
	// wait keeps the run pending (the worker is offline, or an active run
	// is ahead of it); at its deadline it is missed.
	wait
)

// runOutcome is the decision for one run and, when it launches, how its
// job ends.
type runOutcome struct {
	action runAction
	// missedReason is the reason of a waiting run that reaches its
	// deadline (schedules.ReasonWorkerOffline by default).
	missedReason string
	status       domain.JobStatus
	run          time.Duration
	stats        jobs.Stats
	err          jobs.JobError
	exitCode     int
	warnings     []string
	// output is the job's result (the restic kinds).
	output *syncrv1.JobOutput
}

// succeeded is the outcome of a successful transfer.
func succeeded(run time.Duration, bytes, files, checks int64) runOutcome {
	return runOutcome{action: launch, status: domain.JobSucceeded, run: run, stats: done(bytes, files, checks)}
}

// failedRun is the outcome of a failed transfer after run.
func failedRun(run time.Duration, code, msg string, exit int, st jobs.Stats) runOutcome {
	return runOutcome{action: launch, status: domain.JobFailed, run: run, stats: st, err: jobs.JobError{Code: code, Message: msg}, exitCode: exit}
}

// varied returns a deterministic value in [lo, hi) for the n-th run.
func varied(n int, lo, hi int64) int64 {
	span := hi - lo
	if span <= 0 || n < 0 {
		return lo
	}
	h := int64(n)*7919 + 97
	h ^= h >> 5
	return lo + h%span
}

// officeOnline reports whether the office PC is on at at: weekdays and
// Saturdays from 07:00 to 19:00 Berlin time, and in the hour before its
// last contact officeOffline before the seed, whatever the time of day;
// never since (it is offline now).
//
// That hour makes its hourly schedules' newest runs the same at every
// time of day: opened by the first of the three fires since, pending, the
// other two merged into it. Without it, a seed at night or on a Sunday
// had the PC off for more than the runs' 14-hour deadline, and in the
// hour after a deadline passed the newest run had nothing merged into it.
func officeOnline(at, seed time.Time) bool {
	lastContact := seed.Add(-officeOffline)
	switch {
	case at.After(lastContact):
		return false
	case at.After(lastContact.Add(-time.Hour)):
		return true
	}
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		berlin = time.UTC
	}
	l := at.In(berlin)
	return l.Weekday() != time.Sunday && l.Hour() >= 7 && l.Hour() < 19
}

// officeOffline is how long the office PC has been offline (its demo
// worker's last contact).
const officeOffline = 3 * time.Hour

// demoSchedules are the schedules, covering every target type, missed-run
// and overlap policy, paused and enabled, with ~30 days of runs: missed
// ones (worker offline, overlap, skipped by policy), coalesced ones (the
// office PC is off at night, the controller was down once), failed and
// interrupted jobs, one Run now, one paused by its owner and one paused
// because its worker was removed. seed is the seed's start.
func demoSchedules(seed time.Time) []demoSchedule {
	h, m, d := time.Hour, time.Minute, 24*time.Hour
	return []demoSchedule{
		{
			owner: ownerAdmin, name: sNightly, description: "Sync the photo library to Backblaze B2 every night.",
			cron: "30 2 * * *", timezone: "Europe/Berlin", label: "nas",
			missed: schedules.MissedPolicy{Action: schedules.MissedRunLate, MaxDelay: 6 * h}, overlap: schedules.OverlapSkip,
			links: []demoLink{
				{cDiscord, []domain.NotificationEvent{domain.EventFailed, domain.EventInterrupted, domain.EventMissed}},
				{cHA, []domain.NotificationEvent{domain.EventSucceeded, domain.EventFailed}},
			},
			job: demoJob{owner: ownerAdmin, mode: syncMode, src: loc{rPhotos, ""}, dst: loc{rB2, "photos"},
				options: ptrOptions(8, true)},
			age: 31 * d, manualAgo: 3*d + 5*h,
			outcome: func(n int, _ time.Time) runOutcome {
				switch {
				case n == 9:
					return runOutcome{action: wait} // nas-01 was off for a day: missed
				case n%11 == 4:
					return failedRun(3*m+20*time.Second, "execution_failed",
						"rclone exited with status 7: Failed to sync: b2: 503 service_unavailable: c001_v0001130_t0045 is too busy", 7,
						jobs.Stats{Bytes: 410 * mib, TotalBytes: 2 * gib, Files: 96, TotalFiles: 410, Checks: 48100, Errors: 2})
				case n%13 == 7:
					return runOutcome{action: launch, status: domain.JobInterrupted, run: 21 * m,
						stats: jobs.Stats{Bytes: 3 * gib, TotalBytes: 9 * gib, Files: 610, TotalFiles: 1830, Checks: 30200},
						err:   jobs.JobError{Code: "worker_restarted", Message: "the worker restarted while the job was running"}, exitCode: -1}
				}
				o := succeeded(time.Duration(varied(n, 12, 55))*m, varied(n, 900, 9000)*mib, varied(n, 150, 1900), 48211+int64(n)*40)
				if n%5 == 2 {
					o.warnings = []string{"skipped 1 symlink: /srv/photos/latest (--links is off)"}
				}
				return o
			},
		},
		{
			owner: ownerAdmin, name: sOffice, description: "Copy the office documents to MinIO while the PC is on.",
			cron: "0 * * * *", timezone: "Europe/Berlin", worker: "office-pc",
			// Long enough for a night's runs to merge into one (a run's
			// deadline is fixed when it opens).
			missed: schedules.MissedPolicy{Action: schedules.MissedRunLate, MaxDelay: 14 * h}, overlap: schedules.OverlapSkip,
			links: []demoLink{{cMail, []domain.NotificationEvent{domain.EventFailed, domain.EventMissed}}},
			job:   demoJob{owner: ownerAdmin, mode: copyMode, src: loc{"", `C:\Users\ada\Documents`}, dst: loc{rMinIO, "office-pc/documents"}},
			age:   2*d + 4*h,
			outcome: func(n int, at time.Time) runOutcome {
				if !officeOnline(at, seed) {
					return runOutcome{action: wait} // runs merge until the PC is back
				}
				if n == 6 {
					return failedRun(1*m+5*time.Second, "execution_failed",
						"rclone exited with status 1: failed to copy: AccessDenied: Access Denied (status code: 403)", 1,
						jobs.Stats{Bytes: 12 * mib, TotalBytes: 140 * mib, Files: 9, TotalFiles: 61, Errors: 1})
				}
				return succeeded(time.Duration(varied(n, 20, 140))*time.Second, varied(n, 1, 90)*mib, varied(n, 1, 40), 3166)
			},
		},
		{
			owner: ownerAdmin, name: sWeekly, description: "Archive Nextcloud to the storage box every Sunday.",
			cron: "0 3 * * 0", timezone: "UTC", worker: "backup-host",
			missed: schedules.MissedPolicy{Action: schedules.MissedRunLate, MaxDelay: 12 * h}, overlap: schedules.OverlapQueue,
			links: []demoLink{{cDiscord, []domain.NotificationEvent{domain.EventFailed, domain.EventInterrupted, domain.EventMissed}}},
			job: demoJob{owner: ownerAdmin, mode: copyMode, src: loc{rNextcloud, ""}, dst: loc{rStorage, "archive"},
				options: ptrOptions(4, false)},
			age: 30 * d,
			outcome: func(n int, _ time.Time) runOutcome {
				if n == 1 {
					// It queued behind a long manual restore and ran out of time.
					return runOutcome{action: wait, missedReason: schedules.ReasonOverlap}
				}
				return succeeded(time.Duration(varied(n, 95, 200))*m, varied(n, 14, 22)*gib, varied(n, 9300, 9800), 11240)
			},
		},
		{
			owner: ownerAdmin, name: sOffsite, description: "Mirror the pooled disks offsite every 6 hours; skip runs the worker cannot take.",
			cron: "0 */6 * * *", timezone: "UTC", label: "offsite",
			missed: schedules.MissedPolicy{Action: schedules.MissedSkip}, overlap: schedules.OverlapAllow,
			links: []demoLink{{cHA, []domain.NotificationEvent{domain.EventFailed, domain.EventMissed}}},
			job:   demoJob{owner: ownerAdmin, mode: syncMode, src: loc{rPool, "nextcloud"}, dst: loc{rB2, "pool"}},
			age:   12*d + 2*h,
			outcome: func(n int, _ time.Time) runOutcome {
				switch {
				case n%9 == 3:
					return runOutcome{action: wait} // backup-host was busy rebooting: skipped
				case n%17 == 8:
					return failedRun(40*time.Second, "execution_failed",
						"rclone exited with status 3: directory not found: pool/nextcloud", 3, jobs.Stats{Errors: 1})
				}
				return succeeded(time.Duration(varied(n, 60, 600))*time.Second, varied(n, 5, 600)*mib, varied(n, 1, 120), 9300+int64(n))
			},
		},
		{
			owner: ownerAdmin, name: sOldNAS, description: "Pull the old NAS's shares into the pool.",
			cron: "0 1 * * *", timezone: "UTC", worker: wOldNAS,
			missed: schedules.MissedPolicy{Action: schedules.MissedRunLate, MaxDelay: 2 * h}, overlap: schedules.OverlapSkip,
			links: []demoLink{{cDiscord, []domain.NotificationEvent{domain.EventFailed, domain.EventMissed}}},
			job:   demoJob{owner: ownerAdmin, mode: copyMode, src: loc{"", "/volume1/shares"}, dst: loc{rPool, "old-nas"}},
			age:   20 * d,
			outcome: func(n int, _ time.Time) runOutcome {
				return succeeded(time.Duration(varied(n, 4, 25))*m, varied(n, 50, 900)*mib, varied(n, 10, 300), 20100)
			},
		},
		{
			owner: ownerAlice, name: sAlice, description: "",
			cron: "15 22 * * *", timezone: "Europe/Paris", label: "linux",
			missed: schedules.MissedPolicy{Action: schedules.MissedRunLate, MaxDelay: time.Hour}, overlap: schedules.OverlapSkip,
			job: demoJob{owner: ownerAlice, mode: copyMode, src: loc{rAliceDocs, ""}, dst: loc{rAliceB2, "documents"}},
			age: 14 * d, pausedAgo: 4*d + 3*h,
			outcome: func(n int, _ time.Time) runOutcome {
				return succeeded(time.Duration(varied(n, 40, 300))*time.Second, varied(n, 2, 300)*mib, varied(n, 1, 90), 2306)
			},
		},
	}
}

// controllerDown is the window, counted back from the seed, during which
// the demo controller was down: fires in it coalesce into one run when it
// comes back.
func controllerDown(seed time.Time) (from, until time.Time) {
	day := seed.Add(-5 * 24 * time.Hour).UTC().Truncate(24 * time.Hour)
	return day.Add(-50 * time.Minute), day.Add(6*time.Hour + 30*time.Minute)
}
