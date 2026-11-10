package restic_test

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/adapters/idgen"
	"github.com/Zeptiny/Syncr/internal/adapters/memstore"
	"github.com/Zeptiny/Syncr/internal/adapters/secrets"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/app/users"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/backend/builtin"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/platform/clock"
)

// sentinel is the repository password of the tests: it must never show up
// in an error or a log line.
const sentinel = "sentinel-repo-password-9f3"

const resticID = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"

var t0 = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

// fakeProber is a restic.Prober returning canned results and recording its
// calls.
type fakeProber struct {
	mu sync.Mutex
	// verifyErr, snapshotsErr and listErr fail the next calls.
	verifyErr, snapshotsErr, listErr error
	config                           restic.VerifyResult
	snapshots                        restic.SnapshotsResult
	listing                          restic.Listing
	// onSnapshots runs inside Snapshots (e.g. to advance the clock).
	onSnapshots func()

	verifies  []verifyCall
	snapCalls []restic.Target
	listCalls []listCall
}

type verifyCall struct {
	owner  domain.UserID
	target restic.Target
	init   bool
}

type listCall struct {
	target         restic.Target
	snapshot, path string
}

func (p *fakeProber) Verify(_ context.Context, owner domain.UserID, t restic.Target, init bool) (restic.VerifyResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.verifies = append(p.verifies, verifyCall{owner, t, init})
	if p.verifyErr != nil {
		return restic.VerifyResult{}, p.verifyErr
	}
	return p.config, nil
}

func (p *fakeProber) Snapshots(_ context.Context, _ domain.UserID, t restic.Target) (restic.SnapshotsResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.snapCalls = append(p.snapCalls, t)
	if p.onSnapshots != nil {
		p.onSnapshots()
	}
	if p.snapshotsErr != nil {
		return restic.SnapshotsResult{}, p.snapshotsErr
	}
	return p.snapshots, nil
}

func (p *fakeProber) List(_ context.Context, _ domain.UserID, t restic.Target, snapshot, path string) (restic.Listing, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.listCalls = append(p.listCalls, listCall{t, snapshot, path})
	if p.listErr != nil {
		return restic.Listing{}, p.listErr
	}
	return p.listing, nil
}

// fakeDeletions is a restic.Deletions recording its calls.
type fakeDeletions struct {
	mu                sync.Mutex
	pickErr, startErr error
	worker            domain.WorkerID
	picks             []domain.WorkerID // the workerID argument of each Pick
	starts            []startCall
	// started, if set, receives every Start.
	started chan startCall
}

type startCall struct {
	owner  domain.UserID
	repo   domain.ResticRepoID
	job    domain.JobID
	worker domain.WorkerID
}

func (d *fakeDeletions) Pick(_ context.Context, _ domain.UserID, _ domain.RemoteID, workerID domain.WorkerID) (domain.WorkerID, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.picks = append(d.picks, workerID)
	if d.pickErr != nil {
		return domain.WorkerID{}, d.pickErr
	}
	if !workerID.IsZero() {
		return workerID, nil
	}
	return d.worker, nil
}

func (d *fakeDeletions) Start(_ context.Context, owner domain.UserID, repo domain.ResticRepoID, job domain.JobID, worker domain.WorkerID) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	call := startCall{owner, repo, job, worker}
	d.starts = append(d.starts, call)
	if d.started != nil {
		d.started <- call
	}
	return d.startErr
}

// storeRemotes is the RemoteChecker over the store.
type storeRemotes struct{ store *memstore.Store }

func (r storeRemotes) RemoteType(ctx context.Context, owner domain.UserID, id domain.RemoteID) (string, error) {
	rem, err := r.store.GetRemote(ctx, id)
	if err != nil {
		return "", err
	}
	if rem.Deleted() || rem.OwnerID != owner {
		return "", fmt.Errorf("remote %s: %w", id, domain.ErrNotFound)
	}
	return rem.Type, nil
}

// flakyCipher is the real keyring, which can be made to fail decrypting.
type flakyCipher struct {
	*secrets.Keyring
	failDecrypt bool
}

func (c *flakyCipher) Decrypt(ct, ad []byte) ([]byte, error) {
	if c.failDecrypt {
		return nil, errors.New("decrypt failed")
	}
	return c.Keyring.Decrypt(ct, ad)
}

