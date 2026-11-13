package api

import (
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/adapters/idgen"
	"github.com/Zeptiny/Syncr/internal/adapters/secrets"
	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/backend/builtin"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// repoPassword is the repository password of the API tests: it must never
// be in a response or a log line.
const repoPassword = "sentinel-api-repo-password-71c"

const apiResticID = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"

// lateRepositories is the jobs' and schedules' RepositoryChecker of the
// test env: the restic service once a test has built it (the store it
// needs exists only after newEnv), no repository before.
type lateRepositories struct {
	mu  sync.Mutex
	svc *restic.Service
}

func (l *lateRepositories) set(svc *restic.Service) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.svc = svc
}

func (l *lateRepositories) UsableRepository(ctx context.Context, owner domain.UserID, id domain.ResticRepoID, jobID domain.JobID) (string, domain.RemoteID, error) {
	l.mu.Lock()
	svc := l.svc
	l.mu.Unlock()
	if svc == nil {
		return "", domain.RemoteID{}, fmt.Errorf("repository %s: %w", id, domain.ErrNotFound)
	}
	return svc.UsableRepository(ctx, owner, id, jobID)
}

// lateRestic lets the restic service be built after newEnv made the store
// it needs (the handler is built before).
type lateRestic struct{ ResticService }

// apiProber is the restic.Prober of the API tests.
type apiProber struct {
	mu        sync.Mutex
	err       error
	resticID  string
	worker    domain.WorkerID
	snapshots []restic.Snapshot
	listing   restic.Listing
	targets   []restic.Target
}

func (p *apiProber) fail(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.err = err
}

func (p *apiProber) setResticID(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resticID = id
}

func (p *apiProber) lastTarget() restic.Target {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.targets) == 0 {
		return restic.Target{}
	}
	return p.targets[len(p.targets)-1]
}

func (p *apiProber) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.targets)
}

func (p *apiProber) Verify(_ context.Context, _ domain.UserID, t restic.Target, _ bool) (restic.VerifyResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.targets = append(p.targets, t)
	if p.err != nil {
		return restic.VerifyResult{}, p.err
	}
	return restic.VerifyResult{ResticID: p.resticID, FormatVersion: 2, WorkerID: p.worker, WorkerName: "worker"}, nil
}

func (p *apiProber) Snapshots(_ context.Context, _ domain.UserID, t restic.Target) (restic.SnapshotsResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.targets = append(p.targets, t)
	if p.err != nil {
		return restic.SnapshotsResult{}, p.err
	}
	return restic.SnapshotsResult{Snapshots: p.snapshots, WorkerID: p.worker, WorkerName: "worker"}, nil
}

func (p *apiProber) List(_ context.Context, _ domain.UserID, t restic.Target, _, path string) (restic.Listing, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.targets = append(p.targets, t)
	if p.err != nil {
		return restic.Listing{}, p.err
	}
	l := p.listing
	l.Path, l.WorkerID, l.WorkerName = path, p.worker, "worker"
	return l, nil
}

// Measure, Find and Diff are not called by the handlers yet.

func (p *apiProber) Measure(context.Context, domain.UserID, restic.Target, string, string) (restic.Measurement, error) {
	return restic.Measurement{}, errors.ErrUnsupported
}

func (p *apiProber) Find(context.Context, domain.UserID, restic.Target, restic.FindQuery) (restic.FindResult, error) {
	return restic.FindResult{}, errors.ErrUnsupported
}

func (p *apiProber) Diff(context.Context, domain.UserID, restic.Target, string, string, string) (restic.Comparison, error) {
	return restic.Comparison{}, errors.ErrUnsupported
}

// apiDeletions is the restic.Deletions of the API tests: it picks the
// env's worker and records the jobs it was asked to start.
type apiDeletions struct {
	mu      sync.Mutex
	worker  domain.WorkerID
	started []domain.JobID
}

func (d *apiDeletions) Pick(_ context.Context, _ domain.UserID, _ domain.RemoteID, workerID domain.WorkerID) (domain.WorkerID, error) {
	if !workerID.IsZero() {
		return workerID, nil
	}
	return d.worker, nil
}

func (d *apiDeletions) Start(_ context.Context, _ domain.UserID, _ domain.ResticRepoID, jobID domain.JobID, _ domain.WorkerID) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.started = append(d.started, jobID)
	return nil
}

// fakeRemoteTypes is the restic service's RemoteChecker over the env's
// fake remotes.
type fakeRemoteTypes struct{ f *fakeJobRemotes }

func (r fakeRemoteTypes) RemoteType(_ context.Context, owner domain.UserID, id domain.RemoteID) (string, error) {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	ref, ok := r.f.refs[id]
	if !ok || ref.Deleted || r.f.owner[id] != owner {
		return "", fmt.Errorf("remote %s: %w", id, domain.ErrNotFound)
	}
	return ref.Type, nil
}

// resticEnv is the API test env with the real restic service (over the
// env's store and remotes) on a fake prober and deleter. Its jobs and
// schedules check repositories with that service.
type resticEnv struct {
	*testEnv
	svc       *restic.Service
	prober    *apiProber
	deletions *apiDeletions
	remote    domain.RemoteID // an S3 remote of testUser
}

