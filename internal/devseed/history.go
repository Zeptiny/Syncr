package devseed

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/notifications"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/cronexpr"
	"github.com/Zeptiny/Syncr/internal/descriptor"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
	"github.com/Zeptiny/Syncr/internal/notify"
)

// history replays about a month of the demo controller's life in time
// order, with services of its own whose clock it moves (jobs, schedules,
// the notification outbox): the demo schedules fire, their runs launch,
// wait, merge, are missed or fail as their outcome functions say, the
// manual demo jobs run, and every outcome is planned and delivered by the
// real outbox code, through scripted copies of the channels (which never
// send). The scheduler itself is controller code the seed may not use; the
// history makes the same calls it makes (FireDue, MarkLaunching, jobs
// Create with the reserved ID, MarkLaunched, EndRun).
type history struct {
	s      *Seed
	now    time.Time
	clk    *stepClock
	ids    *seedIDs
	jobs   *jobs.Service
	sched  *schedules.Service
	outbox *notifications.Service
	script *script

	scheds []*seededSchedule
	byID   map[domain.ScheduleID]*seededSchedule
	// runN is each run's index in its schedule's history.
	runN map[domain.ScheduleRunID]int
	// removed are the workers removed so far.
	removed map[domain.WorkerID]bool
	running int
	// lastJob is the newest job's creation time: jobs are created at
	// least a second apart, so they list in seed order.
	lastJob time.Time
}

// jobTime returns at, or a second after the newest job if that is later.
func (h *history) jobTime(at time.Time) time.Time {
	if !h.lastJob.IsZero() && !at.After(h.lastJob) {
		at = h.lastJob.Add(time.Second)
	}
	h.lastJob = at
	return at
}

// seededSchedule is a demo schedule and its state during the history.
type seededSchedule struct {
	demo    demoSchedule
	id      domain.ScheduleID
	expr    *cronexpr.Expr
	created time.Time
	runs    int
}

// event is one moment of the history.
type event struct {
	at time.Time
	// order breaks ties: lower first.
	order int
	do    func(ctx context.Context) error
}

func (s *Seed) seedHistory(ctx context.Context, now time.Time) error {
	clk := &stepClock{now: now}
	ids := &seedIDs{clock: clk}
	h := &history{
		s: s, now: now, clk: clk, ids: ids, script: newScript(now),
		byID: map[domain.ScheduleID]*seededSchedule{}, runN: map[domain.ScheduleRunID]int{}, removed: map[domain.WorkerID]bool{},
	}
	var err error
	if h.jobs, err = jobs.New(jobs.Deps{
		Jobs: s.d.Store, Samples: s.d.Store, Clock: clk, IDs: ids,
		Broker: nopBroker{}, Notifier: jobs.NopNotifier{}, Waker: nopWaker{}, Remotes: s.d.Remotes,
		Repositories: storeRepos{s.d.Store}, TimeLimits: s.d.TimeLimits,
	}, jobs.Config{}); err != nil {
		return err
	}
	if h.sched, err = schedules.New(schedules.Deps{
		Repo: s.d.Store, Remotes: s.d.Remotes, Contacts: s.d.Contacts, Workers: workerCheck{s.d.Workers},
		Repositories: storeRepos{s.d.Store},
		Waker:        nopScheduleWaker{}, Clock: clk, IDs: ids,
	}, schedules.Config{}); err != nil {
		return err
	}
	channels, err := h.script.registry(s.d.Channels)
	if err != nil {
		return err
	}
	if h.outbox, err = notifications.New(notifications.Deps{
		Repo: s.d.Store, Channels: channels, Contacts: s.d.Contacts, Jobs: s.d.Describer, Clock: clk, IDs: ids, Random: zeroReader{},
	}, notifications.Config{Concurrency: 1}); err != nil {
		return err
	}

	events, err := h.events()
	if err != nil {
		return err
	}
	slices.SortStableFunc(events, func(a, b event) int {
		return cmp.Or(a.at.Compare(b.at), cmp.Compare(a.order, b.order))
	})
	for _, e := range events {
		if err := e.do(ctx); err != nil {
			return fmt.Errorf("at %s: %w", e.at.Format(time.RFC3339), err)
		}
	}
	if h.running != 1 {
		return fmt.Errorf("%d running demo jobs, want exactly one", h.running)
	}
	return h.finish(ctx)
}