type env struct {
	t         *testing.T
	svc       *restic.Service
	store     *memstore.Store
	clock     *clock.Fake
	prober    *fakeProber
	deletions *fakeDeletions
	cipher    *flakyCipher
	ids       *idgen.Sequential
	logs      *bytes.Buffer

	alice, bob, admin domain.Actor
	// s3, local and union are alice's remotes; bobS3 is bob's.
	s3, local, union, bobS3 domain.RemoteID
	worker                  domain.WorkerID
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
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
	e := &env{
		t: t, store: memstore.New(), clock: clock.NewFake(t0), cipher: &flakyCipher{Keyring: ring},
		ids: &idgen.Sequential{}, logs: &bytes.Buffer{},
	}
	for i, a := range []*domain.Actor{&e.alice, &e.bob, &e.admin} {
		role := domain.RoleUser
		if i == 2 {
			role = domain.RoleAdmin
		}
		*a = domain.Actor{UserID: e.ids.NewUserID(), Role: role}
		u := users.User{ID: a.UserID, Username: []string{"alice", "bob", "admin"}[i], Role: role, PasswordHash: "x",
			CreatedAt: t0, UpdatedAt: t0, PasswordChangedAt: t0, Version: 1}
		if err := e.store.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	remote := func(owner domain.UserID, name, typ string) domain.RemoteID {
		id := e.ids.NewRemoteID()
		r := remotes.Remote{ID: id, OwnerID: owner, Name: name, Type: typ, SecretsEnc: []byte("enc"), CreatedAt: t0, UpdatedAt: t0, Version: 1}
		if err := e.store.CreateRemote(ctx, r); err != nil {
			t.Fatal(err)
		}
		return id
	}
	e.s3 = remote(e.alice.UserID, "B2 offsite", "s3")
	e.local = remote(e.alice.UserID, "NAS disk", "local")
	e.union = remote(e.alice.UserID, "Pool", "union")
	e.bobS3 = remote(e.bob.UserID, "Bob's bucket", "s3")
	e.worker = e.ids.NewWorkerID()
	if err := e.store.CreateWorker(ctx, workers.Worker{ID: e.worker, Name: "nas-01", Mode: domain.WorkerActive, CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	e.prober = &fakeProber{config: restic.VerifyResult{ResticID: resticID, FormatVersion: 2, WorkerID: e.worker, WorkerName: "nas-01"}}
	e.deletions = &fakeDeletions{worker: e.worker}
	e.svc, err = restic.New(restic.Deps{
		Repo: e.store, Remotes: storeRemotes{e.store}, Prober: e.prober, Backends: reg, Deletions: e.deletions,
		Cipher: e.cipher, Clock: e.clock, IDs: e.ids,
		Logger: slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}, restic.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if strings.Contains(e.logs.String(), sentinel) {
			t.Errorf("the log contains the repository password:\n%s", e.logs.String())
		}
	})
	return e
}

func input(remote domain.RemoteID) restic.Input {
	return restic.Input{Name: "Photos vault", Description: "family photos", RemoteID: remote, Path: "/restic//photos/", Password: sentinel, Mode: restic.ModeInit}
}

// create stores a repository of alice on her S3 remote.
func (e *env) create(mutate ...func(*restic.Input)) restic.Repo {
	e.t.Helper()
	in := input(e.s3)
	for _, m := range mutate {
		m(&in)
	}
	out, err := e.svc.Create(context.Background(), e.alice, in)
	if err != nil {
		e.t.Fatalf("Create: %v", err)
	}
	return out.Repo
}

func (e *env) get(id domain.ResticRepoID) restic.Repo {
	e.t.Helper()
	r, err := e.store.GetRepo(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

// field returns the fields of err, a validation error, as "field: message".
func fieldsOf(err error) []string {
	var verr *domain.ValidationError
	if !errors.As(err, &verr) {
		return nil
	}
	out := make([]string, len(verr.Fields))
	for i, f := range verr.Fields {
		out[i] = f.Field
	}
	return out
}

func noSecret(t *testing.T, err error) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), sentinel) {
		t.Errorf("the error contains the repository password: %v", err)
	}
}

func snap(n int, at time.Time) restic.Snapshot {
	return restic.Snapshot{ID: fmt.Sprintf("%064x", n), Time: at, Hostname: "nas-01", Paths: []string{"/srv/photos"},
		Summary: &restic.SnapshotSummary{TotalBytesProcessed: int64(n) * 1000}}
}

func TestNewRejectsMissingDependencies(t *testing.T) {
	t.Parallel()
	if _, err := restic.New(restic.Deps{}, restic.Config{}); err == nil {
		t.Error("New without dependencies succeeded")
	}
}

func TestCreateInit(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	out, err := e.svc.Create(ctx, e.alice, input(e.s3))
	if err != nil {
		t.Fatal(err)
	}
	r := out.Repo
	if r.Name != "Photos vault" || r.Path != "restic/photos" || r.ResticID != resticID || r.FormatVersion != 2 ||
		r.OwnerID != e.alice.UserID || r.Version != 1 || !r.CreatedAt.Equal(t0) || out.WorkerID != e.worker || out.WorkerName != "nas-01" {
		t.Errorf("Create = %+v", out)
	}
	if len(e.prober.verifies) != 1 {
		t.Fatalf("Verify calls = %d", len(e.prober.verifies))
	}
	call := e.prober.verifies[0]
	if !call.init || call.owner != e.alice.UserID || call.target.RemoteID != e.s3 || call.target.Path != "restic/photos" ||
		call.target.Password != sentinel || !call.target.RepositoryID.IsZero() || !call.target.WorkerID.IsZero() {
		t.Errorf("Verify call = init %v, target remote %s path %q", call.init, call.target.RemoteID, call.target.Path)
	}
	// The password is stored encrypted, bound to the repository.
	stored := e.get(r.ID)
	if bytes.Contains(stored.PasswordEnc, []byte(sentinel)) || len(stored.PasswordEnc) == 0 {
		t.Error("the password is not stored encrypted")
	}
	res, err := e.svc.Resolve(ctx, e.alice.UserID, r.ID)
	if err != nil || res.Password != sentinel || res.Path != "restic/photos" || res.RemoteID != e.s3 || res.ResticID != resticID || res.Name != r.Name {
		t.Errorf("Resolve = (%v), password ok %v", err, res.Password == sentinel)
	}
	other := domain.NewResticRepoID()
	if _, err := e.cipher.Keyring.Decrypt(stored.PasswordEnc, restic.PasswordAD(other)); err == nil {
		t.Error("the password decrypts under another repository's associated data")
	}
}

func TestCreateConnectAndLocal(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	in := input(e.local)
	in.Mode, in.Password, in.WorkerID, in.Path = restic.ModeConnect, "short", e.worker, ""
	out, err := e.svc.Create(ctx, e.alice, in)
	if err != nil {
		t.Fatal(err)
	}
	call := e.prober.verifies[0]
	if call.init || call.target.WorkerID != e.worker || call.target.Path != "" || out.Repo.Path != "" {
		t.Errorf("connect on a local remote: verify init=%v worker=%s, repo path %q", call.init, call.target.WorkerID, out.Repo.Path)
	}
}

func TestCreateValidation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	tests := []struct {
		name   string
		mutate func(*restic.Input)
		fields []string
	}{
		{"no name", func(in *restic.Input) { in.Name = "  " }, []string{"name"}},
		{"long name", func(in *restic.Input) { in.Name = strings.Repeat("n", 65) }, []string{"name"}},
		{"control character in the name", func(in *restic.Input) { in.Name = "a\nb" }, []string{"name"}},
		{"long description", func(in *restic.Input) { in.Description = strings.Repeat("d", 501) }, []string{"description"}},
		{"dot-dot path", func(in *restic.Input) { in.Path = "a/../b" }, []string{"path"}},
		{"backslash path", func(in *restic.Input) { in.Path = `a\b` }, []string{"path"}},
		{"no mode", func(in *restic.Input) { in.Mode = "" }, []string{"mode"}},
		{"unknown mode", func(in *restic.Input) { in.Mode = "import" }, []string{"mode"}},
		{"no password", func(in *restic.Input) { in.Password = "" }, []string{"password"}},
		{"short password for init", func(in *restic.Input) { in.Password = "1234567" }, []string{"password"}},
		{"no remote", func(in *restic.Input) { in.RemoteID = domain.RemoteID{} }, []string{"remote_id"}},
		{"unknown remote", func(in *restic.Input) { in.RemoteID = domain.NewRemoteID() }, []string{"remote_id"}},
		{"another user's remote", func(in *restic.Input) { in.RemoteID = e.bobS3 }, []string{"remote_id"}},
		{"a remote that cannot hold repositories", func(in *restic.Input) { in.RemoteID = e.union }, []string{"remote_id"}},
		{"local remote without a worker", func(in *restic.Input) { in.RemoteID = e.local }, []string{"worker_id"}},
		{"several problems", func(in *restic.Input) { in.Name, in.Password, in.Path = "", "", ".." }, []string{"name", "path", "password"}},
	}
	for _, tt := range tests {
		in := input(e.s3)
		tt.mutate(&in)
		_, err := e.svc.Create(ctx, e.alice, in)
		if got := fieldsOf(err); !slices.Equal(got, tt.fields) {
			t.Errorf("%s: Create = %v (fields %q), want %q", tt.name, err, got, tt.fields)
		}
		noSecret(t, err)
	}
	if len(e.prober.verifies) != 0 {
		t.Errorf("invalid input reached the worker %d times", len(e.prober.verifies))
	}
	if _, err := e.svc.Create(ctx, domain.Actor{}, input(e.s3)); !errors.Is(err, domain.ErrPermissionDenied) {
		t.Errorf("Create without a user = %v", err)
	}
}