func newResticEnv(t *testing.T, mutate ...func(*Deps)) *resticEnv {
	t.Helper()
	late := &lateRestic{}
	e := newEnv(t, append([]func(*Deps){func(d *Deps) { d.Restic = late }}, mutate...)...)
	key := secrets.Key{ID: 1, Bytes: make([]byte, secrets.KeySize)}
	if _, err := crand.Read(key.Bytes); err != nil {
		t.Fatal(err)
	}
	ring, err := secrets.NewKeyring(key)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := backend.NewRegistry(builtin.Types()...)
	if err != nil {
		t.Fatal(err)
	}
	re := &resticEnv{
		testEnv: e, prober: &apiProber{resticID: apiResticID, worker: e.worker}, deletions: &apiDeletions{worker: e.worker},
	}
	re.svc, err = restic.New(restic.Deps{
		Repo: e.store, Remotes: fakeRemoteTypes{e.remotes}, Prober: re.prober, Backends: reg, Deletions: re.deletions,
		Cipher: ring, Clock: e.clock, IDs: &idgen.Sequential{}, Logger: newTestLogger(e.logs),
	}, restic.Config{})
	if err != nil {
		t.Fatal(err)
	}
	late.ResticService = re.svc
	e.repos.set(re.svc)
	re.remote = e.addRemote(testUser, "B2 offsite", "s3")
	t.Cleanup(func() {
		if strings.Contains(e.logs.String(), repoPassword) {
			t.Errorf("the log contains the repository password:\n%s", e.logs.String())
		}
	})
	return re
}

func (e *testEnv) resticFor(a domain.Actor) syncrv1connect.ResticServiceClient {
	return syncrv1connect.NewResticServiceClient(e.clientWith(testActorHeader, actorHeader(a, "session")), e.srv.URL)
}

func (e *resticEnv) input(name, path string) *syncrv1.RepositoryInput {
	return &syncrv1.RepositoryInput{
		Name: name, Description: "family photos", RemoteId: e.remote.String(), Path: path, Password: repoPassword,
		Mode: syncrv1.RepositoryCreateMode_REPOSITORY_CREATE_MODE_INIT,
	}
}

// createRepo creates a repository of testUser through the API. Each has
// its own restic ID.
func (e *resticEnv) createRepo(name string) *syncrv1.Repository {
	e.t.Helper()
	e.prober.setResticID(fmt.Sprintf("%064x", len(name)*1000+int(name[0])))
	resp, err := e.resticFor(testUser).CreateRepository(e.t.Context(), connect.NewRequest(&syncrv1.CreateRepositoryRequest{Input: e.input(name, "restic/"+name)}))
	if err != nil {
		e.t.Fatalf("CreateRepository(%s): %v", name, err)
	}
	return resp.Msg.GetRepository()
}

// noPassword fails the test if msg contains the repository password.
func noPassword(t *testing.T, what string, msg proto.Message) {
	t.Helper()
	b, err := protojson.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), repoPassword) {
		t.Errorf("%s contains the repository password", what)
	}
}

func TestResticCreateGetList(t *testing.T) {
	t.Parallel()
	e := newResticEnv(t)
	ctx := t.Context()
	c := e.resticFor(testUser)

	resp, err := c.CreateRepository(ctx, connect.NewRequest(&syncrv1.CreateRepositoryRequest{Input: e.input("Photos vault", "/restic//photos/")}))
	if err != nil {
		t.Fatal(err)
	}
	r := resp.Msg.GetRepository()
	if r.GetName() != "Photos vault" || r.GetPath() != "restic/photos" || r.GetRemoteId() != e.remote.String() || r.GetResticId() != apiResticID ||
		r.GetFormatVersion() != 2 || r.GetVersion() != 1 || r.GetCreatedAt() == nil || r.GetDeleting() || r.GetDeletionJobId() != "" ||
		r.GetRemote().GetName() != "B2 offsite" || r.GetRemote().GetBackendType() != "s3" {
		t.Errorf("CreateRepository = %v", r)
	}
	if w := resp.Msg.GetWorker(); w.GetId() != e.worker.String() || w.GetName() != "worker" {
		t.Errorf("worker = %v", w)
	}
	// A new repository was never listed: its statistics say so.
	if st := r.GetStats(); st.GetSnapshotCount() != 0 || !st.GetSnapshotsStale() || st.GetSnapshotsRefreshedAt() != nil || st.GetLastBackup() != nil {
		t.Errorf("stats of a new repository = %v", st)
	}
	if got := e.prober.lastTarget(); got.Password != repoPassword || got.Path != "restic/photos" || got.RemoteID != e.remote {
		t.Errorf("the worker was asked about remote %s path %q", got.RemoteID, got.Path)
	}
	noPassword(t, "CreateRepositoryResponse", resp.Msg)

	got, err := c.GetRepository(ctx, connect.NewRequest(&syncrv1.GetRepositoryRequest{RepositoryId: r.GetId()}))
	if err != nil || !proto.Equal(got.Msg.GetRepository(), r) {
		t.Errorf("GetRepository = %v, %v; want %v", got, err, r)
	}
	noPassword(t, "GetRepositoryResponse", got.Msg)

	other := e.addRemote(testUser, "Archive", "sftp")
	in := e.input("Archive", "a")
	in.RemoteId, in.Mode, in.WorkerId = other.String(), syncrv1.RepositoryCreateMode_REPOSITORY_CREATE_MODE_CONNECT, e.worker.String()
	e.prober.setResticID(strings.Repeat("b", 64))
	if _, err := c.CreateRepository(ctx, connect.NewRequest(&syncrv1.CreateRepositoryRequest{Input: in})); err != nil {
		t.Fatal(err)
	}
	if got := e.prober.lastTarget(); got.WorkerID != e.worker {
		t.Errorf("the chosen worker was not passed on: %s", got.WorkerID)
	}
	list, err := c.ListRepositories(ctx, connect.NewRequest(&syncrv1.ListRepositoriesRequest{}))
	if err != nil || len(list.Msg.GetRepositories()) != 2 || list.Msg.GetRepositories()[0].GetName() != "Archive" || list.Msg.GetRepositories()[1].GetName() != "Photos vault" {
		t.Fatalf("ListRepositories = %v, %v", list, err)
	}
	noPassword(t, "ListRepositoriesResponse", list.Msg)
	list, err = c.ListRepositories(ctx, connect.NewRequest(&syncrv1.ListRepositoriesRequest{RemoteId: other.String()}))
	if err != nil || len(list.Msg.GetRepositories()) != 1 || list.Msg.GetRepositories()[0].GetRemote().GetName() != "Archive" {
		t.Errorf("ListRepositories(remote) = %v, %v", list, err)
	}
	_, err = c.ListRepositories(ctx, connect.NewRequest(&syncrv1.ListRepositoriesRequest{RemoteId: "nope"}))
	wantCode(t, err, connect.CodeInvalidArgument)
}

