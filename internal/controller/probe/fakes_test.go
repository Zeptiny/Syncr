package probe_test

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/adapters/idgen"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/controller/probe"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/link"
)

// Secrets of the test fixtures: they may reach a spec and nothing else.
const (
	remoteSecret = "s3-secret-sentinel"
	repoPassword = "repo-password-sentinel"
)

var (
	alice = domain.Actor{UserID: domain.NewUserID(), Role: domain.RoleUser}
	bob   = domain.Actor{UserID: domain.NewUserID(), Role: domain.RoleAdmin}
)

// fakeTransport answers probes with the function set by the test and
// records what it was sent.
type fakeTransport struct {
	mu     sync.Mutex
	sent   []sentProbe
	answer func(worker domain.WorkerID, p *syncrv1.RunProbe) (*syncrv1.ProbeResult, error)
}

type sentProbe struct {
	worker domain.WorkerID
	probe  *syncrv1.RunProbe
}

func (f *fakeTransport) Probe(_ context.Context, worker domain.WorkerID, p *syncrv1.RunProbe) (*syncrv1.ProbeResult, error) {
	f.mu.Lock()
	if sent, ok := proto.Clone(p).(*syncrv1.RunProbe); ok {
		f.sent = append(f.sent, sentProbe{worker, sent})
	}
	answer := f.answer
	f.mu.Unlock()
	if answer == nil {
		return &syncrv1.ProbeResult{ProbeId: p.GetProbeId(), Status: syncrv1.ProbeStatus_PROBE_STATUS_OK}, nil
	}
	return answer(worker, p)
}

func (f *fakeTransport) last(t *testing.T) sentProbe {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		t.Fatal("no probe was sent")
	}
	return f.sent[len(f.sent)-1]
}

func (f *fakeTransport) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

// ops returns the operations of the probes sent, in order.
func (f *fakeTransport) ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, s := range f.sent {
		out = append(out, link.ProbeOpOf(s.probe.GetSpec()))
	}
	return out
}

// failed is a transport answer: the worker reports a failure.
func failed(code syncrv1.JobErrorCode, msg string) func(domain.WorkerID, *syncrv1.RunProbe) (*syncrv1.ProbeResult, error) {
	return func(_ domain.WorkerID, p *syncrv1.RunProbe) (*syncrv1.ProbeResult, error) {
		return &syncrv1.ProbeResult{
			ProbeId: p.GetProbeId(), Status: syncrv1.ProbeStatus_PROBE_STATUS_FAILED,
			Error: &syncrv1.JobError{Code: code, Message: msg},
		}, nil
	}
}

// fakeWorkers is the Workers port over a fixed set.
type fakeWorkers struct {
	mu     sync.Mutex
	list   []workers.Worker
	status map[domain.WorkerID]domain.WorkerStatus
}

func (f *fakeWorkers) add(name string, st domain.WorkerStatus, info workers.Info) domain.WorkerID {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := workers.Worker{ID: domain.NewWorkerID(), Name: name, Mode: domain.WorkerActive, Info: info}
	f.list = append(f.list, w)
	if f.status == nil {
		f.status = map[domain.WorkerID]domain.WorkerStatus{}
	}
	f.status[w.ID] = st
	return w.ID
}

func (f *fakeWorkers) Get(_ context.Context, id domain.WorkerID) (workers.Worker, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, w := range f.list {
		if w.ID == id {
			return w, nil
		}
	}
	return workers.Worker{}, fmt.Errorf("worker %s: %w", id, domain.ErrNotFound)
}

func (f *fakeWorkers) List(context.Context, workers.ListFilter) ([]workers.Worker, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]workers.Worker(nil), f.list...), nil
}

func (f *fakeWorkers) Status(w workers.Worker) domain.WorkerStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status[w.ID]
}

// current is the Hello of a Milestone 5 worker.
func current() workers.Info {
	return workers.Info{
		Version: "v2.5.0", Features: link.Features(), ProbeOps: link.ProbeOps(),
		JobKinds: []string{"rclone_transfer", "restic_backup", "restic_delete_data"},
	}
}

// fakeJobs is the Jobs port: unfinished counts per worker and the worker
// of the newest succeeded job per repository or remote.
type fakeJobs struct {
	mu         sync.Mutex
	unfinished map[domain.WorkerID]int
	lastByRepo map[domain.ResticRepoID]domain.WorkerID
	lastByRem  map[domain.RemoteID]domain.WorkerID
	created    []jobs.CreateParams
	createErr  error
}

func (f *fakeJobs) Unfinished(_ context.Context, id domain.WorkerID) ([]jobs.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return make([]jobs.Job, f.unfinished[id]), nil
}

func (f *fakeJobs) List(_ context.Context, actor domain.Actor, flt jobs.ListFilter) (jobs.Page, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(flt.Statuses) != 1 || flt.Statuses[0] != domain.JobSucceeded || flt.Limit != 1 || actor.Role != domain.RoleUser {
		return jobs.Page{}, fmt.Errorf("unexpected job query %+v as %+v", flt, actor)
	}
	var w domain.WorkerID
	switch {
	case !flt.RepositoryID.IsZero():
		w = f.lastByRepo[flt.RepositoryID]
	case !flt.RemoteID.IsZero():
		w = f.lastByRem[flt.RemoteID]
	}
	if w.IsZero() {
		return jobs.Page{}, nil
	}
	return jobs.Page{Jobs: []jobs.Job{{WorkerID: w, Status: domain.JobSucceeded}}}, nil
}

