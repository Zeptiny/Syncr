package dispatch_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/adapters/broker"
	"github.com/Zeptiny/Syncr/internal/adapters/idgen"
	"github.com/Zeptiny/Syncr/internal/adapters/memstore"
	"github.com/Zeptiny/Syncr/internal/adapters/secrets"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/controller/dispatch"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/platform/clock"
)

// recorder is the broker (forwarding to memstore) and notifier of the test
// controller; it remembers published statuses and failure notifications.
type recorder struct {
	*broker.Broker
	mu sync.Mutex
	// history holds the status of every published job version. Concurrent
	// changes may publish out of order; the version orders them.
	history map[domain.JobID]map[int64]domain.JobStatus
	failed  []jobs.Job
}

func newRecorder() *recorder {
	return &recorder{Broker: broker.New(), history: make(map[domain.JobID]map[int64]domain.JobStatus)}
}

func (r *recorder) Publish(u jobs.Update) {
	if u.Job != nil {
		r.mu.Lock()
		if r.history[u.JobID] == nil {
			r.history[u.JobID] = make(map[int64]domain.JobStatus)
		}
		r.history[u.JobID][u.Job.Version] = u.Job.Status
		r.mu.Unlock()
	}
	r.Broker.Publish(u)
}

func (r *recorder) JobFailed(_ context.Context, j jobs.Job) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failed = append(r.failed, j)
	return nil
}

// statuses returns the job's published statuses in version order, without
// repeats (a cancel request publishes a new version with the same status).
func (r *recorder) statuses(id domain.JobID) []domain.JobStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.JobStatus
	for _, v := range slices.Sorted(maps.Keys(r.history[id])) {
		if st := r.history[id][v]; len(out) == 0 || out[len(out)-1] != st {
			out = append(out, st)
		}
	}
	return out
}

func (r *recorder) failures() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.failed)
}

// env is a controller over an in-memory store. restart rebuilds every
// service (and the broker and hub) over the same store, like a controller
// process restart.
type env struct {
	t      *testing.T
	store  *memstore.Store
	clock  *clock.Fake
	cipher *secrets.Keyring
	ids    *idgen.Sequential
	cfg    dispatch.Config

	rec     *recorder
	hub     *dispatch.Hub
	workers *workers.Service
	jobs    *jobs.Service
	disp    *dispatch.Service
}

func newEnv(t *testing.T) *env {
	t.Helper()
	key := secrets.Key{ID: 1, Bytes: make([]byte, secrets.KeySize)}
	_, _ = rand.Read(key.Bytes)
	cipher, err := secrets.NewKeyring(key)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{
		t: t, store: memstore.New(), cipher: cipher, ids: &idgen.Sequential{},
		clock: clock.NewFake(time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)),
		cfg:   dispatch.DefaultConfig(),
	}
	e.restart()
	return e
}

func (e *env) restart() {
	e.t.Helper()
	var err error
	e.rec, e.hub = newRecorder(), dispatch.NewHub()
	e.jobs, err = jobs.New(jobs.Deps{
		Jobs: e.store, Samples: e.store, Clock: e.clock, IDs: e.ids,
		Broker: e.rec, Notifier: e.rec, Waker: e.hub,
	}, jobs.Config{})
	if err != nil {
		e.t.Fatal(err)
	}
	e.workers, err = workers.New(workers.Deps{
		Workers: e.store, Tokens: e.store, Cipher: e.cipher, Clock: e.clock, IDs: e.ids,
		Jobs: e.jobs, Random: rand.Reader,
	}, workers.Config{})
	if err != nil {
		e.t.Fatal(err)
	}
	e.disp, err = dispatch.New(dispatch.Deps{Jobs: e.jobs, Workers: e.workers, Specs: specBuilder{}, Clock: e.clock, Hub: e.hub}, e.cfg)
	if err != nil {
		e.t.Fatal(err)
	}
}