func TestResticCreateErrors(t *testing.T) {
	t.Parallel()
	e := newResticEnv(t)
	ctx := t.Context()
	c := e.resticFor(testUser)
	create := func(mutate func(*syncrv1.RepositoryInput)) error {
		in := e.input("Photos vault", "restic/photos")
		mutate(in)
		_, err := c.CreateRepository(ctx, connect.NewRequest(&syncrv1.CreateRepositoryRequest{Input: in}))
		return err
	}

	_, err := c.CreateRepository(ctx, connect.NewRequest(&syncrv1.CreateRepositoryRequest{}))
	wantCode(t, err, connect.CodeInvalidArgument)
	if violation(err, "input") == "" {
		t.Errorf("no input: %v", err)
	}
	theirs := e.addRemote(testOther, "Theirs", "s3")
	for _, tt := range []struct {
		name   string
		mutate func(*syncrv1.RepositoryInput)
		field  string
	}{
		{"no name", func(in *syncrv1.RepositoryInput) { in.Name = "" }, "name"},
		{"long description", func(in *syncrv1.RepositoryInput) { in.Description = strings.Repeat("d", 501) }, "description"},
		{"bad path", func(in *syncrv1.RepositoryInput) { in.Path = "a/../b" }, "path"},
		{"no mode", func(in *syncrv1.RepositoryInput) { in.Mode = 0 }, "mode"},
		{"unknown mode", func(in *syncrv1.RepositoryInput) { in.Mode = 99 }, "mode"},
		{"no password", func(in *syncrv1.RepositoryInput) { in.Password = "" }, "password"},
		{"short password", func(in *syncrv1.RepositoryInput) { in.Password = "1234567" }, "password"},
		{"no remote", func(in *syncrv1.RepositoryInput) { in.RemoteId = "" }, "remote_id"},
		{"malformed remote", func(in *syncrv1.RepositoryInput) { in.RemoteId = "nope" }, "remote_id"},
		{"another user's remote", func(in *syncrv1.RepositoryInput) { in.RemoteId = theirs.String() }, "remote_id"},
		{"malformed worker", func(in *syncrv1.RepositoryInput) { in.WorkerId = "nope" }, "worker_id"},
	} {
		err := create(tt.mutate)
		if codeOf(t, err) != connect.CodeInvalidArgument || violation(err, tt.field) == "" {
			t.Errorf("%s: %v, want InvalidArgument on %s", tt.name, err, tt.field)
		}
	}
	if e.prober.calls() != 0 {
		t.Error("invalid input reached a worker")
	}

	// What the worker reports maps to the form's fields; the worker's
	// message (which could echo the password) is not passed on.
	probe := func(code string) error {
		return errors.Join(fmt.Errorf("%w: mapped", domain.ErrFailedPrecondition), &restic.ProbeError{Code: code, Message: "said " + repoPassword})
	}
	for code, field := range map[string]string{"wrong_password": "password", "already_exists": "mode", "path_not_allowed": "worker_id"} {
		e.prober.fail(probe(code))
		err := create(func(*syncrv1.RepositoryInput) {})
		if codeOf(t, err) != connect.CodeInvalidArgument || violation(err, field) == "" || strings.Contains(err.Error(), repoPassword) {
			t.Errorf("%s: %v, want InvalidArgument on %s", code, err, field)
		}
	}
	e.prober.fail(probe("repository_not_found"))
	err = create(func(in *syncrv1.RepositoryInput) {
		in.Mode = syncrv1.RepositoryCreateMode_REPOSITORY_CREATE_MODE_CONNECT
	})
	// The remote is an S3 bucket, where a missing folder looks empty.
	if v := violation(err, "path"); !strings.HasPrefix(v, "no restic repository at this location") || !strings.Contains(v, "looks empty") {
		t.Errorf("connect without a repository: %v", err)
	}
	for _, tt := range []struct {
		err  error
		code connect.Code
	}{
		{probe("execution_failed"), connect.CodeFailedPrecondition},
		{fmt.Errorf("%w: worker \"nas\" is offline", domain.ErrUnavailable), connect.CodeUnavailable},
		{fmt.Errorf("%w: slow down", domain.ErrRateLimited), connect.CodeResourceExhausted},
		{fmt.Errorf("%w: no worker can run restic", domain.ErrFailedPrecondition), connect.CodeFailedPrecondition},
	} {
		e.prober.fail(tt.err)
		err := create(func(*syncrv1.RepositoryInput) {})
		if codeOf(t, err) != tt.code || strings.Contains(err.Error(), repoPassword) {
			t.Errorf("probe error %v: got %v, want %v", tt.err, err, tt.code)
		}
	}
	e.prober.fail(nil)

	// What is taken is said in the message.
	e.createRepo("Photos vault")
	e.prober.setResticID(strings.Repeat("c", 64))
	for _, tt := range []struct {
		mutate func(*syncrv1.RepositoryInput)
		want   string
	}{
		{func(in *syncrv1.RepositoryInput) { in.Name, in.Path = "photos VAULT", "elsewhere" }, "a repository with this name already exists"},
		{func(in *syncrv1.RepositoryInput) { in.Name, in.Path = "Other", "restic/Photos vault" }, "a repository at this location already exists"},
	} {
		err := create(tt.mutate)
		if codeOf(t, err) != connect.CodeAlreadyExists || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("taken: %v, want AlreadyExists %q", err, tt.want)
		}
	}
	e.prober.setResticID(fmt.Sprintf("%064x", len("Photos vault")*1000+int('P')))
	err = create(func(in *syncrv1.RepositoryInput) { in.Name, in.Path = "Other", "elsewhere" })
	if codeOf(t, err) != connect.CodeAlreadyExists || !strings.Contains(err.Error(), "already one of your repositories") {
		t.Errorf("the same restic repository: %v", err)
	}
}