func TestCreateMapsProbeErrors(t *testing.T) {
	t.Parallel()
	probe := func(code string) error {
		// As the controller's runner reports it: a domain error and the
		// worker's error, whose message echoes the password.
		return fmt.Errorf("read the repository: %w", errors.Join(
			fmt.Errorf("%w: mapped", domain.ErrFailedPrecondition),
			&restic.ProbeError{Code: code, Message: "restic said: " + sentinel},
		))
	}
	tests := []struct {
		name  string
		mode  restic.Mode
		err   error
		field string
		class error
	}{
		{"wrong password", restic.ModeConnect, probe("wrong_password"), "password", nil},
		{"no repository", restic.ModeConnect, probe("repository_not_found"), "path", nil},
		{"already exists", restic.ModeInit, probe("already_exists"), "mode", nil},
		{"outside the roots", restic.ModeInit, probe("path_not_allowed"), "worker_id", nil},
		{"another failure", restic.ModeInit, probe("execution_failed"), "", domain.ErrFailedPrecondition},
		{"offline worker", restic.ModeConnect, fmt.Errorf("%w: worker is offline", domain.ErrUnavailable), "", domain.ErrUnavailable},
		{"rate limited", restic.ModeConnect, fmt.Errorf("%w: slow down", domain.ErrRateLimited), "", domain.ErrRateLimited},
		{"the runner's own field error", restic.ModeConnect, &domain.ValidationError{Fields: []domain.FieldError{{Field: "worker_id", Message: "choose"}}}, "worker_id", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			e.prober.verifyErr = tt.err
			in := input(e.s3)
			in.Mode = tt.mode
			_, err := e.svc.Create(context.Background(), e.alice, in)
			noSecret(t, err)
			if tt.field != "" {
				if got := fieldsOf(err); !slices.Equal(got, []string{tt.field}) {
					t.Errorf("Create = %v, want a problem on %s", err, tt.field)
				}
			} else if !errors.Is(err, tt.class) || fieldsOf(err) != nil {
				t.Errorf("Create = %v, want %v", err, tt.class)
			}
			if list, _ := e.svc.List(context.Background(), e.alice, restic.ListFilter{}); len(list) != 0 {
				t.Errorf("a failed Create stored %d repositories", len(list))
			}
		})
	}
}