// Event orders at one instant.
const (
	orderCreate = iota
	orderChange
	orderTick
	orderJob
)

// events lists the history: schedule creations, their ticks (without the
// controller's downtime, plus the tick that restarted it), pauses, Run
// now, the worker removal and the manual jobs, and a final tick at the
// seed's start.
func (h *history) events() ([]event, error) {
	var out []event
	ticks := map[time.Time]bool{}
	downFrom, downUntil := controllerDown(h.now)
	for _, d := range slices.Concat(demoSchedules(h.now), demoResticSchedules(h.now), demoBisyncSchedules(h.now)) {
		e, err := cronexpr.Parse(d.cron, d.timezone)
		if err != nil {
			return nil, fmt.Errorf("schedule %q: %w", d.name, err)
		}
		ss := &seededSchedule{demo: d, expr: e, created: h.now.Add(-d.age)}
		h.scheds = append(h.scheds, ss)
		out = append(out, event{at: ss.created, order: orderCreate, do: func(ctx context.Context) error { return h.create(ctx, ss) }})
		for t := e.Next(ss.created); t.Before(h.now); t = e.Next(t) {
			if !t.Before(downFrom) && t.Before(downUntil) {
				continue
			}
			ticks[t.UTC()] = true
		}
		if d.pausedAgo > 0 {
			out = append(out, event{at: h.now.Add(-d.pausedAgo), order: orderChange, do: func(ctx context.Context) error {
				h.clk.set(h.now.Add(-d.pausedAgo))
				_, err := h.sched.SetEnabled(ctx, h.s.actor(d.owner), ss.id, false)
				return err
			}})
		}
		if d.manualAgo > 0 {
			at := h.now.Add(-d.manualAgo)
			out = append(out, event{at: at, order: orderChange, do: func(ctx context.Context) error {
				h.clk.set(at)
				if _, err := h.sched.RunNow(ctx, h.s.actor(d.owner), ss.id); err != nil {
					return err
				}
				return h.launchOpen(ctx, at)
			}})
		}
	}
	ticks[downUntil.UTC()] = true
	for t := range ticks {
		out = append(out, event{at: t, order: orderTick, do: func(ctx context.Context) error { return h.tick(ctx, t) }})
	}
	for _, w := range demoWorkers() {
		if w.removedAgo == 0 {
			continue
		}
		out = append(out, event{at: h.now.Add(-w.removedAgo), order: orderChange, do: func(ctx context.Context) error {
			id := h.s.Workers[w.name]
			h.removed[id] = true
			return h.s.d.Workers.Remove(ctx, id)
		}})
	}
	for i, dj := range slices.Concat(demoJobs(), demoResticJobs(), demoBisyncJobs(h.now)) {
		at := h.now.Add(-dj.created)
		out = append(out, event{at: at, order: orderJob, do: func(ctx context.Context) error {
			if err := h.manualJob(ctx, dj, at); err != nil {
				return fmt.Errorf("job %d (%s on %s): %w", i, dj.status, dj.worker, err)
			}
			return nil
		}})
	}
	// Data deletions: one finished days ago; one requested just now, which
	// waits for the check running on its repository.
	out = append(out,
		event{at: h.now.Add(-retiredDeletedAgo), order: orderJob, do: func(ctx context.Context) error {
			return h.deleteData(ctx, repoRetired, "nas-01", h.now.Add(-retiredDeletedAgo), true, 4*time.Minute+10*time.Second)
		}},
		event{at: h.now.Add(-oldLaptopDeleteAgo), order: orderJob, do: func(ctx context.Context) error {
			return h.deleteData(ctx, repoOldLaptop, "nas-01", h.now.Add(-oldLaptopDeleteAgo), false, 0)
		}},
	)
	return out, nil
}

