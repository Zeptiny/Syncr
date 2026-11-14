package devseed

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
)

// jobPreset is what a scheduled run presets on its job: the ID it
// reserved and its schedule.
type jobPreset struct {
	id       domain.JobID
	schedule domain.ScheduleID
}

// seedJob runs one demo job, created at created, through its history (the
// past jobs service svc, whose clock clk it moves) and returns its ID. now
// is the seed's start.
func (s *Seed) seedJob(ctx context.Context, svc *jobs.Service, clk *stepClock, now, created time.Time, dj demoJob, preset jobPreset) (domain.JobID, error) {
	def, err := s.definition(dj)
	if err != nil {
		return domain.JobID{}, err
	}
	kind, refs, err := s.d.Kinds.Check(def)
	if err != nil {
		return domain.JobID{}, fmt.Errorf("check the definition: %w", err)
	}
	enc, err := jobkind.Marshal(def)
	if err != nil {
		return domain.JobID{}, err
	}
	workerID, ok := s.Workers[dj.worker]
	if !ok {
		return domain.JobID{}, fmt.Errorf("unknown demo worker %q", dj.worker)
	}
	actor := s.actor(dj.owner)
	epoch := epochOf(dj.worker)

	clk.set(created)
	j, err := svc.Create(ctx, actor, jobs.CreateParams{
		ID: preset.id, ScheduleID: preset.schedule,
		WorkerID: workerID, Kind: kind, Definition: enc, RemoteIDs: refs.RemoteIDs, SearchTerms: refs.SearchTerms,
		RepositoryID: refs.RepositoryID, ConcurrencyKeys: refs.ConcurrencyKeys,
	})
	if err != nil {
		return domain.JobID{}, err
	}
	id := j.ID
	start := created.Add(dj.queue)
	end := start.Add(dj.run)

	switch {
	case dj.status == domain.JobQueued:
		return id, nil
	case dj.status == domain.JobCancelled && dj.run == 0:
		clk.set(created.Add(2 * time.Minute))
		_, err = svc.Cancel(ctx, actor, id)
		return id, err
	case dj.err.Code == jobs.CodeDispatchTimeout, dj.err.Code == jobs.CodeToolUnavailable && dj.run == 0:
		// Failed by the controller before it was sent: the dispatch
		// timeout, or a worker that cannot use what the job needs.
		clk.set(start)
		_, _, err = svc.FailQueued(ctx, id, time.Time{}, dj.err)
		return id, err
	}

	clk.set(created.Add(dj.queue / 2))
	if _, _, err := svc.MarkDispatched(ctx, workerID, id, epoch); err != nil {
		return id, err
	}
	switch {
	case dj.status == domain.JobDispatched:
		return id, nil
	case dj.run == 0 && dj.status != domain.JobRunning:
		// Refused by the worker before it started anything.
		clk.set(start)
		_, _, err = svc.Finish(ctx, workerID, id, epoch, jobs.Outcome{Status: dj.status, Error: dj.err, ExitCode: dj.exitCode, FinishedAt: start})
		return id, err
	}

	clk.set(start)
	if _, _, err := svc.MarkRunning(ctx, workerID, id, epoch, start); err != nil {
		return id, err
	}
	if dj.status == domain.JobRunning {
		// Samples every 5 s up to now, as the worker would have sent them
		// (a job without a known total reports none).
		for at := start.Add(5 * time.Second); at.Before(now) && dj.stats.TotalBytes > 0; at = at.Add(5 * time.Second) {
			if err := s.sample(ctx, svc, clk, workerID, id, epoch, at, runningStats(dj.stats, at.Sub(start), cmp.Or(dj.rate, runningRate))); err != nil {
				return id, err
			}
		}
		return id, nil
	}
	every := max(5*time.Second, dj.run/time.Duration(cmp.Or(dj.samples, 60)))
	for at := start.Add(every); at.Before(end); at = at.Add(every) {
		if err := s.sample(ctx, svc, clk, workerID, id, epoch, at, partialStats(dj.stats, at.Sub(start), dj.run)); err != nil {
			return id, err
		}
	}

	clk.set(end)
	switch dj.status {
	case domain.JobInterrupted:
		// The controller ends it (worker restarted, lost); the last stored
		// sample has its counters.
		_, _, err = svc.Interrupt(ctx, workerID, id, epoch, dj.err)
		return id, err
	case domain.JobCancelled:
		clk.set(end.Add(-3 * time.Second))
		if _, err := svc.Cancel(ctx, actor, id); err != nil {
			return id, err
		}
		clk.set(end)
	}
	stats := dj.stats
	stats.Elapsed = dj.run
	var output []byte
	if dj.output != nil {
		if output, err = proto.Marshal(dj.output); err != nil {
			return id, err
		}
	}
	_, _, err = svc.Finish(ctx, workerID, id, epoch, jobs.Outcome{
		Status: dj.status, Error: dj.err, Stats: stats, Warnings: dj.warnings, ExitCode: dj.exitCode,
		StartedAt: start, FinishedAt: end, Output: output, HookRuns: hookRunsAt(dj.hookRuns, start, end),
	})
	return id, err
}

