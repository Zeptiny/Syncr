package jobs_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/adapters/idgen"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// TestOwnerScoping: a user sees and cancels only their own jobs; another
// user's job is not found for every use case; an admin may view and cancel
// any job.
func TestOwnerScoping(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	alice, bob, admin := e.owner, e.addUser(t, domain.RoleUser), e.addUser(t, domain.RoleAdmin)
	j := e.at(t, domain.JobRunning)
	if _, err := e.svc.RecordProgress(ctx, e.worker, j.ID, epoch, jobs.Sample{Time: e.clock.Now(), Stats: jobs.Stats{Bytes: 1}}); err != nil {
		t.Fatal(err)
	}
	nobody := func(jobs.Update) error { return nil }

	for name, actor := range map[string]domain.Actor{
		"another user":      bob,
		"nobody":            {},
		"a user without id": {Role: domain.RoleUser},
		"spoofed role":      {UserID: bob.UserID, Role: "superuser"},
	} {
		if _, err := e.svc.Get(ctx, actor, j.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("%s: Get = %v", name, err)
		}
		if _, err := e.svc.Samples(ctx, actor, j.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("%s: Samples = %v", name, err)
		}
		if err := e.svc.Watch(ctx, actor, j.ID, nobody); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("%s: Watch = %v", name, err)
		}
		if _, err := e.svc.Cancel(ctx, actor, j.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("%s: Cancel = %v", name, err)
		}
	}
	if got, _ := e.svc.Get(ctx, alice, j.ID); got.CancelRequested() || got.Version != j.Version {
		t.Errorf("a foreign cancel changed the job: %+v", got)
	}

	for name, actor := range map[string]domain.Actor{"owner": alice, "admin": admin} {
		if got, err := e.svc.Get(ctx, actor, j.ID); err != nil || got.ID != j.ID {
			t.Errorf("%s: Get = (%v, %v)", name, got.ID, err)
		}
		if smp, err := e.svc.Samples(ctx, actor, j.ID); err != nil || len(smp) != 1 {
			t.Errorf("%s: Samples = (%v, %v)", name, smp, err)
		}
		stop := errors.New("stop")
		if err := e.svc.Watch(ctx, actor, j.ID, func(jobs.Update) error { return stop }); !errors.Is(err, stop) {
			t.Errorf("%s: Watch = %v", name, err)
		}
	}
	// The admin cancels alice's job.
	got, err := e.svc.Cancel(ctx, admin, j.ID)
	if err != nil || !got.CancelRequested() || got.OwnerID != alice.UserID {
		t.Errorf("admin Cancel = (%+v, %v)", got, err)
	}
}