// create creates a demo schedule as its owner at its creation time.
func (h *history) create(ctx context.Context, ss *seededSchedule) error {
	d := ss.demo
	h.clk.set(ss.created)
	def, err := h.s.definition(d.job)
	if err != nil {
		return err
	}
	kind, refs, err := h.s.d.Kinds.Check(def)
	if err != nil {
		return fmt.Errorf("schedule %q: %w", d.name, err)
	}
	enc, err := jobkind.Marshal(def)
	if err != nil {
		return err
	}
	in := schedules.Input{
		Name: d.name, Description: d.description, Cron: d.cron, Timezone: d.timezone, Missed: d.missed, Overlap: d.overlap,
		Enabled: true, Kind: kind, Definition: enc, RemoteIDs: refs.RemoteIDs, SearchTerms: refs.SearchTerms,
		RepositoryID: refs.RepositoryID,
		Target:       schedules.Target{Label: d.label},
	}
	if d.worker != "" {
		in.Target = schedules.Target{WorkerID: h.s.Workers[d.worker]}
	}
	for _, l := range d.links {
		id, ok := h.s.Contacts[l.contact]
		if !ok {
			return fmt.Errorf("schedule %q: unknown demo contact %q", d.name, l.contact)
		}
		in.Contacts = append(in.Contacts, schedules.ContactLink{ContactID: id, Events: domain.EventSet(l.events).Normalize()})
	}
	sc, err := h.sched.Create(ctx, h.s.actor(d.owner), in)
	if err != nil {
		return fmt.Errorf("schedule %q: %w", d.name, err)
	}
	ss.id = sc.ID
	h.byID[sc.ID] = ss
	h.s.Schedules[d.name] = sc.ID
	return nil
}

// tick is one scheduler pass at at: fire what is due, then decide the open
// runs.
func (h *history) tick(ctx context.Context, at time.Time) error {
	h.clk.set(at)
	if _, err := h.sched.FireDue(ctx); err != nil {
		return err
	}
	return h.launchOpen(ctx, at)
}

// launchOpen decides every open run at at, oldest first; launches start a
// few seconds apart, as the scheduler creates their jobs one by one.
func (h *history) launchOpen(ctx context.Context, at time.Time) error {
	runs, err := h.sched.OpenRuns(ctx, -1)
	if err != nil {
		return err
	}
	launched := 0
	for _, r := range runs {
		ss, ok := h.byID[r.ScheduleID]
		if !ok {
			continue
		}
		n, seen := h.runN[r.ID]
		if !seen {
			n = ss.runs
			ss.runs++
			h.runN[r.ID] = n
		}
		start := at.Add(time.Duration(launched) * 3 * time.Second)
		ok, err := h.decide(ctx, ss, r, n, start)
		if err != nil {
			return fmt.Errorf("schedule %q run %d: %w", ss.demo.name, n, err)
		}
		if ok {
			launched++
		}
	}
	return nil
}

// decide handles one open run at at (as the scheduler's decide does) and
// reports whether it launched.
func (h *history) decide(ctx context.Context, ss *seededSchedule, r schedules.Run, n int, at time.Time) (bool, error) {
	d := ss.demo
	if at.After(r.Deadline) {
		// Past its deadline already (the controller was down).
		return false, h.end(ctx, r, at, schedules.RunMissed, schedules.ReasonWorkerOffline, "no worker took the run before its deadline")
	}
	var workerName string
	switch {
	case d.worker != "":
		if h.removed[h.s.Workers[d.worker]] {
			h.clk.set(at)
			_, err := h.sched.EndRun(ctx, r, schedules.RunFailed, schedules.ReasonWorkerRemoved,
				"the schedule's worker was removed; choose another worker and resume the schedule", true)
			if err == nil {
				err = h.notify(ctx)
			}
			return false, err
		}
		workerName = d.worker
	default:
		workerName = labelWorker(d.label)
	}
	o := d.outcome(n, at)
	// A job must have ended before the seed's start (only the manual demo
	// job is running); a run too close to it is left to the controller.
	if o.action == launch {
		latest := h.now.Add(-30 * time.Second)
		if !at.Add(o.run).Before(latest) {
			o.run = latest.Sub(at)
		}
		if o.run < 5*time.Second {
			return false, nil
		}
	}
	if o.action == wait {
		if next := ss.expr.Next(at); !next.After(r.Deadline) && !next.IsZero() {
			return false, nil // the next fire merges into it and moves its deadline
		}
		reason := cmp.Or(o.missedReason, schedules.ReasonWorkerOffline)
		msg := "the worker was offline until the run's deadline"
		if reason == schedules.ReasonOverlap {
			msg = "the previous run was still active at the run's deadline"
		}
		return false, h.end(ctx, r, r.Deadline.Add(10*time.Second), schedules.RunMissed, reason, msg)
	}

	at = h.jobTime(at)
	h.clk.set(at)
	workerID := h.s.Workers[workerName]
	jobID := h.ids.NewJobID()
	r, err := h.sched.MarkLaunching(ctx, r, jobID, workerID)
	if err != nil {
		return false, err
	}
	dj := d.job
	dj.worker, dj.status, dj.queue, dj.run = workerName, o.status, 2*time.Second, o.run
	dj.stats, dj.err, dj.exitCode, dj.warnings, dj.samples, dj.output = o.stats, o.err, o.exitCode, o.warnings, 6, o.output
	h.script.onScheduledJob(ss.demo.name, at, o.status, jobID)
	id, err := h.s.seedJob(ctx, h.jobs, h.clk, h.now, at, dj, jobPreset{id: jobID, schedule: ss.id})
	if err != nil {
		return false, err
	}
	h.s.Jobs = append(h.s.Jobs, id)
	h.clk.set(at)
	if _, err := h.sched.MarkLaunched(ctx, r); err != nil {
		return false, err
	}
	h.clk.set(at.Add(dj.queue + dj.run))
	return true, h.notify(ctx)
}