// enroll enrolls an active worker through a token and returns its ID.
func (e *env) enroll(name string) domain.WorkerID {
	e.t.Helper()
	ctx := context.Background()
	g, err := e.workers.CreateEnrollmentToken(ctx, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	en, err := e.workers.Enroll(ctx, workers.EnrollParams{Token: g.Token, Name: name})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.workers.Authenticate(ctx, en.Worker.ID, en.Secret); err != nil {
		e.t.Fatal(err)
	}
	return en.Worker.ID
}

func testSpec(path string) *syncrv1.JobSpec {
	return &syncrv1.JobSpec{
		Remotes: []*syncrv1.RemoteDefinition{{Alias: "s3", BackendType: "s3", Secrets: map[string]string{"secret_access_key": "hunter2"}}},
		Operation: &syncrv1.JobSpec_RcloneTransfer{RcloneTransfer: &syncrv1.RcloneTransfer{
			Mode:        syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_COPY,
			Source:      &syncrv1.Location{Path: path},
			Destination: &syncrv1.Location{Remote: "s3", Path: "bucket"},
		}},
	}
}

// testOwner owns the test jobs.
var testOwner = domain.NewUserID()

// specBuilder is the dispatcher's SpecBuilder in these tests: a job's
// "definition" is the protojson of the very spec to send (see run).
//
// m3: pending track C: real definitions are built by job kinds from the
// owner's resolved remotes; these tests only need some spec delivered.
type specBuilder struct{}

func (specBuilder) Build(_ context.Context, j jobs.Job) (*syncrv1.JobSpec, error) {
	spec := new(syncrv1.JobSpec)
	if err := protojson.Unmarshal(j.Definition, spec); err != nil {
		return nil, fmt.Errorf("%w: %w", domain.ErrInvalidArgument, err)
	}
	return spec, nil
}

// run queues a job on worker w, as the API's RunJob will.
func (e *env) run(w domain.WorkerID) jobs.Job {
	e.t.Helper()
	raw, err := protojson.Marshal(testSpec("/data"))
	if err != nil {
		e.t.Fatal(err)
	}
	j, err := e.jobs.Create(context.Background(), jobs.CreateParams{WorkerID: w, OwnerID: testOwner, Kind: "rclone_transfer", Definition: raw})
	if err != nil {
		e.t.Fatal(err)
	}
	return j
}

func (e *env) job(id domain.JobID) jobs.Job {
	e.t.Helper()
	j, err := e.jobs.Get(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return j
}

func (e *env) worker(id domain.WorkerID) workers.Worker {
	e.t.Helper()
	w, err := e.workers.Get(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return w
}

// fakeWorker is a worker process speaking the link through the dispatcher,
// with the semantics of internal/worker/agent and outbox: an in-memory
// outbox of events with seqs, StartJob/CancelJob deduplicated by job ID,
// finished jobs remembered for the epoch, and a WorkerState consistent with
// the events it is sent with. Methods are not safe for concurrent use.
type fakeWorker struct {
	t     *testing.T
	disp  func() *dispatch.Service // current controller (survives restarts)
	clock *clock.Fake
	id    domain.WorkerID

	epoch    string
	seq      int64
	outbox   []*syncrv1.Event
	held     []string // accepted job IDs, in order
	running  map[string]bool
	finished map[string]bool
	specs    map[string]*syncrv1.JobSpec
	acks     []*syncrv1.CommandAck
	restarts int
}

func (e *env) newFakeWorker(id domain.WorkerID) *fakeWorker {
	w := &fakeWorker{t: e.t, disp: func() *dispatch.Service { return e.disp }, clock: e.clock, id: id}
	w.restart()
	return w
}

// restart simulates a worker process restart: a new epoch, everything in
// memory lost, a Hello as seq 1.
func (w *fakeWorker) restart() {
	w.restarts++
	w.epoch = fmt.Sprintf("%s-epoch-%d", w.id.String()[len(w.id.String())-4:], w.restarts)
	w.seq, w.outbox, w.held, w.acks = 0, nil, nil, nil
	w.running, w.finished, w.specs = map[string]bool{}, map[string]bool{}, map[string]*syncrv1.JobSpec{}
	w.emit(&syncrv1.Event{Payload: &syncrv1.Event_Hello{Hello: &syncrv1.Hello{
		WorkerVersion: "v2.0.0-test", Os: "linux", Arch: "amd64", JobKinds: []string{"rclone_transfer"}, MaxConcurrentJobs: 2,
		LocalRoots: []string{"/data"}, Tools: []*syncrv1.ToolVersion{{Name: "rclone", Version: "v1.75.2"}},
	}}})
}

func (w *fakeWorker) emit(ev *syncrv1.Event) *syncrv1.Event {
	w.seq++
	ev.Seq, ev.Time = w.seq, timestamppb.New(w.clock.Now())
	w.outbox = append(w.outbox, ev)
	return ev
}

// handle processes commands like the worker agent and queues their acks.
func (w *fakeWorker) handle(cmds []*syncrv1.Command) {
	for _, c := range cmds {
		switch x := c.GetCommand().(type) {
		case *syncrv1.Command_StartJob:
			id := x.StartJob.GetJobId()
			if !slices.Contains(w.held, id) && !w.finished[id] {
				w.held = append(w.held, id)
				w.specs[id] = x.StartJob.GetSpec()
			}
		case *syncrv1.Command_CancelJob:
			id := x.CancelJob.GetJobId()
			if slices.Contains(w.held, id) {
				w.finish(id, syncrv1.JobResultStatus_JOB_RESULT_STATUS_CANCELLED,
					&syncrv1.JobError{Code: syncrv1.JobErrorCode_JOB_ERROR_CODE_CANCELLED, Message: "job cancelled by the controller"})
			}
		}
		w.acks = append(w.acks, &syncrv1.CommandAck{CommandId: c.GetId()})
	}
}

func (w *fakeWorker) start(id string) {
	w.t.Helper()
	if !slices.Contains(w.held, id) || w.running[id] {
		w.t.Fatalf("fake worker cannot start %s", id)
	}
	w.running[id] = true
	w.emit(&syncrv1.Event{Payload: &syncrv1.Event_JobStarted{JobStarted: &syncrv1.JobStarted{JobId: id}}})
}

func (w *fakeWorker) progress(id string, bytes int64) {
	w.emit(&syncrv1.Event{Payload: &syncrv1.Event_Progress{Progress: &syncrv1.Progress{JobId: id, Stats: &syncrv1.JobStats{Bytes: bytes, Elapsed: durationpb.New(time.Second)}}}})
}

// finish appends the job's result, then forgets the job (in that order, so
// a state taken in between still lists it).
func (w *fakeWorker) finish(id string, st syncrv1.JobResultStatus, jerr *syncrv1.JobError) *syncrv1.Event {
	ev := w.emit(&syncrv1.Event{Payload: &syncrv1.Event_JobResult{JobResult: &syncrv1.JobResult{
		JobId: id, Status: st, Error: jerr, Stats: &syncrv1.JobStats{Bytes: 1 << 33, Files: 3}, ExitCode: 0,
		FinishedAt: timestamppb.New(w.clock.Now()),
	}}})
	w.held = slices.DeleteFunc(w.held, func(h string) bool { return h == id })
	delete(w.running, id)
	w.finished[id] = true
	return ev
}

func (w *fakeWorker) state() *syncrv1.WorkerState {
	st := &syncrv1.WorkerState{LastSeq: w.seq, OutboxBacklog: int32(len(w.outbox))}
	for _, id := range w.held {
		aj := &syncrv1.ActiveJob{JobId: id}
		if w.running[id] {
			aj.StartedAt = timestamppb.New(w.clock.Now())
		}
		st.Jobs = append(st.Jobs, aj)
	}
	return st
}

// reportRequest builds the next Report: every unacked event, every pending
// ack and the matching state.
func (w *fakeWorker) reportRequest() *syncrv1.ReportRequest {
	return &syncrv1.ReportRequest{Epoch: w.epoch, Events: slices.Clone(w.outbox), Acks: slices.Clone(w.acks), State: w.state()}
}

// deliver sends req and applies the response like the worker's link core.
func (w *fakeWorker) deliver(req *syncrv1.ReportRequest) (int64, error) {
	resp, err := w.disp().Report(context.Background(), w.id, req)
	if err != nil {
		return 0, err
	}
	if req.GetEpoch() == w.epoch {
		w.outbox = slices.DeleteFunc(w.outbox, func(ev *syncrv1.Event) bool { return ev.GetSeq() <= resp.GetAckSeq() })
		w.acks = slices.DeleteFunc(w.acks, func(a *syncrv1.CommandAck) bool {
			return slices.ContainsFunc(req.GetAcks(), func(r *syncrv1.CommandAck) bool { return r.GetCommandId() == a.GetCommandId() })
		})
	}
	return resp.GetAckSeq(), nil
}

func (w *fakeWorker) report() int64 {
	w.t.Helper()
	ack, err := w.deliver(w.reportRequest())
	if err != nil {
		w.t.Fatalf("report: %v", err)
	}
	return ack
}

// poll long-polls (wait 0: no waiting), applying pending acks first, and
// handles the commands it receives.
func (w *fakeWorker) poll() []*syncrv1.Command {
	w.t.Helper()
	req := &syncrv1.PollRequest{Epoch: w.epoch, Acks: slices.Clone(w.acks), Wait: durationpb.New(0)}
	resp, err := w.disp().Poll(context.Background(), w.id, req)
	if err != nil {
		w.t.Fatalf("poll: %v", err)
	}
	w.acks = nil
	w.handle(resp.GetCommands())
	return resp.GetCommands()
}