func TestListScoping(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	alice, bob, admin := e.owner, e.addUser(t, domain.RoleUser), e.addUser(t, domain.RoleAdmin)
	a1, b1, x1, a2 := e.createAs(t, alice, jobs.CreateParams{}), e.createAs(t, bob, jobs.CreateParams{}), e.createAs(t, admin, jobs.CreateParams{}), e.createAs(t, alice, jobs.CreateParams{})
	ids := func(actor domain.Actor, f jobs.ListFilter) []domain.JobID {
		t.Helper()
		p, err := e.svc.List(ctx, actor, f)
		if err != nil {
			t.Fatal(err)
		}
		var out []domain.JobID
		for _, j := range p.Jobs {
			out = append(out, j.ID)
		}
		return out
	}
	tests := []struct {
		name  string
		actor domain.Actor
		f     jobs.ListFilter
		want  []domain.JobID
	}{
		{"own jobs", alice, jobs.ListFilter{}, []domain.JobID{a2.ID, a1.ID}},
		{"a user cannot list all", bob, jobs.ListFilter{AllUsers: true}, []domain.JobID{b1.ID}},
		{"a user cannot pick an owner", bob, jobs.ListFilter{OwnerID: alice.UserID}, []domain.JobID{b1.ID}},
		{"an admin's own jobs", admin, jobs.ListFilter{}, []domain.JobID{x1.ID}},
		{"an admin's owner filter needs all users", admin, jobs.ListFilter{OwnerID: alice.UserID}, []domain.JobID{x1.ID}},
		{"all users", admin, jobs.ListFilter{AllUsers: true}, []domain.JobID{a2.ID, x1.ID, b1.ID, a1.ID}},
		{"all users, one owner", admin, jobs.ListFilter{AllUsers: true, OwnerID: bob.UserID}, []domain.JobID{b1.ID}},
		{"nobody", domain.Actor{}, jobs.ListFilter{AllUsers: true}, nil},
		{"an admin without id", domain.Actor{Role: domain.RoleAdmin}, jobs.ListFilter{}, nil},
	}
	for _, tt := range tests {
		if got := ids(tt.actor, tt.f); !slices.Equal(got, tt.want) {
			t.Errorf("%s: %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestCreateChecksRemotes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	alice, bob := e.owner, e.addUser(t, domain.RoleUser)
	photos, archive, bobs := e.addRemote(t, alice, "Photos"), e.addRemote(t, alice, "Archive B2"), e.addRemote(t, bob, "Bob's")
	gone := e.addRemote(t, alice, "Gone")
	if err := e.store.DeleteRemote(ctx, gone, e.clock.Now()); err != nil {
		t.Fatal(err)
	}
	p := func(ids ...domain.RemoteID) jobs.CreateParams {
		return jobs.CreateParams{WorkerID: e.worker, Kind: "rclone_transfer", Definition: []byte(testDefinition), RemoteIDs: ids, SearchTerms: []string{"Sync", "/Data/Src"}}
	}

	j, err := e.svc.Create(ctx, alice, p(photos, archive, photos))
	if err != nil {
		t.Fatal(err)
	}
	// RemoteIDs: sorted by ID, once each; the search text holds the kind,
	// the terms and the remote names, lower-cased.
	want := []domain.RemoteID{photos, archive}
	if !slices.Equal(j.RemoteIDs, want) || j.SearchText != "rclone_transfer\nsync\n/data/src\nphotos\narchive b2" {
		t.Errorf("job remotes %v, search text %q", j.RemoteIDs, j.SearchText)
	}
	if stored, _ := e.svc.Get(ctx, alice, j.ID); !slices.Equal(stored.RemoteIDs, want) || stored.SearchText != j.SearchText {
		t.Errorf("stored job remotes %v, search text %q", stored.RemoteIDs, stored.SearchText)
	}

	before := len(e.rec.updates)
	for name, ids := range map[string][]domain.RemoteID{
		"another user's remote": {photos, bobs},
		"a deleted remote":      {gone},
		"a missing remote":      {domain.NewRemoteID()},
	} {
		if _, err := e.svc.Create(ctx, alice, p(ids...)); !errors.Is(err, jobs.ErrUnknownRemote) || !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrUnknownRemote", name, err)
		}
	}
	if len(e.rec.updates) != before {
		t.Error("a rejected job was published")
	}

	// Errors of the checker other than not found are passed on as is.
	e.remotes.err = errors.New("database down")
	if _, err := e.svc.Create(ctx, alice, p(photos)); err == nil || errors.Is(err, domain.ErrNotFound) {
		t.Errorf("checker failure: err = %v", err)
	}
}

func TestCreateWithoutRemoteChecker(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	svc, err := jobs.New(jobs.Deps{
		Jobs: e.store, Samples: e.store, Clock: e.clock, IDs: idgen.UUIDv7{}, Broker: e.rec, Notifier: e.rec, Waker: e.rec,
	}, jobs.Config{})
	if err != nil {
		t.Fatal(err)
	}
	p := jobs.CreateParams{WorkerID: e.worker, Kind: "rclone_transfer", Definition: []byte(testDefinition)}
	if _, err := svc.Create(ctx, e.owner, p); err != nil {
		t.Errorf("a job without remotes: %v", err)
	}
	p.RemoteIDs = []domain.RemoteID{e.addRemote(t, e.owner, "r")}
	if _, err := svc.Create(ctx, e.owner, p); !errors.Is(err, jobs.ErrUnknownRemote) {
		t.Errorf("a job with remotes: %v, want ErrUnknownRemote (fail closed)", err)
	}
}

func TestListFilters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	alice := e.owner
	photos, archive := e.addRemote(t, alice, "Photos 100%"), e.addRemote(t, alice, "Archive_B2")
	other := e.addWorker(t)
	t0 := e.clock.Now()
	mk := func(p jobs.CreateParams) jobs.Job {
		e.clock.Advance(time.Minute)
		return e.createAs(t, alice, p)
	}
	j1 := mk(jobs.CreateParams{RemoteIDs: []domain.RemoteID{photos}, SearchTerms: []string{"sync", "/srv/photos"}})
	j2 := mk(jobs.CreateParams{RemoteIDs: []domain.RemoteID{photos, archive}, SearchTerms: []string{"copy", "albums"}})
	j3 := mk(jobs.CreateParams{WorkerID: other, Kind: "restic_backup", SearchTerms: []string{"/home"}})
	ids := func(f jobs.ListFilter) []domain.JobID {
		t.Helper()
		p, err := e.svc.List(ctx, alice, f)
		if err != nil {
			t.Fatal(err)
		}
		var out []domain.JobID
		for _, j := range p.Jobs {
			out = append(out, j.ID)
		}
		return out
	}
	tests := []struct {
		name string
		f    jobs.ListFilter
		want []domain.JobID
	}{
		{"all", jobs.ListFilter{}, []domain.JobID{j3.ID, j2.ID, j1.ID}},
		{"remote", jobs.ListFilter{RemoteID: archive}, []domain.JobID{j2.ID}},
		{"remote used twice", jobs.ListFilter{RemoteID: photos}, []domain.JobID{j2.ID, j1.ID}},
		{"worker", jobs.ListFilter{WorkerID: other}, []domain.JobID{j3.ID}},
		{"kinds", jobs.ListFilter{Kinds: []string{"restic_backup", "restic_check"}}, []domain.JobID{j3.ID}},
		{"query on a path, any case", jobs.ListFilter{Query: "  SRV/Photos "}, []domain.JobID{j1.ID}},
		{"query on a remote name", jobs.ListFilter{Query: "archive_b2"}, []domain.JobID{j2.ID}},
		{"query on the kind", jobs.ListFilter{Query: "restic"}, []domain.JobID{j3.ID}},
		{"percent is literal", jobs.ListFilter{Query: "100%"}, []domain.JobID{j2.ID, j1.ID}},
		{"percent matches nothing else", jobs.ListFilter{Query: "%photos"}, nil},
		{"underscore is literal", jobs.ListFilter{Query: "rclone_"}, []domain.JobID{j2.ID, j1.ID}},
		{"a space is no underscore", jobs.ListFilter{Query: "archive b2"}, nil},
		{"created after (inclusive)", jobs.ListFilter{CreatedAfter: j2.CreatedAt}, []domain.JobID{j3.ID, j2.ID}},
		{"created before (exclusive)", jobs.ListFilter{CreatedBefore: j2.CreatedAt}, []domain.JobID{j1.ID}},
		{"created range", jobs.ListFilter{CreatedAfter: t0, CreatedBefore: j3.CreatedAt}, []domain.JobID{j2.ID, j1.ID}},
		{"everything", jobs.ListFilter{RemoteID: photos, Kinds: []string{"rclone_transfer"}, Query: "albums", CreatedAfter: t0}, []domain.JobID{j2.ID}},
	}
	for _, tt := range tests {
		if got := ids(tt.f); !slices.Equal(got, tt.want) {
			t.Errorf("%s: %v, want %v", tt.name, got, tt.want)
		}
	}
}