func (s *Seed) sample(ctx context.Context, svc *jobs.Service, clk *stepClock, w domain.WorkerID, id domain.JobID, epoch string, at time.Time, st jobs.Stats) error {
	clk.set(at)
	ok, err := svc.RecordProgress(ctx, w, id, epoch, jobs.Sample{Time: at, Stats: st})
	if err == nil && !ok {
		err = errors.New("progress was not accepted")
	}
	return err
}

// definition is the job definition of dj, over the seeded remotes and
// repositories, with its time limit.
func (s *Seed) definition(dj demoJob) (*syncrv1.JobDefinition, error) {
	def, err := s.operation(dj)
	if err != nil {
		return nil, err
	}
	if dj.maxRuntime > 0 {
		def.MaxRuntime = durationpb.New(dj.maxRuntime)
	}
	def.Hooks = dj.hooks
	return def, nil
}

// operation is the definition of dj without its time limit.
func (s *Seed) operation(dj demoJob) (*syncrv1.JobDefinition, error) {
	if dj.def != nil {
		return dj.def(s)
	}
	location := func(l loc) (*syncrv1.JobLocation, error) {
		if l.remote == "" {
			return &syncrv1.JobLocation{Path: l.path}, nil
		}
		id, ok := s.Remotes[l.remote]
		if !ok {
			return nil, fmt.Errorf("unknown demo remote %q", l.remote)
		}
		return &syncrv1.JobLocation{RemoteId: id.String(), Path: l.path}, nil
	}
	src, err := location(dj.src)
	if err != nil {
		return nil, err
	}
	dst, err := location(dj.dst)
	if err != nil {
		return nil, err
	}
	return &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_RcloneTransfer{RcloneTransfer: &syncrv1.RcloneTransferDefinition{
		Mode: dj.mode, Source: src, Destination: dst, Options: dj.options,
	}}}, nil
}

// ---------------------------------------------------------------------------
// Progress curves
// ---------------------------------------------------------------------------

// curve is the transferred amount after t seconds at a wavering speed: the
// integral of 1 + 0.3 sin(u/37) + 0.15 sin(u/9), whose slope stays above
// 0.5, so the curve only rises and the rate chart has a visible shape.
func curve(t float64) float64 {
	return t + 0.3*37*(1-math.Cos(t/37)) + 0.15*9*(1-math.Cos(t/9))
}

// partialStats are the counters of a job with final counters f after
// elapsed of its run.
func partialStats(f jobs.Stats, elapsed, run time.Duration) jobs.Stats {
	frac := curve(elapsed.Seconds()) / curve(run.Seconds())
	scale := func(n int64) int64 { return int64(float64(n) * frac) }
	total := f.TotalBytes
	if total == 0 {
		total = f.Bytes
	}
	return jobs.Stats{
		Bytes: scale(f.Bytes), TotalBytes: total, Files: scale(f.Files), TotalFiles: f.TotalFiles,
		Checks: scale(f.Checks), Deletes: scale(f.Deletes), Elapsed: elapsed,
	}
}

// runningRate is the average speed of the running job Animate moves, and
// the default of the other running jobs' stored samples (demoJob.rate).
const runningRate = 42 * mib

// runningStats are the counters of a running job (totals t) after elapsed
// at about rate bytes a second, never reaching the total.
func runningStats(t jobs.Stats, elapsed time.Duration, rate int64) jobs.Stats {
	bytes := min(int64(curve(elapsed.Seconds())*float64(rate)), t.TotalBytes-1)
	files := int64(float64(bytes) / float64(t.TotalBytes) * float64(t.TotalFiles))
	return jobs.Stats{
		Bytes: bytes, TotalBytes: t.TotalBytes, Files: files, TotalFiles: t.TotalFiles,
		Checks: min(t.TotalFiles, files*3+int64(elapsed/time.Second)*12), Elapsed: elapsed,
	}
}