func TestCreateUniqueness(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	first := e.create()
	calls := len(e.prober.verifies)

	// A taken name or location never reaches the worker.
	_, err := e.svc.Create(ctx, e.alice, restic.Input{Name: "PHOTOS VAULT", RemoteID: e.s3, Path: "elsewhere", Password: sentinel, Mode: restic.ModeInit})
	if !errors.Is(err, restic.ErrNameTaken) || !errors.Is(err, domain.ErrAlreadyExists) {
		t.Errorf("same name = %v, want ErrNameTaken", err)
	}
	_, err = e.svc.Create(ctx, e.alice, restic.Input{Name: "Other", RemoteID: e.s3, Path: "restic/photos/", Password: sentinel, Mode: restic.ModeConnect})
	if !errors.Is(err, restic.ErrLocationTaken) {
		t.Errorf("same location = %v, want ErrLocationTaken", err)
	}
	if len(e.prober.verifies) != calls {
		t.Errorf("a doomed Create asked the worker")
	}
	// The same restic repository through another location.
	_, err = e.svc.Create(ctx, e.alice, restic.Input{Name: "Other", RemoteID: e.s3, Path: "alias", Password: sentinel, Mode: restic.ModeConnect})
	if !errors.Is(err, restic.ErrSameRepository) {
		t.Errorf("same restic ID = %v, want ErrSameRepository", err)
	}
	// Bob may use the same name; all are free again after a delete.
	if _, err := e.svc.Create(ctx, e.bob, restic.Input{Name: "Photos vault", RemoteID: e.bobS3, Path: "restic/photos", Password: sentinel, Mode: restic.ModeConnect}); err != nil {
		t.Errorf("another owner with the same name and restic ID: %v", err)
	}
	if _, err := e.svc.Delete(ctx, e.alice, first.ID, restic.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Create(ctx, e.alice, input(e.s3)); err != nil {
		t.Errorf("Create after the delete: %v", err)
	}
}

func TestCreateRejectsABadResticID(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.prober.config.ResticID = "not-hex"
	if _, err := e.svc.Create(context.Background(), e.alice, input(e.s3)); !errors.Is(err, domain.ErrUnavailable) {
		t.Errorf("Create = %v, want ErrUnavailable", err)
	}
}

func TestOwnerScoping(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	r := e.create()
	if err := e.store.ReplaceSnapshots(ctx, r.ID, []restic.Snapshot{snap(1, t0)}, false, t0); err != nil {
		t.Fatal(err)
	}
	calls := func() int {
		return len(e.prober.verifies) + len(e.prober.snapCalls) + len(e.prober.listCalls) + len(e.deletions.picks) + len(e.deletions.starts)
	}
	before := calls()
	for _, actor := range []domain.Actor{e.bob, e.admin, {}} {
		checks := map[string]error{}
		_, checks["Get"] = e.svc.Get(ctx, actor, r.ID)
		_, checks["Update"] = e.svc.Update(ctx, actor, r.ID, r.Version, restic.Update{Name: "Stolen", Password: "new-password"})
		_, checks["Delete"] = e.svc.Delete(ctx, actor, r.ID, restic.DeleteOptions{})
		_, checks["Delete with data"] = e.svc.Delete(ctx, actor, r.ID, restic.DeleteOptions{DeleteData: true, ConfirmName: r.Name})
		_, checks["Snapshots"] = e.svc.Snapshots(ctx, actor, r.ID, restic.SnapshotsOptions{})
		_, checks["Snapshots refresh"] = e.svc.Snapshots(ctx, actor, r.ID, restic.SnapshotsOptions{Refresh: true})
		_, checks["Browse"] = e.svc.Browse(ctx, actor, r.ID, "1a2b3c4d", "/", domain.WorkerID{})
		_, _, checks["UsableRepository"] = e.svc.UsableRepository(ctx, actor.UserID, r.ID, domain.JobID{})
		_, checks["Resolve"] = e.svc.Resolve(ctx, actor.UserID, r.ID)
		for what, err := range checks {
			if !errors.Is(err, domain.ErrNotFound) {
				t.Errorf("%s as %s %s = %v, want ErrNotFound", what, actor.Role, actor.UserID, err)
			}
		}
		if list, err := e.svc.List(ctx, actor, restic.ListFilter{}); err != nil || len(list) != 0 {
			t.Errorf("List as %s = %v, %v", actor.Role, list, err)
		}
	}
	if calls() != before {
		t.Error("a foreign request reached the worker or the deleter")
	}
	if got := e.get(r.ID); got.Name != r.Name || got.Deleted() || got.Deleting || got.Version != 1 {
		t.Errorf("the repository changed: %+v", got)
	}
	// Names is for display on objects the caller already sees.
	names, err := e.svc.Names(ctx, []domain.ResticRepoID{r.ID, domain.NewResticRepoID()})
	if err != nil || len(names) != 1 || names[r.ID].Name != r.Name || names[r.ID].OwnerID != e.alice.UserID || names[r.ID].Deleted {
		t.Errorf("Names = %+v, %v", names, err)
	}
	if _, err := e.svc.Get(ctx, e.alice, domain.ResticRepoID{}); !errors.Is(err, domain.ErrInvalidID) {
		t.Errorf("Get(zero) = %v", err)
	}
}

func TestListFiltersByRemote(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	a := e.create()
	e.prober.config.ResticID = strings.Repeat("b", 64)
	b := e.create(func(in *restic.Input) { in.Name, in.RemoteID, in.WorkerID, in.Path = "Archive", e.local, e.worker, "x" })
	all, err := e.svc.List(ctx, e.alice, restic.ListFilter{OwnerID: e.bob.UserID})
	if err != nil || len(all) != 2 || all[0].ID != b.ID || all[1].ID != a.ID {
		t.Errorf("List = %v, %v; want Archive then Photos vault (the owner filter is forced)", all, err)
	}
	one, err := e.svc.List(ctx, e.alice, restic.ListFilter{RemoteID: e.local})
	if err != nil || len(one) != 1 || one[0].ID != b.ID {
		t.Errorf("List(remote) = %v, %v", one, err)
	}
}

func TestUpdate(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	r := e.create()
	e.clock.Advance(time.Hour)

	// Rename only: no worker involved, the password is kept.
	got, err := e.svc.Update(ctx, e.alice, r.ID, 1, restic.Update{Name: " Family ", Description: "new"})
	if err != nil || got.Name != "Family" || got.Description != "new" || got.Version != 2 || !got.UpdatedAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("Update = %+v, %v", got, err)
	}
	if len(e.prober.verifies) != 1 {
		t.Error("a rename asked the worker")
	}
	if res, err := e.svc.Resolve(ctx, e.alice.UserID, r.ID); err != nil || res.Password != sentinel {
		t.Errorf("the password changed with a rename: %v", err)
	}

	// A stale version.
	if _, err := e.svc.Update(ctx, e.alice, r.ID, 1, restic.Update{Name: "X"}); !errors.Is(err, domain.ErrConflict) {
		t.Errorf("stale Update = %v", err)
	}
	// Validation.
	_, err = e.svc.Update(ctx, e.alice, r.ID, 2, restic.Update{Name: "", Password: "a\x00b"})
	if got := fieldsOf(err); !slices.Equal(got, []string{"name", "password"}) {
		t.Errorf("invalid Update = %v", err)
	}

	// A new password is verified with cat config on the stored location.
	const next = "sentinel-repo-password-next"
	got, err = e.svc.Update(ctx, e.alice, r.ID, 2, restic.Update{Name: "Family", Password: next, WorkerID: e.worker})
	if err != nil || got.Version != 3 {
		t.Fatalf("Update(password) = %+v, %v", got, err)
	}
	call := e.prober.verifies[len(e.prober.verifies)-1]
	if call.init || call.target.Password != next || call.target.RepositoryID != r.ID || call.target.Path != "restic/photos" || call.target.WorkerID != e.worker {
		t.Errorf("verify call for the new password: %+v", call.init)
	}
	if res, err := e.svc.Resolve(ctx, e.alice.UserID, r.ID); err != nil || res.Password != next {
		t.Errorf("the new password is not stored: %v", err)
	}

	// A wrong one is a field error and changes nothing.
	e.prober.verifyErr = errors.Join(domain.ErrFailedPrecondition, &restic.ProbeError{Code: "wrong_password", Message: "wrong " + sentinel})
	_, err = e.svc.Update(ctx, e.alice, r.ID, 3, restic.Update{Name: "Family", Password: sentinel})
	if got := fieldsOf(err); !slices.Equal(got, []string{"password"}) {
		t.Errorf("wrong password = %v", err)
	}
	noSecret(t, err)
	// A password that opens another repository is refused.
	e.prober.verifyErr = nil
	e.prober.config.ResticID = strings.Repeat("c", 64)
	_, err = e.svc.Update(ctx, e.alice, r.ID, 3, restic.Update{Name: "Family", Password: sentinel})
	if !errors.Is(err, domain.ErrFailedPrecondition) {
		t.Errorf("another repository's password = %v", err)
	}
	if res, _ := e.svc.Resolve(ctx, e.alice.UserID, r.ID); res.Password != next || e.get(r.ID).Version != 3 {
		t.Error("a refused password was stored")
	}

	// A taken name.
	e.prober.config.ResticID = strings.Repeat("d", 64)
	e.create(func(in *restic.Input) { in.Name, in.Path = "Archive", "archive" })
	if _, err := e.svc.Update(ctx, e.alice, r.ID, 3, restic.Update{Name: "archive"}); !errors.Is(err, restic.ErrNameTaken) {
		t.Errorf("taken name = %v", err)
	}
	// Changing only the case of its own name is fine.
	if _, err := e.svc.Update(ctx, e.alice, r.ID, 3, restic.Update{Name: "FAMILY"}); err != nil {
		t.Errorf("recasing = %v", err)
	}
}

func TestForgetOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	r := e.create()
	if err := e.store.ReplaceSnapshots(ctx, r.ID, []restic.Snapshot{snap(1, t0)}, false, t0); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Hour)
	// The typed name is not needed to forget.
	res, err := e.svc.Delete(ctx, e.alice, r.ID, restic.DeleteOptions{ConfirmName: "whatever"})
	if err != nil || res != (restic.DeleteResult{}) {
		t.Fatalf("Delete = %+v, %v", res, err)
	}
	got := e.get(r.ID)
	if !got.DeletedAt.Equal(t0.Add(time.Hour)) || got.PasswordEnc != nil || !got.DataDeletedAt.IsZero() || got.Deleting {
		t.Errorf("after forgetting: %+v", got)
	}
	if list, _ := e.store.ListSnapshots(ctx, r.ID); len(list.Snapshots) != 0 {
		t.Error("the snapshot cache survived")
	}
	if len(e.deletions.picks)+len(e.deletions.starts) != 0 {
		t.Error("forgetting started a data deletion")
	}
	for what, err := range map[string]error{
		"Get":     second(e.svc.Get(ctx, e.alice, r.ID)),
		"Delete":  second(e.svc.Delete(ctx, e.alice, r.ID, restic.DeleteOptions{})),
		"Resolve": second(e.svc.Resolve(ctx, e.alice.UserID, r.ID)),
	} {
		if !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("%s of a forgotten repository = %v", what, err)
		}
	}
	if _, _, err := e.svc.UsableRepository(ctx, e.alice.UserID, r.ID, domain.JobID{}); !errors.Is(err, restic.ErrUnknownRepository) {
		t.Errorf("UsableRepository(forgotten) = %v", err)
	}
	// Job history still names it.
	if names, _ := e.svc.Names(ctx, []domain.ResticRepoID{r.ID}); !names[r.ID].Deleted || names[r.ID].Name != r.Name {
		t.Errorf("Names(forgotten) = %+v", names)
	}
}

