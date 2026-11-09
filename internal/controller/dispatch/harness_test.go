package dispatch_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/adapters/broker"
	"github.com/Zeptiny/Syncr/internal/adapters/idgen"
	"github.com/Zeptiny/Syncr/internal/adapters/memstore"
	"github.com/Zeptiny/Syncr/internal/adapters/secrets"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/app/users"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/controller/dispatch"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
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

// JobFinished records the jobs that ended failed or interrupted, the
// outcomes the dispatcher tests check.
func (r *recorder) JobFinished(j jobs.Job) {
	if j.Status != domain.JobFailed && j.Status != domain.JobInterrupted {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failed = append(r.failed, j)
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
	// remotes resolves the test remote; it survives restarts.
	remotes *fakeRemotes
	// logs captures every service's log lines.
	logs *logBuffer
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
		clock:   clock.NewFake(time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)),
		cfg:     dispatch.DefaultConfig(),
		remotes: newFakeRemotes(),
		logs:    &logBuffer{},
	}
	e.seed()
	e.restart()
	return e
}

// seed stores the owner of the test jobs and the test remote (the store's
// foreign keys need them).
func (e *env) seed() {
	e.t.Helper()
	ctx := context.Background()
	u := users.User{ID: testOwner, Username: "owner", Role: domain.RoleUser, PasswordHash: "x", Version: 1}
	if err := e.store.CreateUser(ctx, u); err != nil {
		e.t.Fatal(err)
	}
	r := remotes.Remote{ID: testRemote(), OwnerID: testOwner, Name: "Archive", Type: "s3", Version: 1}
	if err := e.store.CreateRemote(ctx, r); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) restart() {
	e.t.Helper()
	var err error
	log := slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e.rec, e.hub = newRecorder(), dispatch.NewHub()
	e.jobs, err = jobs.New(jobs.Deps{
		Jobs: e.store, Samples: e.store, Clock: e.clock, IDs: e.ids,
		Broker: e.rec, Notifier: e.rec, Waker: e.hub, Remotes: e.remotes, Logger: log,
	}, jobs.Config{})
	if err != nil {
		e.t.Fatal(err)
	}
	e.workers, err = workers.New(workers.Deps{
		Workers: e.store, Tokens: e.store, Cipher: e.cipher, Clock: e.clock, IDs: e.ids,
		Jobs: e.jobs, Random: rand.Reader, Logger: log,
	}, workers.Config{})
	if err != nil {
		e.t.Fatal(err)
	}
	e.disp, err = dispatch.New(dispatch.Deps{Jobs: e.jobs, Workers: e.workers, Specs: e.specs(), Clock: e.clock, Hub: e.hub, Logger: log}, e.cfg)
	if err != nil {
		e.t.Fatal(err)
	}
}

// specs is the real spec builder over the fake remotes and the built-in
// job kinds.
func (e *env) specs() *dispatch.Specs {
	e.t.Helper()
	specs, err := dispatch.NewSpecs(e.remotes, nil, kinds(e.t))
	if err != nil {
		e.t.Fatal(err)
	}
	return specs
}

func kinds(t *testing.T) *jobkind.Registry {
	t.Helper()
	k, err := jobkind.NewRegistry(jobkind.Builtin()...)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// logBuffer is a goroutine-safe log sink.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
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

// testSecret is the secret of the test remote: it must reach the worker's
// spec and nothing else.
const testSecret = "hunter2-secret-sentinel"

// testOwner owns the test jobs and the test remote.
var testOwner = domain.NewUserID()

func testActor() domain.Actor { return domain.Actor{UserID: testOwner, Role: domain.RoleUser} }

// testRemote is the owner's s3 remote, rooted at its bucket.
func testRemote() domain.RemoteID {
	id, err := domain.ParseRemoteID("00000000-0000-7000-8000-00000000c001")
	if err != nil {
		panic(err)
	}
	return id
}

func testResolved() remotes.Resolved {
	return remotes.Resolved{ID: testRemote(), Name: "Archive", Type: "s3", Rclone: backend.Rclone{
		Backend: "s3", Options: map[string]string{"provider": "Minio"},
		Secrets: map[string]string{"secret_access_key": testSecret}, Root: "bucket",
	}}
}

// testDefinition copies the worker's path to the test remote.
func testDefinition(path string) *syncrv1.JobDefinition {
	return &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_RcloneTransfer{RcloneTransfer: &syncrv1.RcloneTransferDefinition{
		Mode:        syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_COPY,
		Source:      &syncrv1.JobLocation{Path: path},
		Destination: &syncrv1.JobLocation{RemoteId: testRemote().String()},
	}}}
}