// end ends an open run at at, then plans and delivers its notifications.
func (h *history) end(ctx context.Context, r schedules.Run, at time.Time, status schedules.RunStatus, reason, msg string) error {
	h.clk.set(at)
	if _, err := h.sched.EndRun(ctx, r, status, reason, msg, false); err != nil {
		return err
	}
	return h.notify(ctx)
}

// labelWorker is the worker the demo scheduler picks for a label.
func labelWorker(label string) string {
	if label == "offsite" {
		return "backup-host"
	}
	return "nas-01"
}

// manualJob runs a demo job started by hand at at.
func (h *history) manualJob(ctx context.Context, dj demoJob, at time.Time) error {
	at = h.jobTime(at)
	id, err := h.s.seedJob(ctx, h.jobs, h.clk, h.now, at, dj, jobPreset{})
	if err != nil {
		return err
	}
	h.s.Jobs = append(h.s.Jobs, id)
	h.script.onManualJob(dj, id)
	if dj.status == domain.JobRunning && !dj.static {
		h.running++
		h.s.Running = id
		h.s.runningWorker = h.s.Workers[dj.worker]
		h.s.runningEpoch = epochOf(dj.worker)
		h.s.runningStart = at.Add(dj.queue)
	}
	if !dj.status.Terminal() {
		return nil
	}
	h.clk.set(at.Add(dj.queue + dj.run))
	if dj.created == sendingJobAgo {
		// Its email is being sent as the seed ends: planned and claimed,
		// with a lease longer than a screenshot session.
		if _, err := h.outbox.Plan(ctx); err != nil {
			return err
		}
		claimed, err := h.s.d.Store.ClaimDeliveries(ctx, h.clk.Now(), time.Hour, 1)
		if err != nil {
			return err
		}
		if len(claimed) != 1 || claimed[0].JobID != id {
			return fmt.Errorf("claimed %d deliveries for the sending demo, want the job's one", len(claimed))
		}
		return nil
	}
	return h.notify(ctx)
}

// notify plans the outcomes recorded so far and delivers what is due, at
// the clock's time.
func (h *history) notify(ctx context.Context) error {
	if _, err := h.outbox.Plan(ctx); err != nil {
		return err
	}
	_, err := h.outbox.DeliverDue(ctx)
	return err
}

// finish ends the history at the seed's start: a last scheduler pass and
// outbox pass, then one dead delivery is queued again for later (a pending
// delivery that was never attempted since).
func (h *history) finish(ctx context.Context) error {
	if err := h.tick(ctx, h.now); err != nil {
		return err
	}
	h.clk.set(h.now)
	if err := h.notify(ctx); err != nil {
		return err
	}
	id, ok := h.script.requeue()
	if !ok {
		return nil
	}
	ds, err := h.s.d.Store.ListDeliveries(ctx, notifications.DeliveryFilter{
		ContactID: h.s.Contacts[cHA], Statuses: []notifications.Status{notifications.StatusDead}, Limit: -1,
	})
	if err != nil {
		return err
	}
	for _, d := range ds {
		if d.JobID == id {
			_, err = h.s.d.Store.RetryDelivery(ctx, d.ID, h.now.Add(requeueIn))
			return err
		}
	}
	return errors.New("the scripted dead delivery is missing")
}