// ---------------------------------------------------------------------------
// Animation
// ---------------------------------------------------------------------------

// AnimateInterval is how often Animate reports progress.
const AnimateInterval = time.Second

// Animate keeps the demo alive until ctx ends: every AnimateInterval it
// marks the online demo workers as seen and records the running job's
// progress through the controller's job service (live watchers see it; a
// sample is stored every few seconds). Errors are logged.
func (s *Seed) Animate(ctx context.Context) {
	t := time.NewTicker(AnimateInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Step(ctx); err != nil && ctx.Err() == nil {
				s.log.WarnContext(ctx, "devseed: animation step failed", "error", err.Error())
			}
		}
	}
}

// Step performs one animation step at the clock's time (see Animate).
func (s *Seed) Step(ctx context.Context) error {
	var errs []error
	for _, id := range s.online {
		errs = append(errs, s.d.Workers.Touch(ctx, id))
	}
	at := s.d.Clock.Now()
	var totals jobs.Stats
	for _, dj := range demoJobs() {
		if dj.status == domain.JobRunning {
			totals = dj.stats
		}
	}
	if at.After(s.runningStart) {
		smp := jobs.Sample{Time: at, Stats: runningStats(totals, at.Sub(s.runningStart), runningRate)}
		// false once the job is no longer running (someone cancelled it).
		_, err := s.d.Jobs.RecordProgress(ctx, s.runningWorker, s.Running, s.runningEpoch, smp)
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// ---------------------------------------------------------------------------
// Seed-only collaborators of the past jobs' service
// ---------------------------------------------------------------------------

// stepClock is a clock the seed sets to each moment of a job's history.
type stepClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *stepClock) set(t time.Time) {
	c.mu.Lock()
	c.now = t
	c.mu.Unlock()
}

// seedIDs mints time-ordered UUIDv7 IDs of every kind the seed's own
// services create (jobs, schedules, runs, deliveries, repositories): the clock's time and
// fixed "random" bits derived from a counter, so IDs order like the
// objects' creation times and two seeds at the same time mint the same IDs.
type seedIDs struct {
	clock *stepClock
	mu    sync.Mutex
	n     int
}

// next returns the next ID in canonical text form.
func (g *seedIDs) next(kind string) string {
	g.mu.Lock()
	g.n++
	n := g.n
	g.mu.Unlock()
	var u [16]byte
	binary.BigEndian.PutUint64(u[:8], uint64(g.clock.Now().UnixMilli())<<16)
	sum := sha256.Sum256(fmt.Appendf(nil, "syncr devseed %s %d", kind, n))
	copy(u[6:], sum[:10])
	u[6] = 0x70 | u[6]&0x0f // version 7
	u[8] = 0x80 | u[8]&0x3f // RFC 9562 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// mustID parses an ID next minted; the bytes always form a valid UUID.
func mustID[T any](id T, err error) T {
	if err != nil {
		panic(err)
	}
	return id
}

func (g *seedIDs) NewJobID() domain.JobID { return mustID(domain.ParseJobID(g.next("job"))) }

func (g *seedIDs) NewResticRepoID() domain.ResticRepoID {
	return mustID(domain.ParseResticRepoID(g.next("repository")))
}

func (g *seedIDs) NewScheduleID() domain.ScheduleID {
	return mustID(domain.ParseScheduleID(g.next("schedule")))
}

func (g *seedIDs) NewScheduleRunID() domain.ScheduleRunID {
	return mustID(domain.ParseScheduleRunID(g.next("run")))
}

func (g *seedIDs) NewDeliveryID() domain.DeliveryID {
	return mustID(domain.ParseDeliveryID(g.next("delivery")))
}

// nopBroker drops the past jobs' updates: nobody watches them while the
// seed runs.
type nopBroker struct{}

func (nopBroker) Publish(jobs.Update) {}

func (nopBroker) Subscribe(domain.JobID) jobs.Subscription { return nopSubscription{} }

type nopSubscription struct{}

func (nopSubscription) Next(ctx context.Context) (jobs.Update, error) {
	<-ctx.Done()
	return jobs.Update{}, ctx.Err()
}

func (nopSubscription) Close() {}

// nopWaker wakes nobody: the demo workers do not poll.
type nopWaker struct{}

func (nopWaker) Wake(domain.WorkerID) {}
