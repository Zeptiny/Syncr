package devseed_test

import (
	"context"
	"slices"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/link"
)

// TestSeedMilestone7Probes: the demo workers of the current build list
// Milestone 7's scans and attic-nas, built before Milestone 6, does not.
// The scans have no canned answers yet: each fails as a worker reports an
// operation it cannot run, without breaking the demo.
func TestSeedMilestone7Probes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	s := e.seed(t)

	nas, err := e.deps.Workers.Get(ctx, s.Workers["nas-01"])
	if err != nil {
		t.Fatal(err)
	}
	attic, err := e.deps.Workers.Get(ctx, s.Workers["attic-nas"])
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range link.HeavyProbeOps() {
		if !slices.Contains(nas.Info.ProbeOps, op) || slices.Contains(attic.Info.ProbeOps, op) {
			t.Errorf("%s: nas-01 lists %v, attic-nas %v; want only nas-01 to list it", op, nas.Info.ProbeOps, attic.Info.ProbeOps)
		}
	}

	repo := &syncrv1.ResticRepository{Location: &syncrv1.Location{Remote: "r1", Path: "syncr-offsite/restic/photos"}, Password: "syncr-demo-restic"}
	for _, spec := range []*syncrv1.ProbeSpec{
		{Op: &syncrv1.ProbeSpec_MeasureLocal{MeasureLocal: &syncrv1.ProbeMeasureLocal{Path: "/srv/photos"}}},
		{Op: &syncrv1.ProbeSpec_MeasureRemote{MeasureRemote: &syncrv1.ProbeMeasureRemote{Location: &syncrv1.Location{Remote: "r1"}}}},
		{Op: &syncrv1.ProbeSpec_ResticMeasure{ResticMeasure: &syncrv1.ProbeResticMeasure{Repository: repo, SnapshotId: "1a2b3c4d", Path: "/"}}},
		{Op: &syncrv1.ProbeSpec_ResticFind{ResticFind: &syncrv1.ProbeResticFind{Repository: repo, Patterns: []string{"*.jpg"}}}},
		{Op: &syncrv1.ProbeSpec_ResticDiff{ResticDiff: &syncrv1.ProbeResticDiff{Repository: repo, FromSnapshotId: "1a2b3c4d", ToSnapshotId: "5e6f7a8b"}}},
	} {
		op := link.ProbeOpOf(spec)
		res, err := e.deps.Probes.Probe(ctx, s.Workers["nas-01"], &syncrv1.RunProbe{ProbeId: "scan", Spec: spec})
		if err != nil || res.GetProbeId() != "scan" || res.GetStatus() != syncrv1.ProbeStatus_PROBE_STATUS_FAILED ||
			res.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_UNSUPPORTED_KIND || res.GetOutput() != nil {
			t.Errorf("%s = %v (%v), want FAILED with UNSUPPORTED_KIND", op, res, err)
		}
	}
}