// requeueIn is when the queued-again dead delivery is next attempted.
const requeueIn = 45 * time.Minute

// ---------------------------------------------------------------------------
// Scripted channels
// ---------------------------------------------------------------------------

// script decides what the history's channels answer: every send succeeds
// (without leaving the process) except the failures it scripted for some
// jobs: the newest failed manual job's email is refused for now (the
// delivery retries) and its webhook is gone (dead); the webhook of the
// nightly run two days back is gone too (that dead delivery is queued
// again at the end, a pending delivery never attempted since).
type script struct {
	now      time.Time
	mu       sync.Mutex
	failures map[string]map[string]*notify.SendError // job ID -> channel -> error
	requeued domain.JobID
}

func newScript(now time.Time) *script {
	return &script{now: now, failures: map[string]map[string]*notify.SendError{}}
}

// Scripted failures.
var (
	errMailBusy = &notify.SendError{Status: 421, RetryAfter: time.Hour,
		Reason: "SMTP 421 4.7.0 smtp.example.net: too many concurrent connections, try again later"}
	errHookGone = &notify.SendError{Permanent: true, Status: 410, Reason: "HTTP 410 Gone: the webhook does not exist"}
)

func (sc *script) fail(job domain.JobID, channel string, err *notify.SendError) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	m := sc.failures[job.String()]
	if m == nil {
		m = map[string]*notify.SendError{}
		sc.failures[job.String()] = m
	}
	m[channel] = err
}

// onScheduledJob is told of a schedule's run launching job at at with
// status.
func (sc *script) onScheduledJob(schedule string, at time.Time, status domain.JobStatus, job domain.JobID) {
	inWindow := !at.Before(sc.now.Add(-60*time.Hour)) && at.Before(sc.now.Add(-36*time.Hour))
	// Home Assistant hears of the nightly run's success or failure.
	if schedule != sNightly || !inWindow || (status != domain.JobSucceeded && status != domain.JobFailed) {
		return
	}
	sc.fail(job, "webhook", errHookGone)
	sc.mu.Lock()
	sc.requeued = job
	sc.mu.Unlock()
}

// onManualJob is told of a manual demo job's ID before it is notified.
func (sc *script) onManualJob(dj demoJob, id domain.JobID) {
	if dj.created == retryingJobAgo {
		sc.fail(id, "email", errMailBusy)
		sc.fail(id, "webhook", errHookGone)
	}
}

// requeue returns the job whose dead webhook delivery is queued again.
func (sc *script) requeue() (domain.JobID, bool) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.requeued, !sc.requeued.IsZero()
}

// registry returns copies of base's channels whose Send follows the
// script.
func (sc *script) registry(base *notify.Registry) (*notify.Registry, error) {
	var chs []notify.Channel
	for _, c := range base.Channels() {
		chs = append(chs, scriptedChannel{Channel: c, script: sc})
	}
	return notify.NewRegistry(chs...)
}

// scriptedChannel is a channel whose Send never sends: it answers what
// the script says.
type scriptedChannel struct {
	notify.Channel
	script *script
}

// Send implements notify.Channel.
func (c scriptedChannel) Send(ctx context.Context, _ descriptor.Values, msg notify.Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if msg.Job == nil {
		return nil
	}
	c.script.mu.Lock()
	defer c.script.mu.Unlock()
	if err, ok := c.script.failures[msg.Job.ID][c.ID()]; ok {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Seed-only collaborators of the history's services
// ---------------------------------------------------------------------------

// workerCheck adapts the workers service to schedules.WorkerChecker (its
// Get fails with ErrNotFound for a missing or removed worker).
type workerCheck struct{ ws *workers.Service }

func (c workerCheck) CheckWorker(ctx context.Context, id domain.WorkerID) error {
	_, err := c.ws.Get(ctx, id)
	return err
}

// nopScheduleWaker wakes nobody: the history drives its passes itself.
type nopScheduleWaker struct{}

func (nopScheduleWaker) Wake() {}

// zeroReader is the outbox's jitter source: deterministic retry times.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