func second[T any](_ T, err error) error { return err }

func TestDeleteWithData(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	r := e.create()
	// A schedule on the repository, to see it paused.
	sc := schedules.Schedule{
		ID: e.ids.NewScheduleID(), OwnerID: e.alice.UserID, Name: "Nightly", Cron: "0 2 * * *", Timezone: "UTC",
		Kind: "restic_backup", Definition: []byte(`{}`), Target: schedules.Target{WorkerID: e.worker},
		Missed: schedules.MissedPolicy{Action: schedules.MissedSkip}, Overlap: schedules.OverlapSkip,
		Enabled: true, NextRunAt: t0.Add(time.Hour), RepositoryID: r.ID, CreatedAt: t0, UpdatedAt: t0, Version: 1,
	}
	if err := e.store.CreateSchedule(ctx, sc); err != nil {
		t.Fatal(err)
	}

	// The confirmation is enforced here, not only in the UI.
	for _, name := range []string{"", "photos vault", "Photos vault "} {
		_, err := e.svc.Delete(ctx, e.alice, r.ID, restic.DeleteOptions{DeleteData: true, ConfirmName: name})
		if got := fieldsOf(err); !slices.Equal(got, []string{"confirm_name"}) {
			t.Errorf("confirm %q = %v, want a problem on confirm_name", name, err)
		}
	}
	// A failed pick changes nothing.
	e.deletions.pickErr = fmt.Errorf("%w: no worker can delete the data", domain.ErrFailedPrecondition)
	if _, err := e.svc.Delete(ctx, e.alice, r.ID, restic.DeleteOptions{DeleteData: true, ConfirmName: r.Name}); !errors.Is(err, domain.ErrFailedPrecondition) {
		t.Errorf("failed pick = %v", err)
	}
	e.deletions.pickErr = &domain.ValidationError{Fields: []domain.FieldError{{Field: "worker_id", Message: "choose the worker"}}}
	_, err := e.svc.Delete(ctx, e.alice, r.ID, restic.DeleteOptions{DeleteData: true, ConfirmName: r.Name})
	if got := fieldsOf(err); !slices.Equal(got, []string{"worker_id"}) {
		t.Errorf("a remote needing a worker = %v", err)
	}
	if got := e.get(r.ID); got.Deleting || got.Version != 1 || len(e.deletions.starts) != 0 {
		t.Fatalf("a rejected request changed the repository: %+v", got)
	}
	e.deletions.pickErr = nil

	e.clock.Advance(time.Minute)
	res, err := e.svc.Delete(ctx, e.alice, r.ID, restic.DeleteOptions{DeleteData: true, ConfirmName: r.Name})
	if err != nil || res.DeletionJobID.IsZero() || res.WorkerID != e.worker {
		t.Fatalf("Delete with data = %+v, %v", res, err)
	}
	got := e.get(r.ID)
	if !got.Deleting || got.DeletionJobID != res.DeletionJobID || !got.DeletionRequestedAt.Equal(t0.Add(time.Minute)) || got.Deleted() || got.PasswordEnc == nil {
		t.Errorf("after the request: %+v", got)
	}
	want := startCall{e.alice.UserID, r.ID, res.DeletionJobID, e.worker}
	if len(e.deletions.starts) != 1 || e.deletions.starts[0] != want {
		t.Errorf("Start calls = %+v, want %+v", e.deletions.starts, want)
	}
	if s, _ := e.store.GetSchedule(ctx, sc.ID); s.Enabled || s.PausedReason != schedules.ReasonRepositoryDeleting || !s.NextRunAt.IsZero() {
		t.Errorf("the schedule is not paused: %+v", s)
	}

	// While deleting: listed, and closed for everything but its own job.
	if list, err := e.svc.List(ctx, e.alice, restic.ListFilter{}); err != nil || len(list) != 1 || !list[0].Deleting {
		t.Errorf("List while deleting = %+v, %v", list, err)
	}
	if _, _, err := e.svc.UsableRepository(ctx, e.alice.UserID, r.ID, domain.JobID{}); !errors.Is(err, restic.ErrRepositoryDeleting) {
		t.Errorf("UsableRepository(no job) = %v", err)
	}
	if _, _, err := e.svc.UsableRepository(ctx, e.alice.UserID, r.ID, e.ids.NewJobID()); !errors.Is(err, restic.ErrRepositoryDeleting) {
		t.Errorf("UsableRepository(another job) = %v", err)
	}
	if name, remote, err := e.svc.UsableRepository(ctx, e.alice.UserID, r.ID, res.DeletionJobID); err != nil || name != r.Name || remote != e.s3 {
		t.Errorf("UsableRepository(the deletion job) = %q, %s, %v", name, remote, err)
	}
	// Its jobs still get their spec.
	if rr, err := e.svc.Resolve(ctx, e.alice.UserID, r.ID); err != nil || rr.Password != sentinel {
		t.Errorf("Resolve while deleting: %v", err)
	}
	blocked := map[string]error{
		"Update":            second(e.svc.Update(ctx, e.alice, r.ID, got.Version, restic.Update{Name: "X"})),
		"Snapshots refresh": second(e.svc.Snapshots(ctx, e.alice, r.ID, restic.SnapshotsOptions{Refresh: true})),
		"Browse":            second(e.svc.Browse(ctx, e.alice, r.ID, "1a2b3c4d", "/", domain.WorkerID{})),
		"forget only":       second(e.svc.Delete(ctx, e.alice, r.ID, restic.DeleteOptions{})),
	}
	for what, err := range blocked {
		if !errors.Is(err, restic.ErrRepositoryDeleting) || !errors.Is(err, domain.ErrFailedPrecondition) {
			t.Errorf("%s while deleting = %v, want ErrRepositoryDeleting", what, err)
		}
	}
	if err := blocked["forget only"]; !strings.Contains(err.Error(), "cancel job "+res.DeletionJobID.String()+" first") {
		t.Errorf("forget only does not name the job: %v", err)
	}
	// The cache is still served, without asking a worker.
	if list, err := e.svc.Snapshots(ctx, e.alice, r.ID, restic.SnapshotsOptions{}); err != nil || len(e.prober.snapCalls) != 0 || !list.Stale {
		t.Errorf("Snapshots while deleting = %+v, %v", list, err)
	}

	// A repeated request returns the same job and queues it again.
	again, err := e.svc.Delete(ctx, e.alice, r.ID, restic.DeleteOptions{DeleteData: true, ConfirmName: r.Name})
	if err != nil || again.DeletionJobID != res.DeletionJobID || !again.WorkerID.IsZero() {
		t.Errorf("repeated request = %+v, %v", again, err)
	}
	if len(e.deletions.starts) != 2 || e.deletions.starts[1].job != res.DeletionJobID {
		t.Errorf("the repeated request did not restart the same job: %+v", e.deletions.starts)
	}
	// Even when no worker can be picked right now.
	e.deletions.pickErr = fmt.Errorf("%w: offline", domain.ErrUnavailable)
	again, err = e.svc.Delete(ctx, e.alice, r.ID, restic.DeleteOptions{DeleteData: true, ConfirmName: r.Name})
	if err != nil || again.DeletionJobID != res.DeletionJobID {
		t.Errorf("repeated request without a worker = %+v, %v", again, err)
	}
	if e.get(r.ID).Version != got.Version {
		t.Error("a repeated request changed the repository")
	}
}