func TestResticOwnerScoping(t *testing.T) {
	t.Parallel()
	e := newResticEnv(t)
	ctx := t.Context()
	r := e.createRepo("Photos vault")
	before := e.prober.calls()
	for _, actor := range []domain.Actor{testOther, testAdmin} {
		c := e.resticFor(actor)
		checks := map[string]error{}
		_, checks["GetRepository"] = c.GetRepository(ctx, connect.NewRequest(&syncrv1.GetRepositoryRequest{RepositoryId: r.GetId()}))
		_, checks["UpdateRepository"] = c.UpdateRepository(ctx, connect.NewRequest(&syncrv1.UpdateRepositoryRequest{RepositoryId: r.GetId(), Version: 1, Name: "Stolen", Password: "new-password"}))
		_, checks["DeleteRepository"] = c.DeleteRepository(ctx, connect.NewRequest(&syncrv1.DeleteRepositoryRequest{RepositoryId: r.GetId()}))
		_, checks["DeleteRepository with data"] = c.DeleteRepository(ctx, connect.NewRequest(&syncrv1.DeleteRepositoryRequest{RepositoryId: r.GetId(), DeleteData: true, ConfirmName: r.GetName()}))
		_, checks["ListSnapshots"] = c.ListSnapshots(ctx, connect.NewRequest(&syncrv1.ListSnapshotsRequest{RepositoryId: r.GetId(), Refresh: true}))
		_, checks["BrowseSnapshot"] = c.BrowseSnapshot(ctx, connect.NewRequest(&syncrv1.BrowseSnapshotRequest{RepositoryId: r.GetId(), SnapshotId: "1a2b3c4d", Path: "/"}))
		_, checks["RunJob"] = e.runAs(actor, apiBackupDef(r.GetId()))
		for what, err := range checks {
			if codeOf(t, err) != connect.CodeNotFound {
				t.Errorf("%s as %s = %v, want NotFound", what, actor.Role, err)
			}
		}
		list, err := c.ListRepositories(ctx, connect.NewRequest(&syncrv1.ListRepositoriesRequest{}))
		if err != nil || len(list.Msg.GetRepositories()) != 0 {
			t.Errorf("ListRepositories as %s = %v, %v", actor.Role, list, err)
		}
	}
	if e.prober.calls() != before || len(e.deletions.started) != 0 {
		t.Error("a foreign request reached a worker or started a deletion")
	}
	got, err := e.resticFor(testUser).GetRepository(ctx, connect.NewRequest(&syncrv1.GetRepositoryRequest{RepositoryId: r.GetId()}))
	if err != nil || !proto.Equal(got.Msg.GetRepository(), r) {
		t.Errorf("the repository changed: %v, %v", got, err)
	}
	// Malformed and missing IDs.
	c := e.resticFor(testUser)
	_, err = c.GetRepository(ctx, connect.NewRequest(&syncrv1.GetRepositoryRequest{RepositoryId: "nope"}))
	wantCode(t, err, connect.CodeInvalidArgument)
	_, err = c.GetRepository(ctx, connect.NewRequest(&syncrv1.GetRepositoryRequest{}))
	wantCode(t, err, connect.CodeInvalidArgument)
	_, err = c.GetRepository(ctx, connect.NewRequest(&syncrv1.GetRepositoryRequest{RepositoryId: domain.NewResticRepoID().String()}))
	wantCode(t, err, connect.CodeNotFound)
	// Without credentials nothing is served.
	_, err = syncrv1connect.NewResticServiceClient(e.srv.Client(), e.srv.URL).ListRepositories(ctx, connect.NewRequest(&syncrv1.ListRepositoriesRequest{}))
	wantCode(t, err, connect.CodeUnauthenticated)
}

