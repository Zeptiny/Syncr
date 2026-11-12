package devseed_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
	"github.com/Zeptiny/Syncr/internal/link"
)

// Names and messages of the two-way sync demo that the web UI's
// screenshots rely on.
const (
	sharedSchedule = "Shared folder ⇄ R2"
	sharedRemote   = "Cloudflare R2"
	sharedTitle    = "Two-way sync /srv/shared ⇄ Cloudflare R2"
	msgSide        = "Folder A (path1) is empty; refusing to sync it with the other side (is the drive mounted and the path right?)"
	msgResync      = "a resync is required: failed to open source object: open /srv/shared/scans/2026-11-contract.pdf: permission denied; " +
		"run it once with resync to rebuild the pair's state from both sides as they are now"
	msgSafety = `safety check stopped the two-way sync: too many deletes (>50%, 731 of 1268) on Path1 "/srv/shared/"; ` +
		"if these changes are intended, run it once with force"
	msgConflicts = `2 files changed on both sides; both versions were kept, renamed with the suffix "conflict"`
)

// bisyncOutput decodes a two-way sync job's stored output.
func bisyncOutput(t *testing.T, j jobs.Job) *syncrv1.RcloneBisyncOutput {
	t.Helper()
	if len(j.Output) == 0 {
		return nil
	}
	out := new(syncrv1.JobOutput)
	if err := proto.Unmarshal(j.Output, out); err != nil {
		t.Fatal(err)
	}
	return out.GetRcloneBisync()
}