func TestDeleteWithDataStartFailures(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	r := e.create()
	opts := restic.DeleteOptions{DeleteData: true, ConfirmName: r.Name}

	// Transient: the repository stays deleting and a retry reuses the job.
	e.deletions.startErr = errors.New("the store is unreachable")
	if _, err := e.svc.Delete(ctx, e.alice, r.ID, opts); err == nil {
		t.Fatal("a failed Start was not reported")
	}
	got := e.get(r.ID)
	if !got.Deleting || got.DeletionJobID.IsZero() {
		t.Fatalf("after a transient failure: %+v", got)
	}
	e.deletions.startErr = nil
	res, err := e.svc.Delete(ctx, e.alice, r.ID, opts)
	if err != nil || res.DeletionJobID != got.DeletionJobID {
		t.Fatalf("retry = %+v, %v; want job %s", res, err, got.DeletionJobID)
	}
	if n := len(e.deletions.starts); n != 2 || e.deletions.starts[0].job != e.deletions.starts[1].job {
		t.Errorf("Start calls = %+v", e.deletions.starts)
	}

	// Permanent: the repository is active again, with the reason.
	e2 := newEnv(t)
	r2 := e2.create()
	e2.deletions.startErr = fmt.Errorf("%w: worker was removed", domain.ErrNotFound)
	if _, err := e2.svc.Delete(ctx, e2.alice, r2.ID, restic.DeleteOptions{DeleteData: true, ConfirmName: r2.Name}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("permanent failure = %v", err)
	}
	got = e2.get(r2.ID)
	if got.Deleting || got.Deleted() || got.PasswordEnc == nil || !strings.Contains(got.DeletionError, "could not be started") || got.DeletionJobID.IsZero() {
		t.Errorf("after a permanent failure: %+v", got)
	}
	// Try again: a new job.
	e2.deletions.startErr = nil
	res2, err := e2.svc.Delete(ctx, e2.alice, r2.ID, restic.DeleteOptions{DeleteData: true, ConfirmName: r2.Name})
	if err != nil || res2.DeletionJobID == got.DeletionJobID {
		t.Errorf("second attempt = %+v, %v; want a new job", res2, err)
	}
	if now := e2.get(r2.ID); !now.Deleting || now.DeletionError != "" {
		t.Errorf("second attempt left %+v", now)
	}
}