func TestResticUpdate(t *testing.T) {
	t.Parallel()
	e := newResticEnv(t)
	ctx := t.Context()
	c := e.resticFor(testUser)
	r := e.createRepo("Photos vault")
	update := func(m *syncrv1.UpdateRepositoryRequest) (*syncrv1.Repository, error) {
		m.RepositoryId = r.GetId()
		resp, err := c.UpdateRepository(ctx, connect.NewRequest(m))
		if err != nil {
			return nil, err
		}
		noPassword(t, "UpdateRepositoryResponse", resp.Msg)
		return resp.Msg.GetRepository(), nil
	}

	_, err := update(&syncrv1.UpdateRepositoryRequest{Name: "Family"})
	if codeOf(t, err) != connect.CodeInvalidArgument || violation(err, "version") == "" {
		t.Errorf("without a version: %v", err)
	}
	got, err := update(&syncrv1.UpdateRepositoryRequest{Version: 1, Name: "Family", Description: "new"})
	if err != nil || got.GetName() != "Family" || got.GetDescription() != "new" || got.GetVersion() != 2 || got.GetRemote().GetName() != "B2 offsite" {
		t.Fatalf("UpdateRepository = %v, %v", got, err)
	}
	_, err = update(&syncrv1.UpdateRepositoryRequest{Version: 1, Name: "Again"})
	wantCode(t, err, connect.CodeAborted)
	_, err = update(&syncrv1.UpdateRepositoryRequest{Version: 2, Name: ""})
	if violation(err, "name") == "" {
		t.Errorf("no name: %v", err)
	}
	_, err = update(&syncrv1.UpdateRepositoryRequest{Version: 2, Name: "Family", WorkerId: "nope"})
	if violation(err, "worker_id") == "" {
		t.Errorf("malformed worker: %v", err)
	}

	// A new password is verified on the chosen worker.
	e.prober.setResticID(r.GetResticId())
	got, err = update(&syncrv1.UpdateRepositoryRequest{Version: 2, Name: "Family", Password: repoPassword + "-next", WorkerId: e.worker.String()})
	if err != nil || got.GetVersion() != 3 {
		t.Fatalf("UpdateRepository(password) = %v, %v", got, err)
	}
	if tgt := e.prober.lastTarget(); tgt.Password != repoPassword+"-next" || tgt.WorkerID != e.worker || tgt.RepositoryID.String() != r.GetId() {
		t.Errorf("the new password was not verified on the chosen worker")
	}
	e.prober.fail(errors.Join(domain.ErrFailedPrecondition, &restic.ProbeError{Code: "wrong_password"}))
	_, err = update(&syncrv1.UpdateRepositoryRequest{Version: 3, Name: "Family", Password: "wrong-password"})
	if codeOf(t, err) != connect.CodeInvalidArgument || violation(err, "password") != "is not this repository's password" {
		t.Errorf("wrong password: %v", err)
	}
}