// TestSeedTwoWaySync: the two-way sync's schedule on nas-01 and its runs
// show each of the kind's states, and the resync run by hand follows the
// run that needed it.
func TestSeedTwoWaySync(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	s := e.seed(t)

	r2, err := e.deps.Remotes.Get(ctx, s.Admin, s.Remotes[sharedRemote])
	if err != nil || r2.Type != "s3" || r2.Options["provider"] != "Cloudflare" || r2.Options["bucket"] != "ada-shared" {
		t.Fatalf("%s = %+v (%v)", sharedRemote, r2, err)
	}
	sc, err := e.store.GetSchedule(ctx, s.Schedules[sharedSchedule])
	if err != nil || !sc.Enabled || sc.Target.WorkerID != s.Workers["nas-01"] || sc.Target.Label != "" || sc.Kind != "rclone_bisync" ||
		!sc.NextRunAt.After(t0) {
		t.Fatalf("%s = %+v (%v)", sharedSchedule, sc, err)
	}
	def, err := jobkind.Unmarshal(sc.Definition)
	if err != nil || def.GetRcloneBisync().GetResync() || def.GetRcloneBisync().GetForce() ||
		def.GetRcloneBisync().GetPath2().GetRemoteId() != r2.ID.String() {
		t.Fatalf("its definition = %v (%v)", def, err)
	}
	if err := e.deps.Kinds.CheckSchedulable(def); err != nil {
		t.Errorf("its definition cannot be scheduled: %v", err)
	}
	if got := e.deps.Kinds.DescribeOperation(def, jobkind.Names{Remotes: map[domain.RemoteID]string{r2.ID: sharedRemote}}); got != sharedTitle {
		t.Errorf("Describe = %q, want %q", got, sharedTitle)
	}

	page, err := e.deps.Jobs.List(ctx, s.Admin, jobs.ListFilter{ScheduleID: sc.ID, Limit: jobs.MaxPageSize})
	if err != nil || len(page.Jobs) < 30 || len(page.Jobs) > 45 {
		t.Fatalf("%d scheduled runs (%v), want about 38", len(page.Jobs), err)
	}
	runs := slices.Clone(page.Jobs)
	slices.Reverse(runs) // oldest first
	var keys []string
	counts := map[string]int{}
	var resyncRun jobs.Job
	for i, j := range runs {
		if j.Kind != "rclone_bisync" || j.WorkerID != s.Workers["nas-01"] || len(j.ConcurrencyKeys) != 2 {
			t.Fatalf("run %d = %+v", i, j)
		}
		if keys == nil {
			keys = j.ConcurrencyKeys
		} else if !slices.Equal(j.ConcurrencyKeys, keys) {
			t.Errorf("run %d holds the keys %q, the first %q", i, j.ConcurrencyKeys, keys)
		}
		out := bisyncOutput(t, j)
		switch {
		case j.Status == domain.JobSucceeded && out == nil:
			t.Errorf("run %d succeeded without an output", i)
		case j.Status == domain.JobFailed:
			counts[j.Error.Code]++
			if out != nil {
				t.Errorf("failed run %d has an output", i)
			}
			want := map[string]struct {
				msg  string
				exit int
			}{
				link.ErrorCodeName(syncrv1.JobErrorCode_JOB_ERROR_CODE_SIDE_UNAVAILABLE): {msgSide, 7},
				link.ErrorCodeName(syncrv1.JobErrorCode_JOB_ERROR_CODE_RESYNC_REQUIRED):  {msgResync, 7},
				link.ErrorCodeName(syncrv1.JobErrorCode_JOB_ERROR_CODE_SAFETY_ABORT):     {msgSafety, 1},
			}[j.Error.Code]
			if want.msg == "" || j.Error.Message != want.msg || j.ExitCode != want.exit {
				t.Errorf("failed run %d: %+v, exit %d", i, j.Error, j.ExitCode)
			}
			if j.Error.Code == "resync_required" {
				resyncRun = j
			}
		case out.GetResyncReason() == syncrv1.BisyncResyncReason_BISYNC_RESYNC_REASON_FIRST_RUN:
			counts["first_run"]++
			if i != 0 {
				t.Errorf("run %d is a first-run resync", i)
			}
		case out.GetConflicts() > 0:
			counts["conflicts"]++
			if out.GetConflicts() != 2 || !slices.Equal(out.GetConflictPaths(), []string{"budget-2026.xlsx", "notes/meeting-notes.md"}) ||
				!slices.Equal(j.Warnings, []string{msgConflicts}) {
				t.Errorf("run %d with conflicts: %v, warnings %q", i, out, j.Warnings)
			}
		}
	}
	if counts["first_run"] != 1 || counts["conflicts"] != 1 || counts["side_unavailable"] != 1 || counts["resync_required"] != 1 ||
		counts["safety_abort"] < 1 || counts["safety_abort"] > 2 {
		t.Errorf("runs by state = %v", counts)
	}
	if last := runs[len(runs)-1]; last.Error.Code != "safety_abort" {
		t.Errorf("the newest run ended %s %+v, want safety_abort", last.Status, last.Error)
	}
	if got := e.deps.Kinds.Summarize("rclone_bisync", def, runs[0].Output); !slices.Equal(got, []jobkind.Detail{{Label: "Resync", Value: "First run"}}) {
		t.Errorf("the first run's details = %+v", got)
	}

	// The resync run by hand: after the run that needed it, before the next.
	all, err := e.deps.Jobs.List(ctx, s.Admin, jobs.ListFilter{Kinds: []string{"rclone_bisync"}, Limit: jobs.MaxPageSize})
	if err != nil {
		t.Fatal(err)
	}
	var manual []jobs.Job
	for _, j := range all.Jobs {
		if j.ScheduleID.IsZero() {
			manual = append(manual, j)
		}
	}
	if len(manual) != 1 {
		t.Fatalf("%d two-way sync jobs run by hand, want 1", len(manual))
	}
	resync := manual[0]
	mdef, err := jobkind.Unmarshal(resync.Definition)
	if err != nil || !mdef.GetRcloneBisync().GetResync() || mdef.GetRcloneBisync().GetForce() || resync.Status != domain.JobSucceeded ||
		bisyncOutput(t, resync).GetResyncReason() != syncrv1.BisyncResyncReason_BISYNC_RESYNC_REASON_REQUESTED ||
		!slices.Equal(resync.ConcurrencyKeys, keys) {
		t.Fatalf("the resync = %+v (%v)", resync, err)
	}
	if got := e.deps.Kinds.DescribeOperation(mdef, jobkind.Names{Remotes: map[domain.RemoteID]string{r2.ID: sharedRemote}}); got != sharedTitle+" · Resync" {
		t.Errorf("the resync's title = %q", got)
	}
	if resyncRun.ID.IsZero() || !resync.CreatedAt.After(resyncRun.FinishedAt) {
		t.Errorf("the resync was created at %v, the run that needed it finished at %v", resync.CreatedAt, resyncRun.FinishedAt)
	}
	for _, j := range runs {
		if j.CreatedAt.After(resyncRun.CreatedAt) && j.CreatedAt.Before(resync.FinishedAt) {
			t.Errorf("run %s started between the failed run and the resync", j.ID)
		}
	}

	// nas-01 runs two-way syncs; attic-nas, a build from before them, does
	// not.
	nas, err := e.deps.Workers.Get(ctx, s.Workers["nas-01"])
	if err != nil || !slices.Contains(nas.Info.JobKinds, "rclone_bisync") || !nas.Info.HasFeatures(link.FeatureRcloneBisync) {
		t.Errorf("nas-01 = %+v (%v)", nas.Info, err)
	}
	attic, err := e.deps.Workers.Get(ctx, s.Workers["attic-nas"])
	if err != nil || slices.Contains(attic.Info.JobKinds, "rclone_bisync") || attic.Info.HasFeatures(link.FeatureRcloneBisync) ||
		!slices.Contains(attic.Info.JobKinds, "rclone_transfer") {
		t.Errorf("attic-nas = %+v (%v)", attic.Info, err)
	}

	// Both sides list the shared folder, with the conflicts' versions.
	list := func(spec *syncrv1.ProbeSpec) []string {
		t.Helper()
		res, err := e.deps.Probes.Probe(ctx, s.Workers["nas-01"], &syncrv1.RunProbe{ProbeId: "p", Spec: spec})
		if err != nil || res.GetStatus() != syncrv1.ProbeStatus_PROBE_STATUS_OK {
			t.Fatalf("probe %v = %v (%v)", spec, res, err)
		}
		var names []string
		for _, en := range res.GetListing().GetEntries() {
			names = append(names, en.GetName())
		}
		return names
	}
	wantNotes := []string{"meeting-notes.md", "meeting-notes.md.conflict1", "meeting-notes.md.conflict2", "packing-list.md"}
	if got := list(&syncrv1.ProbeSpec{Op: &syncrv1.ProbeSpec_ListLocal{ListLocal: &syncrv1.ProbeListLocal{Path: "/srv/shared/notes"}}}); !slices.Equal(got, wantNotes) {
		t.Errorf("/srv/shared/notes = %v", got)
	}
	r2def := []*syncrv1.RemoteDefinition{{Alias: "r1", BackendType: "s3", Options: map[string]string{"provider": "Cloudflare"}}}
	if got := list(&syncrv1.ProbeSpec{Remotes: r2def, Op: &syncrv1.ProbeSpec_ListRemote{ListRemote: &syncrv1.ProbeListRemote{
		Location: &syncrv1.Location{Remote: "r1", Path: "ada-shared/shared/notes"},
	}}}); !slices.Equal(got, wantNotes) {
		t.Errorf("R2 shared/notes = %v", got)
	}
	if strings.Contains(e.logs.String(), "syncr-demo-r2-secret") {
		t.Error("the log contains the R2 secret")
	}
}