func TestDeleteWithDataUnavailable(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	r := e.create()
	svc, err := restic.New(restic.Deps{
		Repo: e.store, Remotes: storeRemotes{e.store}, Prober: e.prober, Backends: mustRegistry(t),
		Cipher: e.cipher, Clock: e.clock, IDs: e.ids,
	}, restic.Config{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Delete(context.Background(), e.alice, r.ID, restic.DeleteOptions{DeleteData: true, ConfirmName: r.Name})
	if !errors.Is(err, domain.ErrFailedPrecondition) || !strings.Contains(err.Error(), "data deletion is not available") {
		t.Errorf("Delete with data without a deleter = %v", err)
	}
	if e.get(r.ID).Deleting {
		t.Error("the repository was marked")
	}
}

func mustRegistry(t *testing.T) *backend.Registry {
	t.Helper()
	reg, err := backend.NewRegistry(builtin.Types()...)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// deletionJob stores the deletion job of r in status st.
func (e *env) deletionJob(r restic.Repo, st domain.JobStatus, jerr jobs.JobError) {
	e.t.Helper()
	j := jobs.Job{
		ID: r.DeletionJobID, WorkerID: e.worker, OwnerID: r.OwnerID, Kind: restic.KindDeleteData, Status: st,
		Definition: []byte(`{}`), RepositoryID: r.ID, CreatedAt: e.clock.Now(), QueuedAt: e.clock.Now(), Error: jerr, ExitCode: -1, Version: 1,
	}
	if st.Terminal() {
		j.FinishedAt = e.clock.Now()
	}
	if err := e.store.CreateJob(context.Background(), j); err != nil {
		e.t.Fatal(err)
	}
}

// deleting returns a repository whose data deletion was requested.
func (e *env) deleting(name string) restic.Repo {
	e.t.Helper()
	e.prober.config.ResticID = fmt.Sprintf("%x", sha256.Sum256([]byte(name)))
	r := e.create(func(in *restic.Input) { in.Name, in.Path = name, name })
	if _, err := e.svc.Delete(context.Background(), e.alice, r.ID, restic.DeleteOptions{DeleteData: true, ConfirmName: name}); err != nil {
		e.t.Fatal(err)
	}
	return e.get(r.ID)
}

func TestFinishDeletions(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	ok, failed, cancelled, interrupted, running, missing := e.deleting("ok"), e.deleting("failed"), e.deleting("cancelled"),
		e.deleting("interrupted"), e.deleting("running"), e.deleting("missing")
	e.deletionJob(ok, domain.JobSucceeded, jobs.JobError{})
	e.deletionJob(failed, domain.JobFailed, jobs.JobError{Code: "repository_locked", Message: "another restic client is using the repository; nothing was deleted"})
	e.deletionJob(cancelled, domain.JobCancelled, jobs.JobError{Code: "cancelled"})
	e.deletionJob(interrupted, domain.JobInterrupted, jobs.JobError{Code: "worker_lost", Message: "the worker did not come back"})
	e.deletionJob(running, domain.JobRunning, jobs.JobError{})
	if err := e.store.ReplaceSnapshots(ctx, ok.ID, []restic.Snapshot{snap(1, t0)}, false, t0); err != nil {
		t.Fatal(err)
	}

	e.clock.Advance(time.Minute)
	n, err := e.svc.FinishDeletions(ctx)
	if err != nil || n != 4 {
		t.Fatalf("FinishDeletions = %d, %v; want 4", n, err)
	}
	// Success: only now is the repository deleted and the password gone.
	got := e.get(ok.ID)
	if !got.Deleted() || !got.DataDeletedAt.Equal(t0.Add(time.Minute)) || got.PasswordEnc != nil || got.Deleting {
		t.Errorf("after a succeeded deletion: %+v", got)
	}
	if _, err := e.svc.Get(ctx, e.alice, ok.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Get(deleted with data) = %v", err)
	}
	for r, want := range map[*restic.Repo]string{
		&failed:      "deleting the data failed: repository_locked: another restic client is using the repository; nothing was deleted",
		&cancelled:   "the deletion was cancelled; some repository files may already be gone",
		&interrupted: "deleting the data failed: worker_lost: the worker did not come back",
	} {
		got := e.get(r.ID)
		if got.Deleting || got.Deleted() || got.PasswordEnc == nil || got.DeletionError != want || got.DeletionJobID != r.DeletionJobID {
			t.Errorf("%s: after the job ended: deleting=%v deleted=%v error=%q", r.Name, got.Deleting, got.Deleted(), got.DeletionError)
		}
	}
	// Running and (still within the grace period) missing jobs wait.
	for _, r := range []restic.Repo{running, missing} {
		if got := e.get(r.ID); !got.Deleting {
			t.Errorf("%s was ended early: %+v", r.Name, got)
		}
	}
	if n, err := e.svc.FinishDeletions(ctx); err != nil || n != 0 {
		t.Errorf("a second pass = %d, %v", n, err)
	}

	// A job that was never created is given up after the grace period.
	e.clock.Advance(restic.DeletionGrace)
	if n, err := e.svc.FinishDeletions(ctx); err != nil || n != 1 {
		t.Fatalf("after the grace period = %d, %v", n, err)
	}
	if got := e.get(missing.ID); got.Deleting || got.DeletionError != "the deletion job was never created; try again" || got.PasswordEnc == nil {
		t.Errorf("after the grace period: %+v", got)
	}
	if !e.get(running.ID).Deleting {
		t.Error("a running deletion was given up")
	}
	// The password of a repository is intact after a failed deletion.
	if rr, err := e.svc.Resolve(ctx, e.alice.UserID, failed.ID); err != nil || rr.Password != sentinel {
		t.Errorf("Resolve after a failed deletion: %v", err)
	}
}

// conflictRepo makes FinishDeletion lose its compare-and-swap.
type conflictRepo struct {
	restic.Repository
	finishes int
}

func (c *conflictRepo) FinishDeletion(context.Context, domain.ResticRepoID, domain.JobID, restic.DeletionOutcome, time.Time) error {
	c.finishes++
	return fmt.Errorf("lost the race: %w", domain.ErrConflict)
}

func TestFinishDeletionsSkipsConflicts(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	r := e.deleting("raced")
	e.deletionJob(r, domain.JobSucceeded, jobs.JobError{})
	repo := &conflictRepo{Repository: e.store}
	svc, err := restic.New(restic.Deps{
		Repo: repo, Remotes: storeRemotes{e.store}, Prober: e.prober, Backends: mustRegistry(t),
		Cipher: e.cipher, Clock: e.clock, IDs: e.ids,
	}, restic.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := svc.FinishDeletions(context.Background()); err != nil || n != 0 || repo.finishes != 1 {
		t.Errorf("FinishDeletions = %d, %v after %d attempts; want the conflict skipped", n, err, repo.finishes)
	}
}

// signalRepo reports every FinishDeletion on a channel.
type signalRepo struct {
	restic.Repository
	finished chan domain.ResticRepoID
}

func (r signalRepo) FinishDeletion(ctx context.Context, id domain.ResticRepoID, job domain.JobID, o restic.DeletionOutcome, at time.Time) error {
	err := r.Repository.FinishDeletion(ctx, id, job, o, at)
	r.finished <- id
	return err
}

func TestRunFinishesOnStartAndOnWake(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	repo := signalRepo{Repository: e.store, finished: make(chan domain.ResticRepoID, 4)}
	// The interval never elapses in the test: only the first pass and Wake
	// drive it.
	svc, err := restic.New(restic.Deps{
		Repo: repo, Remotes: storeRemotes{e.store}, Prober: e.prober, Backends: mustRegistry(t), Deletions: e.deletions,
		Cipher: e.cipher, Clock: e.clock, IDs: e.ids,
	}, restic.Config{FinishInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	first := e.deleting("first")
	e.deletionJob(first, domain.JobSucceeded, jobs.JobError{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx) }()
	if got := <-repo.finished; got != first.ID || !e.get(first.ID).Deleted() {
		t.Errorf("the first pass finished %s, want %s", got, first.ID)
	}

	second := e.deleting("second")
	e.deletionJob(second, domain.JobFailed, jobs.JobError{Code: "wrong_password"})
	svc.Wake()
	svc.Wake() // never blocks
	if got := <-repo.finished; got != second.ID {
		t.Errorf("the woken pass finished %s, want %s", got, second.ID)
	}
	if r := e.get(second.ID); r.Deleting || r.DeletionError == "" {
		t.Errorf("after the woken pass: %+v", r)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run = %v", err)
	}
}

func TestSnapshots(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	r := e.create()
	older, newer := snap(1, t0.Add(-2*time.Hour)), snap(2, t0.Add(-time.Hour))
	e.prober.snapshots = restic.SnapshotsResult{
		// Unsorted, with a duplicate and an ID no store would take.
		Snapshots: []restic.Snapshot{older, newer, older, {ID: "short", Time: t0}},
		WorkerID:  e.worker, WorkerName: "nas-01",
	}

	// Never listed: the first call refreshes by itself.
	list, err := e.svc.Snapshots(ctx, e.alice, r.ID, restic.SnapshotsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Snapshots) != 2 || list.Snapshots[0].ID != newer.ID || list.Snapshots[1].ID != older.ID ||
		!list.RefreshedAt.Equal(t0) || list.Stale || list.WorkerID != e.worker || list.WorkerName != "nas-01" || list.RefreshError != "" {
		t.Errorf("first Snapshots = %+v", list)
	}
	if call := e.prober.snapCalls[0]; call.Password != sentinel || call.RepositoryID != r.ID || call.RemoteID != e.s3 || call.Path != "restic/photos" {
		t.Errorf("the probe target is wrong (remote %s, path %q)", call.RemoteID, call.Path)
	}

	// Then the cache is served.
	e.clock.Advance(time.Hour)
	list, err = e.svc.Snapshots(ctx, e.alice, r.ID, restic.SnapshotsOptions{})
	if err != nil || len(e.prober.snapCalls) != 1 || len(list.Snapshots) != 2 || !list.WorkerID.IsZero() || list.Stale {
		t.Errorf("cached Snapshots = %+v, %v (%d probes)", list, err, len(e.prober.snapCalls))
	}

	// A succeeded backup makes it stale; other kinds and failures do not.
	job := func(kind string, st domain.JobStatus, finished time.Time) {
		j := jobs.Job{ID: e.ids.NewJobID(), WorkerID: e.worker, OwnerID: e.alice.UserID, Kind: kind, Status: st, Definition: []byte(`{}`),
			RepositoryID: r.ID, CreatedAt: t0, QueuedAt: t0, FinishedAt: finished, ExitCode: -1, Version: 1}
		if err := e.store.CreateJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	job("restic_check", domain.JobSucceeded, e.clock.Now())
	job("restic_backup", domain.JobFailed, e.clock.Now())
	if list, _ := e.svc.Snapshots(ctx, e.alice, r.ID, restic.SnapshotsOptions{}); list.Stale {
		t.Error("a check or a failed backup made the cache stale")
	}
	job("restic_backup", domain.JobSucceeded, e.clock.Now())
	list, _ = e.svc.Snapshots(ctx, e.alice, r.ID, restic.SnapshotsOptions{})
	if !list.Stale || !list.LastChangeAt.Equal(e.clock.Now()) {
		t.Errorf("after a backup: stale=%v last change %s", list.Stale, list.LastChangeAt)
	}
	if got, _ := e.svc.Get(ctx, e.alice, r.ID); !got.Stats.SnapshotsStale() || got.Stats.SnapshotCount != 2 || got.Stats.LastBackup.ID.IsZero() {
		t.Errorf("stats after a backup: %+v", got.Stats)
	}

	// An explicit refresh through a chosen worker.
	e.clock.Advance(time.Minute)
	e.prober.snapshots.Snapshots = []restic.Snapshot{snap(3, e.clock.Now())}
	e.prober.snapshots.Truncated = true
	list, err = e.svc.Snapshots(ctx, e.alice, r.ID, restic.SnapshotsOptions{Refresh: true, WorkerID: e.worker})
	if err != nil || len(list.Snapshots) != 1 || !list.Truncated || list.Stale || e.prober.snapCalls[1].WorkerID != e.worker {
		t.Errorf("refreshed Snapshots = %+v, %v", list, err)
	}

	// A failed refresh serves the cache with the reason.
	e.prober.snapshotsErr = fmt.Errorf("%w: worker %q is offline (%s)", domain.ErrUnavailable, "nas-01", sentinel)
	list, err = e.svc.Snapshots(ctx, e.alice, r.ID, restic.SnapshotsOptions{Refresh: true})
	if err != nil || len(list.Snapshots) != 1 || !strings.Contains(list.RefreshError, "is offline") || strings.Contains(list.RefreshError, sentinel) {
		t.Errorf("failed refresh = %+v, %v", list, err)
	}
	// An internal error is not shown.
	e.prober.snapshotsErr = errors.New("pq: connection reset")
	list, err = e.svc.Snapshots(ctx, e.alice, r.ID, restic.SnapshotsOptions{Refresh: true})
	if err != nil || list.RefreshError != "the snapshots could not be refreshed" {
		t.Errorf("internal failure = %+v, %v", list, err)
	}
}

func TestSnapshotsFirstRefreshFails(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	r := e.create()
	e.prober.snapshotsErr = fmt.Errorf("%w: no usable worker", domain.ErrFailedPrecondition)
	if _, err := e.svc.Snapshots(ctx, e.alice, r.ID, restic.SnapshotsOptions{}); !errors.Is(err, domain.ErrFailedPrecondition) {
		t.Errorf("first refresh failing = %v, want the error (nothing is cached)", err)
	}
	// A password that cannot be decrypted is a failed precondition.
	e.prober.snapshotsErr = nil
	e.cipher.failDecrypt = true
	if _, err := e.svc.Snapshots(ctx, e.alice, r.ID, restic.SnapshotsOptions{}); !errors.Is(err, domain.ErrFailedPrecondition) {
		t.Errorf("unreadable password = %v", err)
	}
	if _, err := e.svc.Resolve(ctx, e.alice.UserID, r.ID); !errors.Is(err, domain.ErrFailedPrecondition) || errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Resolve with an unreadable password = %v", err)
	}
}

// A backup that finishes while the worker lists the snapshots must leave
// the cache stale: the list may not contain its snapshot.
func TestSnapshotsRefreshTimeIsWhenTheWorkerWasAsked(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	r := e.create()
	e.prober.onSnapshots = func() {
		e.clock.Advance(time.Minute)
		j := jobs.Job{ID: e.ids.NewJobID(), WorkerID: e.worker, OwnerID: e.alice.UserID, Kind: "restic_backup", Status: domain.JobSucceeded,
			Definition: []byte(`{}`), RepositoryID: r.ID, CreatedAt: t0, QueuedAt: t0, FinishedAt: t0.Add(30 * time.Second), ExitCode: -1, Version: 1}
		if err := e.store.CreateJob(ctx, j); err != nil {
			t.Error(err)
		}
	}
	list, err := e.svc.Snapshots(ctx, e.alice, r.ID, restic.SnapshotsOptions{Refresh: true})
	if err != nil || !list.RefreshedAt.Equal(t0) || !list.Stale {
		t.Errorf("Snapshots = refreshed %s stale %v, %v; want refreshed at the request and stale", list.RefreshedAt, list.Stale, err)
	}
}

func TestBrowse(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	r := e.create()
	e.prober.listing = restic.Listing{WorkerID: e.worker, WorkerName: "nas-01", Path: "/srv", Entries: []restic.Entry{{Name: "photos", Path: "/srv/photos", Type: restic.EntryDir}}}

	l, err := e.svc.Browse(ctx, e.alice, r.ID, "1a2b3c4d", "/srv", e.worker)
	if err != nil || l.Path != "/srv" || len(l.Entries) != 1 || l.WorkerName != "nas-01" {
		t.Fatalf("Browse = %+v, %v", l, err)
	}
	call := e.prober.listCalls[0]
	if call.snapshot != "1a2b3c4d" || call.path != "/srv" || call.target.Password != sentinel || call.target.WorkerID != e.worker || call.target.RepositoryID != r.ID {
		t.Errorf("List call = snapshot %q path %q", call.snapshot, call.path)
	}
	// An empty path is the root.
	e.prober.listing.Path = ""
	if l, err := e.svc.Browse(ctx, e.alice, r.ID, strings.Repeat("a", 64), "", domain.WorkerID{}); err != nil || l.Path != "/" || e.prober.listCalls[1].path != "/" {
		t.Errorf("Browse(root) = %+v, %v", l, err)
	}

	bad := []struct{ snapshot, path, field string }{
		{"latest", "/", "snapshot_id"},
		{"1a2b3c4", "/", "snapshot_id"},
		{"1A2B3C4D", "/", "snapshot_id"},
		{"1a2b3c4d", "srv", "path"},
		{"1a2b3c4d", "/srv/../etc", "path"},
		{"1a2b3c4d", "/srv\n", "path"},
	}
	calls := len(e.prober.listCalls)
	for _, tt := range bad {
		_, err := e.svc.Browse(ctx, e.alice, r.ID, tt.snapshot, tt.path, domain.WorkerID{})
		if got := fieldsOf(err); !slices.Equal(got, []string{tt.field}) {
			t.Errorf("Browse(%q, %q) = %v, want a problem on %s", tt.snapshot, tt.path, err, tt.field)
		}
	}
	if len(e.prober.listCalls) != calls {
		t.Error("invalid input reached the worker")
	}

	// The worker's failure passes, scrubbed.
	e.prober.listErr = fmt.Errorf("%w: restic failed with %s", domain.ErrFailedPrecondition, sentinel)
	_, err = e.svc.Browse(ctx, e.alice, r.ID, "1a2b3c4d", "/", domain.WorkerID{})
	if !errors.Is(err, domain.ErrFailedPrecondition) {
		t.Errorf("Browse failure = %v", err)
	}
	noSecret(t, err)
}