// testSpec is the spec the dispatcher builds from testDefinition(path).
func testSpec(path string) *syncrv1.JobSpec {
	return &syncrv1.JobSpec{
		Remotes: []*syncrv1.RemoteDefinition{{
			Alias: "r1", BackendType: "s3", Options: map[string]string{"provider": "Minio"},
			Secrets: map[string]string{"secret_access_key": testSecret},
		}},
		Operation: &syncrv1.JobSpec_RcloneTransfer{RcloneTransfer: &syncrv1.RcloneTransfer{
			Mode:        syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_COPY,
			Source:      &syncrv1.Location{Path: path},
			Destination: &syncrv1.Location{Remote: "r1", Path: "bucket"},
		}},
	}
}

// fakeRemotes is the remotes service of the tests (jobs.RemoteChecker and
// dispatch.Remotes): the test remote belongs to testOwner; a deleted or
// foreign remote is not found. failures are returned by the next Resolve
// calls, in order.
type fakeRemotes struct {
	mu       sync.Mutex
	resolved map[domain.RemoteID]remotes.Resolved
	owners   map[domain.RemoteID]domain.UserID
	failures []error
	resolves int
}

func newFakeRemotes() *fakeRemotes {
	r := testResolved()
	return &fakeRemotes{
		resolved: map[domain.RemoteID]remotes.Resolved{r.ID: r},
		owners:   map[domain.RemoteID]domain.UserID{r.ID: testOwner},
	}
}

// remove makes id unknown (a deleted remote).
func (f *fakeRemotes) remove(id domain.RemoteID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.resolved, id)
	delete(f.owners, id)
}

// failNext makes the next Resolve calls fail with errs.
func (f *fakeRemotes) failNext(errs ...error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = append(f.failures, errs...)
}

func (f *fakeRemotes) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resolves
}

func (f *fakeRemotes) Usable(_ context.Context, owner domain.UserID, ids []domain.RemoteID) (map[domain.RemoteID]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[domain.RemoteID]string, len(ids))
	for _, id := range ids {
		if f.owners[id] != owner {
			return nil, fmt.Errorf("remote %s: %w", id, domain.ErrNotFound)
		}
		out[id] = f.resolved[id].Name
	}
	return out, nil
}

func (f *fakeRemotes) Resolve(_ context.Context, owner domain.UserID, ids []domain.RemoteID) (map[domain.RemoteID]remotes.Resolved, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolves++
	if len(f.failures) > 0 {
		err := f.failures[0]
		f.failures = f.failures[1:]
		return nil, err
	}
	out := make(map[domain.RemoteID]remotes.Resolved, len(ids))
	for _, id := range ids {
		if f.owners[id] != owner {
			return nil, fmt.Errorf("remote %s: %w", id, domain.ErrNotFound)
		}
		r := f.resolved[id]
		r.Rclone = r.Rclone.Clone()
		out[id] = r
	}
	return out, nil
}

// run queues a job on worker w through the job kinds, as the API's RunJob
// does: testDefinition("/data") of testOwner.
func (e *env) run(w domain.WorkerID) jobs.Job {
	e.t.Helper()
	return e.runDef(w, testDefinition("/data"))
}

func (e *env) runDef(w domain.WorkerID, def *syncrv1.JobDefinition) jobs.Job {
	e.t.Helper()
	kind, refs, err := kinds(e.t).Check(def)
	if err != nil {
		e.t.Fatal(err)
	}
	raw, err := jobkind.Marshal(def)
	if err != nil {
		e.t.Fatal(err)
	}
	j, err := e.jobs.Create(context.Background(), testActor(), jobs.CreateParams{
		WorkerID: w, Kind: kind, Definition: raw, RemoteIDs: refs.RemoteIDs, SearchTerms: refs.SearchTerms,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return j
}

func (e *env) job(id domain.JobID) jobs.Job {
	e.t.Helper()
	j, err := e.jobs.Get(context.Background(), testActor(), id)
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