func (f *fakeJobs) Create(_ context.Context, actor domain.Actor, p jobs.CreateParams) (jobs.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return jobs.Job{}, f.createErr
	}
	f.created = append(f.created, p)
	return jobs.Job{ID: p.ID, OwnerID: actor.UserID}, nil
}

// fakeRemotes is the Remotes port: alice's remotes, by ID.
type fakeRemotes struct {
	mu     sync.Mutex
	byID   map[domain.RemoteID]remotes.Resolved
	drafts []remotes.Input
	// draftErr fails Draft (a validation error of the input).
	draftErr error
}

func newFakeRemotes() *fakeRemotes { return &fakeRemotes{byID: map[domain.RemoteID]remotes.Resolved{}} }

// s3 adds a remote rooted at root; local adds a folder of a worker.
func (f *fakeRemotes) s3(root string) domain.RemoteID {
	return f.put(remotes.Resolved{Name: "Archive", Type: "s3", Rclone: backend.Rclone{
		Backend: "s3", Options: map[string]string{"provider": "Minio"}, Secrets: map[string]string{"secret_access_key": remoteSecret}, Root: root,
	}})
}

func (f *fakeRemotes) local(root string) domain.RemoteID {
	return f.put(remotes.Resolved{Name: "Disk", Type: "local", Rclone: backend.Rclone{Root: root}})
}

func (f *fakeRemotes) put(r remotes.Resolved) domain.RemoteID {
	f.mu.Lock()
	defer f.mu.Unlock()
	r.ID = domain.NewRemoteID()
	f.byID[r.ID] = r
	return r.ID
}

// Resolve returns alice's remotes; anyone else's lookup is not found.
func (f *fakeRemotes) Resolve(_ context.Context, owner domain.UserID, ids []domain.RemoteID) (map[domain.RemoteID]remotes.Resolved, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[domain.RemoteID]remotes.Resolved{}
	for _, id := range ids {
		r, ok := f.byID[id]
		if !ok || owner != alice.UserID {
			return nil, fmt.Errorf("remote %s: %w", id, domain.ErrNotFound)
		}
		out[id] = r
	}
	return out, nil
}

// Draft resolves the stored remote id with in's secrets merged, or a new
// s3 remote from in.
func (f *fakeRemotes) Draft(ctx context.Context, actor domain.Actor, id domain.RemoteID, in remotes.Input) (map[domain.RemoteID]remotes.Resolved, domain.RemoteID, error) {
	f.mu.Lock()
	f.drafts = append(f.drafts, in)
	err := f.draftErr
	f.mu.Unlock()
	if err != nil {
		return nil, domain.RemoteID{}, err
	}
	if !id.IsZero() {
		rs, err := f.Resolve(ctx, actor.UserID, []domain.RemoteID{id})
		if err != nil {
			return nil, domain.RemoteID{}, err
		}
		r := rs[id]
		r.Rclone.Secrets = map[string]string{"secret_access_key": in.Secrets["secret_access_key"]}
		return map[domain.RemoteID]remotes.Resolved{id: r}, id, nil
	}
	id = domain.NewRemoteID()
	return map[domain.RemoteID]remotes.Resolved{id: {ID: id, Name: in.Name, Type: in.Type, Rclone: backend.Rclone{
		Backend: "s3", Options: in.Options, Secrets: in.Secrets, Root: in.Options["bucket"],
	}}}, id, nil
}

// limiter allows until it is closed.
type limiter struct {
	mu     sync.Mutex
	closed bool
	keys   []string
}

func (l *limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.keys = append(l.keys, key)
	return !l.closed
}

func (l *limiter) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
}

// env is a Runner and Service over the fakes.
type env struct {
	t         *testing.T
	transport *fakeTransport
	workers   *fakeWorkers
	jobs      *fakeJobs
	remotes   *fakeRemotes
	limiter   *limiter
	logs      *strings.Builder
	runner    *probe.Runner
	svc       *probe.Service
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{
		t: t, transport: &fakeTransport{}, workers: &fakeWorkers{},
		jobs: &fakeJobs{
			unfinished: map[domain.WorkerID]int{}, lastByRepo: map[domain.ResticRepoID]domain.WorkerID{},
			lastByRem: map[domain.RemoteID]domain.WorkerID{},
		},
		remotes: newFakeRemotes(), limiter: &limiter{}, logs: &strings.Builder{},
	}
	var err error
	e.runner, err = probe.NewRunner(probe.Deps{
		Transport: e.transport, Workers: e.workers, Jobs: e.jobs, Remotes: e.remotes, Limiter: e.limiter,
		IDs: &idgen.Sequential{}, Logger: slog.New(slog.NewTextHandler(&lockedWriter{w: e.logs}, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}, probe.Config{Timeout: 20 * time.Second, ResticTimeout: 45 * time.Second, MaxEntries: 100, MaxSnapshots: 200})
	if err != nil {
		t.Fatal(err)
	}
	e.svc = probe.NewService(e.runner)
	return e
}

// noSecrets fails the test if a secret reached the logs or text.
func (e *env) noSecrets(texts ...string) {
	e.t.Helper()
	for _, s := range append(texts, e.logs.String()) {
		if strings.Contains(s, remoteSecret) || strings.Contains(s, repoPassword) {
			e.t.Errorf("a secret leaked: %s", s)
		}
	}
}

type lockedWriter struct {
	mu sync.Mutex
	w  *strings.Builder
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