func TestResticDelete(t *testing.T) {
	t.Parallel()
	e := newResticEnv(t)
	ctx := t.Context()
	c := e.resticFor(testUser)

	// Forget only is the default: no job, the repository is gone.
	forget := e.createRepo("Forget me")
	resp, err := c.DeleteRepository(ctx, connect.NewRequest(&syncrv1.DeleteRepositoryRequest{RepositoryId: forget.GetId()}))
	if err != nil || resp.Msg.GetDeletionJobId() != "" || resp.Msg.GetWorker() != nil {
		t.Fatalf("DeleteRepository = %v, %v", resp, err)
	}
	_, err = c.GetRepository(ctx, connect.NewRequest(&syncrv1.GetRepositoryRequest{RepositoryId: forget.GetId()}))
	wantCode(t, err, connect.CodeNotFound)
	_, err = c.DeleteRepository(ctx, connect.NewRequest(&syncrv1.DeleteRepositoryRequest{RepositoryId: forget.GetId()}))
	wantCode(t, err, connect.CodeNotFound)
	if len(e.deletions.started) != 0 {
		t.Fatal("forgetting started a data deletion")
	}

	// Deleting the data needs the opt-in and the typed name.
	doomed := e.createRepo("Doomed")
	for _, name := range []string{"", "doomed", "wrong"} {
		_, err := c.DeleteRepository(ctx, connect.NewRequest(&syncrv1.DeleteRepositoryRequest{RepositoryId: doomed.GetId(), DeleteData: true, ConfirmName: name}))
		if codeOf(t, err) != connect.CodeInvalidArgument || violation(err, "confirm_name") != "type the repository's name to confirm" {
			t.Errorf("confirm_name %q: %v", name, err)
		}
	}
	_, err = c.DeleteRepository(ctx, connect.NewRequest(&syncrv1.DeleteRepositoryRequest{RepositoryId: doomed.GetId(), DeleteData: true, ConfirmName: "Doomed", WorkerId: "nope"}))
	if violation(err, "worker_id") == "" {
		t.Errorf("malformed worker: %v", err)
	}
	if got, _ := c.GetRepository(ctx, connect.NewRequest(&syncrv1.GetRepositoryRequest{RepositoryId: doomed.GetId()})); got.Msg.GetRepository().GetDeleting() {
		t.Fatal("a rejected request marked the repository")
	}

	resp, err = c.DeleteRepository(ctx, connect.NewRequest(&syncrv1.DeleteRepositoryRequest{RepositoryId: doomed.GetId(), DeleteData: true, ConfirmName: "Doomed"}))
	if err != nil || resp.Msg.GetDeletionJobId() == "" || resp.Msg.GetWorker().GetId() != e.worker.String() || resp.Msg.GetWorker().GetName() != "worker" {
		t.Fatalf("DeleteRepository with data = %v, %v", resp, err)
	}
	job := resp.Msg.GetDeletionJobId()
	if len(e.deletions.started) != 1 || e.deletions.started[0].String() != job {
		t.Errorf("started deletions = %v, want job %s", e.deletions.started, job)
	}
	// It stays listed, marked, until the job succeeds.
	got, err := c.GetRepository(ctx, connect.NewRequest(&syncrv1.GetRepositoryRequest{RepositoryId: doomed.GetId()}))
	if r := got.Msg.GetRepository(); err != nil || !r.GetDeleting() || r.GetDeletionJobId() != job || r.GetDeletionRequestedAt() == nil || r.GetDeletionError() != "" {
		t.Errorf("the deleting repository = %v, %v", r, err)
	}
	list, _ := c.ListRepositories(ctx, connect.NewRequest(&syncrv1.ListRepositoriesRequest{}))
	if rs := list.Msg.GetRepositories(); len(rs) != 1 || !rs[0].GetDeleting() {
		t.Errorf("ListRepositories while deleting = %v", rs)
	}
	// Repeating the request returns the same job.
	again, err := c.DeleteRepository(ctx, connect.NewRequest(&syncrv1.DeleteRepositoryRequest{RepositoryId: doomed.GetId(), DeleteData: true, ConfirmName: "Doomed"}))
	if err != nil || again.Msg.GetDeletionJobId() != job {
		t.Errorf("repeated request = %v, %v", again, err)
	}
	// Meanwhile the repository takes nothing new.
	for what, err := range map[string]error{
		"forget only": second(c.DeleteRepository(ctx, connect.NewRequest(&syncrv1.DeleteRepositoryRequest{RepositoryId: doomed.GetId()}))),
		"update":      second(c.UpdateRepository(ctx, connect.NewRequest(&syncrv1.UpdateRepositoryRequest{RepositoryId: doomed.GetId(), Version: 2, Name: "X"}))),
		"refresh":     second(c.ListSnapshots(ctx, connect.NewRequest(&syncrv1.ListSnapshotsRequest{RepositoryId: doomed.GetId(), Refresh: true}))),
		"browse":      second(c.BrowseSnapshot(ctx, connect.NewRequest(&syncrv1.BrowseSnapshotRequest{RepositoryId: doomed.GetId(), SnapshotId: "1a2b3c4d"}))),
		"run a job":   second(e.runAs(testUser, apiBackupDef(doomed.GetId()))),
	} {
		if codeOf(t, err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "data is being deleted") {
			t.Errorf("%s while deleting = %v, want FailedPrecondition", what, err)
		}
	}
	if _, err := c.ListSnapshots(ctx, connect.NewRequest(&syncrv1.ListSnapshotsRequest{RepositoryId: doomed.GetId()})); err != nil {
		t.Errorf("the cached snapshots are not served while deleting: %v", err)
	}
}

func second[T any](_ T, err error) error { return err }

