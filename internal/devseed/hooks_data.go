package devseed

import (
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Names of the hooks nas-01's operator defined: the documents on the NAS
// belong to a document app with a database, which is stopped and dumped
// before a backup and started again afterwards.
const (
	hookStop  = "stop-paperless"
	hookDump  = "dump-paperless-db"
	hookStart = "start-paperless"
	hookPing  = "ping-healthcheck"
)

// demoHooks are the hooks nas-01 reports, sorted by name as the controller
// stores them.
func demoHooks() []workers.Hook {
	return []workers.Hook{
		{Name: hookDump, Description: "Dump the Paperless database to /srv/docs/db", Timeout: 15 * time.Minute},
		{Name: hookPing, Description: "Tell the uptime monitor that the backup ran", Timeout: 30 * time.Second},
		{Name: hookStart, Description: "Start the Paperless containers again", Timeout: 2 * time.Minute},
		{Name: hookStop, Description: "Stop the Paperless containers, so their files stay still", Timeout: 2 * time.Minute},
	}
}

// docsHooks are the hooks a backup of the NAS documents runs.
func docsHooks() *syncrv1.JobHooks {
	return &syncrv1.JobHooks{
		Before:    []string{hookStop, hookDump},
		OnSuccess: []string{hookPing},
		After:     []string{hookStart},
	}
}

// demoHookRun is one run of a hook of a demo job. It starts at from after
// the job's start, or, when fromEnd is set, that long before its end.
type demoHookRun struct {
	run     jobs.HookRun
	from    time.Duration
	fromEnd bool
}

// How long the hooks of a backup of the NAS documents take.
const (
	stopTook  = 3200 * time.Millisecond
	dumpTook  = 41 * time.Second
	pingTook  = 380 * time.Millisecond
	startTook = 4500 * time.Millisecond
	// startTimeout is the timeout of hookStart (demoHooks).
	startTimeout = 2 * time.Minute
)

// docsHookRuns are the hook runs of a finished backup of the NAS
// documents. With startTimedOut the app did not come back: the hook that
// starts it ran into its timeout after the backup was written.
func docsHookRuns(startTimedOut bool) []demoHookRun {
	start := demoHookRun{fromEnd: true, from: startTook, run: jobs.HookRun{
		Name: hookStart, Phase: domain.HookAfter, Status: domain.HookSucceeded, Duration: startTook,
		Output: "paperless-web\npaperless-worker",
	}}
	ping := demoHookRun{fromEnd: true, from: startTook + pingTook, run: jobs.HookRun{
		Name: hookPing, Phase: domain.HookOnSuccess, Status: domain.HookSucceeded, Duration: pingTook, Output: "OK",
	}}
	if startTimedOut {
		start = demoHookRun{fromEnd: true, from: startTimeout, run: jobs.HookRun{
			Name: hookStart, Phase: domain.HookAfter, Status: domain.HookTimedOut, ExitCode: -1, Duration: startTimeout,
			Message: "timed out after 2m0s",
			Output: "paperless-worker\nError response from daemon: driver failed programming external connectivity on endpoint paperless-web: " +
				"Bind for 0.0.0.0:8000 failed: port is already allocated",
		}}
		ping.from = startTimeout + pingTook
	}
	return []demoHookRun{
		{run: jobs.HookRun{
			Name: hookStop, Phase: domain.HookBefore, Status: domain.HookSucceeded, Duration: stopTook,
			Output: "paperless-web\npaperless-worker",
		}},
		{from: stopTook, run: jobs.HookRun{
			Name: hookDump, Phase: domain.HookBefore, Status: domain.HookSucceeded, Duration: dumpTook,
			Output: "pg_dump: dumping contents of table \"public.documents_document\"\n" +
				"pg_dump: dumping contents of table \"public.documents_note\"\n" +
				"wrote /srv/docs/db/paperless.dump (312 MiB)",
			OutputTruncated: true,
		}},
		ping,
		start,
	}
}

// hookRunsAt places a demo job's hook runs in its run from start to end.
func hookRunsAt(runs []demoHookRun, start, end time.Time) []jobs.HookRun {
	if len(runs) == 0 {
		return nil
	}
	out := make([]jobs.HookRun, len(runs))
	for i, r := range runs {
		out[i] = r.run
		out[i].StartedAt = start.Add(r.from)
		if r.fromEnd {
			out[i].StartedAt = end.Add(-r.from)
		}
	}
	return out
}
