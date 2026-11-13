package rclone

import (
	"context"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// Measure measures op.location (lsjson -R --no-mimetype --no-modtime,
// read line by line): the totals of everything below it and of each
// direct child folder, by name, at most maxChildren of them; entries it
// cannot read are counted (unreadable), not fatal. When ctx's deadline
// passes (the executor's soft deadline) it stops rclone and returns what
// it found with complete=false and no error; when ctx is cancelled it
// fails.
//
// Not implemented yet: it fails with UNSUPPORTED_KIND, and the executor
// does not advertise measure_remote.
func (p *Prober) Measure(ctx context.Context, _ string, _ []*syncrv1.RemoteDefinition, _ *syncrv1.ProbeMeasureRemote, _ int) (*syncrv1.ProbeMeasurement, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_UNSUPPORTED_KIND, "this worker cannot measure a remote's folders yet")
}