func TestResticSnapshotsAndBrowse(t *testing.T) {
	t.Parallel()
	e := newResticEnv(t)
	ctx := t.Context()
	c := e.resticFor(testUser)
	r := e.createRepo("Photos vault")
	at := e.clock.Now().Add(-time.Hour)
	e.prober.snapshots = []restic.Snapshot{
		{ID: fmt.Sprintf("%064x", 1), Time: at.Add(-time.Hour), Hostname: "nas-01"},
		{ID: fmt.Sprintf("%064x", 2), Time: at, Hostname: "nas-01", Username: "root", Parent: fmt.Sprintf("%064x", 1), ProgramVersion: "restic 0.19.1",
			Paths: []string{`C:\Users\ada`}, Tags: []string{"nightly"},
			Summary: &restic.SnapshotSummary{BackupStart: at.Add(-time.Minute), BackupEnd: at, FilesNew: 3, DataAdded: 10, DataAddedPacked: 7, TotalFilesProcessed: 5, TotalBytesProcessed: 12345}},
	}

	// The first call lists through a worker by itself.
	resp, err := c.ListSnapshots(ctx, connect.NewRequest(&syncrv1.ListSnapshotsRequest{RepositoryId: r.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	m := resp.Msg
	if len(m.GetSnapshots()) != 2 || m.GetStale() || m.GetTruncated() || m.GetRefreshError() != "" || m.GetRefreshedAt() == nil ||
		m.GetWorker().GetId() != e.worker.String() || m.GetWorker().GetName() != "worker" {
		t.Fatalf("ListSnapshots = %v", m)
	}
	newest := m.GetSnapshots()[0]
	if newest.GetId() != fmt.Sprintf("%064x", 2) || newest.GetHost() != "nas-01" || newest.GetUsername() != "root" || newest.GetParent() != fmt.Sprintf("%064x", 1) ||
		newest.GetProgramVersion() != "restic 0.19.1" || newest.GetPaths()[0] != `C:\Users\ada` || newest.GetTags()[0] != "nightly" || !newest.GetTime().AsTime().Equal(at) ||
		newest.GetSummary().GetTotalBytesProcessed() != 12345 || newest.GetSummary().GetDataAddedPackedBytes() != 7 || newest.GetSummary().GetFilesNew() != 3 {
		t.Errorf("the newest snapshot = %v", newest)
	}
	if m.GetSnapshots()[1].GetSummary() != nil {
		t.Error("a snapshot without a summary has one")
	}
	noPassword(t, "ListSnapshotsResponse", m)
	// The statistics follow the cache.
	got, _ := c.GetRepository(ctx, connect.NewRequest(&syncrv1.GetRepositoryRequest{RepositoryId: r.GetId()}))
	if st := got.Msg.GetRepository().GetStats(); st.GetSnapshotCount() != 2 || st.GetLatestSnapshotBytes() != 12345 || st.GetSnapshotsStale() || !st.GetLatestSnapshotAt().AsTime().Equal(at) {
		t.Errorf("stats = %v", st)
	}

	// Then the cache is served; a failed refresh says why and keeps it.
	calls := e.prober.calls()
	resp, err = c.ListSnapshots(ctx, connect.NewRequest(&syncrv1.ListSnapshotsRequest{RepositoryId: r.GetId()}))
	if err != nil || e.prober.calls() != calls || resp.Msg.GetWorker() != nil || len(resp.Msg.GetSnapshots()) != 2 {
		t.Errorf("cached ListSnapshots = %v, %v", resp, err)
	}
	e.prober.fail(fmt.Errorf("%w: worker \"worker\" is offline", domain.ErrUnavailable))
	resp, err = c.ListSnapshots(ctx, connect.NewRequest(&syncrv1.ListSnapshotsRequest{RepositoryId: r.GetId(), Refresh: true, WorkerId: e.worker.String()}))
	if err != nil || len(resp.Msg.GetSnapshots()) != 2 || !strings.Contains(resp.Msg.GetRefreshError(), "is offline") {
		t.Errorf("failed refresh = %v, %v", resp, err)
	}
	if tgt := e.prober.lastTarget(); tgt.WorkerID != e.worker {
		t.Error("the chosen worker was not passed on")
	}
	_, err = c.ListSnapshots(ctx, connect.NewRequest(&syncrv1.ListSnapshotsRequest{RepositoryId: r.GetId(), WorkerId: "nope"}))
	wantCode(t, err, connect.CodeInvalidArgument)

	// Browsing: the worker's failures map like the probe service's.
	_, err = c.BrowseSnapshot(ctx, connect.NewRequest(&syncrv1.BrowseSnapshotRequest{RepositoryId: r.GetId(), SnapshotId: "1a2b3c4d", Path: "/"}))
	wantCode(t, err, connect.CodeUnavailable)
	e.prober.fail(&domain.ValidationError{Fields: []domain.FieldError{{Field: "path", Message: "does not exist"}}})
	_, err = c.BrowseSnapshot(ctx, connect.NewRequest(&syncrv1.BrowseSnapshotRequest{RepositoryId: r.GetId(), SnapshotId: "1a2b3c4d", Path: "/gone"}))
	if codeOf(t, err) != connect.CodeInvalidArgument || violation(err, "path") != "does not exist" {
		t.Errorf("a missing folder: %v", err)
	}
	e.prober.fail(nil)
	e.prober.listing = restic.Listing{Truncated: true, Entries: []restic.Entry{
		{Name: "photos", Path: "/srv/photos", Type: restic.EntryDir, Mode: "drwxr-xr-x", ModTime: at},
		{Name: "a b.txt", Path: "/srv/a b.txt", Type: restic.EntryFile, Size: 42},
		{Name: "link", Path: "/srv/link", Type: restic.EntrySymlink},
		{Name: "sock", Path: "/srv/sock", Type: restic.EntryOther},
	}}
	b, err := c.BrowseSnapshot(ctx, connect.NewRequest(&syncrv1.BrowseSnapshotRequest{RepositoryId: r.GetId(), SnapshotId: "1a2b3c4d", Path: "/srv", WorkerId: e.worker.String()}))
	if err != nil {
		t.Fatal(err)
	}
	want := []*syncrv1.ProbeEntry{
		{Name: "photos", Path: "/srv/photos", Type: syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_DIR, Mode: "drwxr-xr-x", ModTime: ts(at)},
		{Name: "a b.txt", Path: "/srv/a b.txt", Type: syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_FILE, Size: 42},
		{Name: "link", Path: "/srv/link", Type: syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_SYMLINK},
		{Name: "sock", Path: "/srv/sock", Type: syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_OTHER},
	}
	if b.Msg.GetPath() != "/srv" || !b.Msg.GetTruncated() || b.Msg.GetWorker().GetName() != "worker" || len(b.Msg.GetEntries()) != len(want) {
		t.Fatalf("BrowseSnapshot = %v", b.Msg)
	}
	for i, w := range want {
		if !proto.Equal(b.Msg.GetEntries()[i], w) {
			t.Errorf("entry %d = %v, want %v", i, b.Msg.GetEntries()[i], w)
		}
	}
	for _, tt := range []struct{ snapshot, path, field string }{
		{"latest", "/", "snapshot_id"}, {"", "/", "snapshot_id"}, {"1a2b3c4d", "srv", "path"}, {"1a2b3c4d", "/a/../b", "path"},
	} {
		_, err := c.BrowseSnapshot(ctx, connect.NewRequest(&syncrv1.BrowseSnapshotRequest{RepositoryId: r.GetId(), SnapshotId: tt.snapshot, Path: tt.path}))
		if codeOf(t, err) != connect.CodeInvalidArgument || violation(err, tt.field) == "" {
			t.Errorf("BrowseSnapshot(%q, %q) = %v, want InvalidArgument on %s", tt.snapshot, tt.path, err, tt.field)
		}
	}
}

func TestProbeProcedures(t *testing.T) {
	t.Parallel()
	slow := []string{
		syncrv1connect.ProbeServiceListWorkerFilesProcedure, syncrv1connect.ProbeServiceListRemoteFilesProcedure,
		syncrv1connect.ProbeServiceTestRemoteProcedure, syncrv1connect.ProbeServiceGetSftpHostKeysProcedure,
		syncrv1connect.ResticServiceCreateRepositoryProcedure,
		syncrv1connect.ResticServiceUpdateRepositoryProcedure, syncrv1connect.ResticServiceListSnapshotsProcedure,
		syncrv1connect.ResticServiceBrowseSnapshotProcedure,
		syncrv1connect.ProbeServiceMeasureWorkerFolderProcedure, syncrv1connect.ProbeServiceMeasureRemoteFolderProcedure,
		syncrv1connect.ProbeServiceGetRemoteUsageProcedure, syncrv1connect.ResticServiceMeasureSnapshotFolderProcedure,
		syncrv1connect.ResticServiceFindInRepositoryProcedure, syncrv1connect.ResticServiceCompareSnapshotsProcedure,
	}
	for _, p := range slow {
		if !probeProcedure(p) {
			t.Errorf("%s does not get the probe time limit", p)
		}
	}
	for _, p := range []string{
		syncrv1connect.ResticServiceListRepositoriesProcedure, syncrv1connect.ResticServiceGetRepositoryProcedure,
		syncrv1connect.ResticServiceDeleteRepositoryProcedure, syncrv1connect.JobServiceRunJobProcedure, "",
	} {
		if probeProcedure(p) {
			t.Errorf("%s gets the probe time limit", p)
		}
	}
	for probe, want := range map[time.Duration]time.Duration{0: 75 * time.Second, -time.Second: 75 * time.Second, 30 * time.Second: 45 * time.Second, 90 * time.Second: 105 * time.Second} {
		if got := probeCallTimeout(probe); got != want {
			t.Errorf("probeCallTimeout(%s) = %s, want %s", probe, got, want)
		}
	}
}

// slowProber answers Verify only when the request's deadline allows a
// probe that takes longer than the ordinary request timeout.
type slowProber struct {
	*apiProber
	deadlines chan time.Duration
}

func (p slowProber) Verify(ctx context.Context, owner domain.UserID, t restic.Target, init bool) (restic.VerifyResult, error) {
	if dl, ok := ctx.Deadline(); ok {
		p.deadlines <- time.Until(dl)
	} else {
		p.deadlines <- 0
	}
	return p.apiProber.Verify(ctx, owner, t, init)
}

// TestResticProbeCallsGetTheProbeTimeout: a call that waits for a worker
// is bounded by the probe time limit, not by the ordinary request timeout.
func TestResticProbeCallsGetTheProbeTimeout(t *testing.T) {
	t.Parallel()
	late := &lateRestic{}
	e := newEnv(t, func(d *Deps) {
		d.Restic = late
		d.RequestTimeout, d.ProbeTimeout = 2*time.Second, 80*time.Second
	})
	re := newResticServiceFor(t, e)
	deadlines := make(chan time.Duration, 1)
	svc, err := restic.New(restic.Deps{
		Repo: e.store, Remotes: fakeRemoteTypes{e.remotes}, Prober: slowProber{re.prober, deadlines}, Backends: re.backends,
		Cipher: re.cipher, Clock: e.clock, IDs: &idgen.Sequential{},
	}, restic.Config{})
	if err != nil {
		t.Fatal(err)
	}
	late.ResticService = svc
	remote := e.addRemote(testUser, "B2", "s3")
	_, err = e.resticFor(testUser).CreateRepository(t.Context(), connect.NewRequest(&syncrv1.CreateRepositoryRequest{Input: &syncrv1.RepositoryInput{
		Name: "Photos", RemoteId: remote.String(), Password: repoPassword, Mode: syncrv1.RepositoryCreateMode_REPOSITORY_CREATE_MODE_INIT,
	}}))
	if err != nil {
		t.Fatal(err)
	}
	// 80s + the 15s margin, minus what the call already used.
	if got := <-deadlines; got <= 60*time.Second || got > 95*time.Second {
		t.Errorf("the probe ran with %s left, want the probe time limit (95s)", got)
	}
}

// resticParts are the collaborators a restic service of the tests needs.
type resticParts struct {
	prober   *apiProber
	backends *backend.Registry
	cipher   *secrets.Keyring
}

func newResticServiceFor(t *testing.T, e *testEnv) resticParts {
	t.Helper()
	key := secrets.Key{ID: 1, Bytes: make([]byte, secrets.KeySize)}
	if _, err := crand.Read(key.Bytes); err != nil {
		t.Fatal(err)
	}
	ring, err := secrets.NewKeyring(key)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := backend.NewRegistry(builtin.Types()...)
	if err != nil {
		t.Fatal(err)
	}
	return resticParts{prober: &apiProber{resticID: apiResticID, worker: e.worker}, backends: reg, cipher: ring}
}

// apiBackupDef is a backup of /srv/photos into repository id.
func apiBackupDef(id string) *syncrv1.JobDefinition {
	return &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticBackup{ResticBackup: &syncrv1.ResticBackupDefinition{
		RepositoryId: id, Paths: []string{"/srv/photos"},
	}}}
}
