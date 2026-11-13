package restic

import (
	"context"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// The Milestone 7 probe operations walk a whole snapshot folder or
// repository: restic_measure, restic_find and restic_diff. Each reads with
// --no-lock, like Ls. When ctx's deadline passes (the executor's soft
// deadline) each stops restic and returns what it found with
// complete=false and no error; when ctx is cancelled it fails.
//
// Not implemented yet: they fail with UNSUPPORTED_KIND, and the executor
// does not advertise them.

// Measure measures a folder of a snapshot (ls --json --recursive -- <id>
// <dir>): the totals of everything below op.path and of each direct child
// folder, by name, at most maxChildren of them; entries it cannot read are
// counted (unreadable), not fatal.
func (p *Prober) Measure(ctx context.Context, _ string, _ []*syncrv1.RemoteDefinition, _ *syncrv1.ProbeResticMeasure, _ int) (*syncrv1.ProbeMeasurement, error) {
	return nil, notYet(ctx, "measure a snapshot's folders")
}

// Find searches the snapshots (find --json [--ignore-case] [--snapshot
// <id>]... -- <pattern>...): at most op.max_matches matches.
func (p *Prober) Find(ctx context.Context, _ string, _ []*syncrv1.RemoteDefinition, _ *syncrv1.ProbeResticFind) (*syncrv1.ResticFindOutput, error) {
	return nil, notYet(ctx, "search snapshots")
}

// Diff compares two snapshots (diff --json -- <from>[:<path>]
// <to>[:<path>]): at most op.max_changes changes, and restic's statistics
// when it finished.
func (p *Prober) Diff(ctx context.Context, _ string, _ []*syncrv1.RemoteDefinition, _ *syncrv1.ProbeResticDiff) (*syncrv1.ResticDiffOutput, error) {
	return nil, notYet(ctx, "compare snapshots")
}

// notYet is the failure of an operation this build does not run: ctx's
// error if it ended, UNSUPPORTED_KIND otherwise.
func notYet(ctx context.Context, what string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_UNSUPPORTED_KIND, "this worker cannot %s yet", what)
}
