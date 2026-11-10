package storetest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// This file is the contract suite of restic.Repository, and of what the
// other ports do with repositories: a remote that holds one cannot be
// deleted, and jobs and schedules refer to them.

func repoID(t *testing.T, n int) domain.ResticRepoID {
	return id(t, domain.ParseResticRepoID, 9000+n)
}

// resticHex returns the 64 hex digits of test restic (or snapshot) ID n.
func resticHex(n int) string { return fmt.Sprintf("%064x", n) }

// newRepo returns repository n of owner on remote at path, with restic ID
// n.
func newRepo(t *testing.T, n int, owner domain.UserID, name string, remote domain.RemoteID, path string) restic.Repo {
	return restic.Repo{
		ID: repoID(t, n), OwnerID: owner, Name: name, Description: "backups", RemoteID: remote, Path: path,
		PasswordEnc: []byte("encrypted password"), ResticID: resticHex(n), FormatVersion: 2,
		CreatedAt: t0(), UpdatedAt: t0(), Version: 1,
	}
}

func mustCreateRepo(t *testing.T, s Store, r restic.Repo) {
	t.Helper()
	if err := s.CreateRepo(context.Background(), r); err != nil {
		t.Fatalf("CreateRepo(%s): %v", r.Name, err)
	}
}

func mustGetRepo(t *testing.T, s Store, id domain.ResticRepoID) restic.Repo {
	t.Helper()
	r, err := s.GetRepo(context.Background(), id)
	if err != nil {
		t.Fatalf("GetRepo: %v", err)
	}
	return r
}

// requireRepos creates the users of the test and a remote of the first,
// and skips the test while the restic port is a stub.
func requireRepos(t *testing.T, s Store, owners ...domain.UserID) domain.RemoteID {
	t.Helper()
	requireRemotes(t, s, owners...)
	_, err := s.ListRepos(context.Background(), restic.ListFilter{})
	skipIfUnsupported(t, "ListRepos", "the restic repositories", err)
	r := newRemote(t, 90, owners[0], "Repo remote")
	mustCreateRemote(t, s, r)
	return r.ID
}

func repoIDs(rs []restic.Repo) []domain.ResticRepoID {
	out := make([]domain.ResticRepoID, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}

func testSnapshot(n int, at time.Time) restic.Snapshot {
	return restic.Snapshot{
		ID: resticHex(n), Time: at, Hostname: "nas-01", Username: "root", Parent: resticHex(n + 100), ProgramVersion: "restic 0.19.1",
		Paths: []string{"/srv/photos", `C:\Users\ada`}, Tags: []string{"nightly"},
		Summary: &restic.SnapshotSummary{
			BackupStart: at.Add(-time.Minute), BackupEnd: at,
			FilesNew: 1, FilesChanged: 2, FilesUnmodified: 3, DirsNew: 4, DirsChanged: 5, DirsUnmodified: 6,
			DataBlobs: 7, TreeBlobs: 8, DataAdded: 9, DataAddedPacked: 10, TotalFilesProcessed: 11, TotalBytesProcessed: int64(n) * 1000,
		},
	}
}

func snapshotIDs(ss []restic.Snapshot) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.ID
	}
	return out
}

// repoJob returns job n of worker w on repository repo.
func repoJob(t *testing.T, n int, w domain.WorkerID, owner domain.UserID, repo domain.ResticRepoID, kind string) jobs.Job {
	j := newJob(t, n, w)
	j.OwnerID, j.Kind, j.RepositoryID = owner, kind, repo
	return j
}

func testRepoCreateGetList(t *testing.T, s Store) {
	alice, bob := userID(t, 1), userID(t, 2)
	remote := requireRepos(t, s, alice, bob)
	ctx := context.Background()
	other := newRemote(t, 91, alice, "Other remote")
	bobs := newRemote(t, 92, bob, "Bob's remote")
	mustCreateRemote(t, s, other)
	mustCreateRemote(t, s, bobs)

	b := newRepo(t, 1, alice, "beta", remote, "restic/beta")
	a := newRepo(t, 2, alice, "Alpha", remote, "")
	c := newRepo(t, 3, alice, "gamma", other.ID, "restic/beta")
	d := newRepo(t, 4, bob, "alpha", bobs.ID, "x")
	d.Description, d.FormatVersion = "", 0
	for _, r := range []restic.Repo{b, a, c, d} {
		mustCreateRepo(t, s, r)
	}

	got := mustGetRepo(t, s, b.ID)
	if !reflect.DeepEqual(got, b) {
		t.Errorf("GetRepo = %+v, want %+v", got, b)
	}
	if got := mustGetRepo(t, s, d.ID); !reflect.DeepEqual(got, d) {
		t.Errorf("GetRepo(defaults) = %+v, want %+v", got, d)
	}
	_, err := s.GetRepo(ctx, repoID(t, 99))
	wantErr(t, "GetRepo(missing)", err, domain.ErrNotFound)

	for _, tt := range []struct {
		name string
		f    restic.ListFilter
		want []domain.ResticRepoID
	}{
		// By lower-cased name, then ID.
		{"all", restic.ListFilter{}, []domain.ResticRepoID{a.ID, d.ID, b.ID, c.ID}},
		{"alice", restic.ListFilter{OwnerID: alice}, []domain.ResticRepoID{a.ID, b.ID, c.ID}},
		{"bob", restic.ListFilter{OwnerID: bob}, []domain.ResticRepoID{d.ID}},
		{"a remote", restic.ListFilter{RemoteID: remote}, []domain.ResticRepoID{a.ID, b.ID}},
		{"alice on another remote", restic.ListFilter{OwnerID: alice, RemoteID: other.ID}, []domain.ResticRepoID{c.ID}},
		{"bob on alice's remote", restic.ListFilter{OwnerID: bob, RemoteID: remote}, []domain.ResticRepoID{}},
	} {
		list, err := s.ListRepos(ctx, tt.f)
		if err != nil || !slices.Equal(repoIDs(list), tt.want) {
			t.Errorf("ListRepos(%s) = %v, %v; want %v", tt.name, repoIDs(list), err, tt.want)
		}
	}

	some, err := s.GetRepos(ctx, []domain.ResticRepoID{c.ID, a.ID, repoID(t, 99), a.ID})
	if err != nil || !slices.Equal(repoIDs(some), []domain.ResticRepoID{b.ID, a.ID, c.ID}[1:]) {
		t.Errorf("GetRepos = %v, %v; want the existing ones once, by ID", repoIDs(some), err)
	}
	if none, err := s.GetRepos(ctx, nil); err != nil || len(none) != 0 {
		t.Errorf("GetRepos(nil) = %v, %v", none, err)
	}

	// Values the schema rejects.
	for what, mutate := range map[string]func(*restic.Repo){
		"no name":              func(r *restic.Repo) { r.Name = "" },
		"long name":            func(r *restic.Repo) { r.Name = strings.Repeat("n", restic.MaxNameLen+1) },
		"long description":     func(r *restic.Repo) { r.Description = strings.Repeat("d", restic.MaxDescriptionLen+1) },
		"short restic id":      func(r *restic.Repo) { r.ResticID = "abc" },
		"upper-case restic id": func(r *restic.Repo) { r.ResticID = strings.Repeat("A", 64) },
		"no password":          func(r *restic.Repo) { r.PasswordEnc = nil },
	} {
		bad := newRepo(t, 50, alice, "bad", remote, "bad")
		mutate(&bad)
		wantErr(t, "CreateRepo("+what+")", s.CreateRepo(ctx, bad), domain.ErrInvalidArgument)
	}
	// The deletion fields, the deleted flag and the statistics of a new
	// repository are not written.
	odd := newRepo(t, 51, alice, "odd", remote, "odd")
	odd.Deleting, odd.DeletionJobID, odd.DeletionRequestedAt, odd.DeletionError = true, jobID(t, 1), t0(), "boom"
	odd.Stats.SnapshotCount = 7
	mustCreateRepo(t, s, odd)
	if got := mustGetRepo(t, s, odd.ID); got.Deleting || !got.DeletionJobID.IsZero() || got.DeletionError != "" || got.Stats.SnapshotCount != 0 {
		t.Errorf("CreateRepo wrote deletion fields or statistics: %+v", got)
	}
}

func testRepoUniqueAmongLive(t *testing.T, s Store) {
	alice, bob := userID(t, 1), userID(t, 2)
	remote := requireRepos(t, s, alice, bob)
	ctx := context.Background()
	other := newRemote(t, 91, alice, "Other remote")
	bobs := newRemote(t, 92, bob, "Bob's remote")
	mustCreateRemote(t, s, other)
	mustCreateRemote(t, s, bobs)
	first := newRepo(t, 1, alice, "Photos", remote, "restic/photos")
	mustCreateRepo(t, s, first)

	sameID := newRepo(t, 1, alice, "Other", other.ID, "elsewhere")
	sameID.ResticID = resticHex(70)
	wantErr(t, "same ID", s.CreateRepo(ctx, sameID), domain.ErrAlreadyExists)
	sameName := newRepo(t, 2, alice, "PHOTOS", other.ID, "elsewhere")
	wantErr(t, "same name in another case", s.CreateRepo(ctx, sameName), domain.ErrAlreadyExists)
	sameLocation := newRepo(t, 3, alice, "Other", remote, "restic/photos")
	wantErr(t, "same location", s.CreateRepo(ctx, sameLocation), domain.ErrAlreadyExists)
	sameRestic := newRepo(t, 4, alice, "Other", other.ID, "elsewhere")
	sameRestic.ResticID = first.ResticID
	wantErr(t, "same restic ID", s.CreateRepo(ctx, sameRestic), domain.ErrAlreadyExists)

	// Another owner may use the name and hold the same restic repository.
	bobsRepo := newRepo(t, 5, bob, "Photos", bobs.ID, "restic/photos")
	bobsRepo.ResticID = first.ResticID
	mustCreateRepo(t, s, bobsRepo)
	// The same path on another remote is another location.
	mustCreateRepo(t, s, newRepo(t, 6, alice, "Mirror", other.ID, "restic/photos"))

	// All three are free again once the repository is deleted.
	if err := s.DeleteRepo(ctx, first.ID, t0().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	again := newRepo(t, 7, alice, "photos", remote, "restic/photos")
	again.ResticID = first.ResticID
	mustCreateRepo(t, s, again)

	// A rename cannot take a live name, but may change its own case.
	mirror := mustGetRepo(t, s, repoID(t, 6))
	mirror.Name = "PHOTOS"
	_, err := s.UpdateRepo(ctx, mirror)
	wantErr(t, "rename to a taken name", err, domain.ErrAlreadyExists)
	mirror.Name = "MIRROR"
	if _, err := s.UpdateRepo(ctx, mirror); err != nil {
		t.Errorf("recasing its own name: %v", err)
	}
}

func testRepoUpdateVersioned(t *testing.T, s Store) {
	alice := userID(t, 1)
	remote := requireRepos(t, s, alice)
	ctx := context.Background()
	r := newRepo(t, 1, alice, "Photos", remote, "restic/photos")
	mustCreateRepo(t, s, r)

	upd := r
	upd.Name, upd.Description, upd.PasswordEnc, upd.UpdatedAt = "Family", "new", []byte("another password"), t0().Add(time.Hour)
	// Fields UpdateRepo never writes.
	upd.OwnerID, upd.Path, upd.ResticID, upd.FormatVersion, upd.CreatedAt = userID(t, 9), "moved", resticHex(9), 1, t0().Add(time.Minute)
	upd.Deleting, upd.DeletionError, upd.DeletedAt = true, "boom", t0()
	got, err := s.UpdateRepo(ctx, upd)
	if err != nil {
		t.Fatal(err)
	}
	want := r
	want.Name, want.Description, want.PasswordEnc, want.UpdatedAt, want.Version = "Family", "new", []byte("another password"), t0().Add(time.Hour), 2
	if !reflect.DeepEqual(got, want) {
		t.Errorf("UpdateRepo = %+v, want %+v", got, want)
	}
	if stored := mustGetRepo(t, s, r.ID); !reflect.DeepEqual(stored, want) {
		t.Errorf("stored = %+v, want %+v", stored, want)
	}

	// A stale version.
	_, err = s.UpdateRepo(ctx, upd)
	wantErr(t, "UpdateRepo(stale)", err, domain.ErrConflict)
	missing := upd
	missing.ID = repoID(t, 99)
	_, err = s.UpdateRepo(ctx, missing)
	wantErr(t, "UpdateRepo(missing)", err, domain.ErrNotFound)
	// A live repository needs its password.
	bad := got
	bad.PasswordEnc = nil
	_, err = s.UpdateRepo(ctx, bad)
	wantErr(t, "UpdateRepo(no password)", err, domain.ErrInvalidArgument)
	bad = got
	bad.Name = ""
	_, err = s.UpdateRepo(ctx, bad)
	wantErr(t, "UpdateRepo(no name)", err, domain.ErrInvalidArgument)

	// Exactly one of two concurrent updates of the same version wins.
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() {
			u := got
			u.Description = fmt.Sprintf("writer %d", i)
			_, errs[i] = s.UpdateRepo(ctx, u)
		})
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case !errors.Is(err, domain.ErrConflict):
			t.Errorf("a racing UpdateRepo failed with %v, want ErrConflict", err)
		}
	}
	if won != 1 || mustGetRepo(t, s, r.ID).Version != 3 {
		t.Errorf("%d concurrent updates won, version %d; want 1 and 3", won, mustGetRepo(t, s, r.ID).Version)
	}
}

func testRepoSoftDelete(t *testing.T, s Store) {
	alice := userID(t, 1)
	remote := requireRepos(t, s, alice)
	ctx := context.Background()
	r := newRepo(t, 1, alice, "Photos", remote, "restic/photos")
	keep := newRepo(t, 2, alice, "Keep", remote, "keep")
	mustCreateRepo(t, s, r)
	mustCreateRepo(t, s, keep)
	for _, id := range []domain.ResticRepoID{r.ID, keep.ID} {
		if err := s.ReplaceSnapshots(ctx, id, []restic.Snapshot{testSnapshot(1, t0())}, true, t0()); err != nil {
			t.Fatal(err)
		}
	}
	at := t0().Add(time.Hour)
	if err := s.DeleteRepo(ctx, r.ID, at); err != nil {
		t.Fatal(err)
	}
	got := mustGetRepo(t, s, r.ID)
	if !got.Deleted() || !got.DeletedAt.Equal(at) || !got.UpdatedAt.Equal(at) || got.PasswordEnc != nil || got.Version != 2 ||
		got.Name != "Photos" || !got.DataDeletedAt.IsZero() || got.Deleting || got.Stats.SnapshotCount != 0 {
		t.Errorf("GetRepo(deleted) = %+v: want DeletedAt set, the password wiped, version 2, no snapshots", got)
	}
	if list, err := s.ListSnapshots(ctx, r.ID); err != nil || len(list.Snapshots) != 0 {
		t.Errorf("ListSnapshots(deleted) = %v, %v; want an empty cache", snapshotIDs(list.Snapshots), err)
	}
	if list, _ := s.ListSnapshots(ctx, keep.ID); len(list.Snapshots) != 1 {
		t.Error("deleting a repository dropped another one's snapshots")
	}
	if list, _ := s.ListRepos(ctx, restic.ListFilter{OwnerID: alice}); !slices.Equal(repoIDs(list), []domain.ResticRepoID{keep.ID}) {
		t.Errorf("ListRepos lists a deleted repository: %v", repoIDs(list))
	}
	if some, _ := s.GetRepos(ctx, []domain.ResticRepoID{r.ID}); len(some) != 1 || !some[0].Deleted() {
		t.Errorf("GetRepos(deleted) = %+v, want the deleted repository", some)
	}
	wantErr(t, "DeleteRepo(deleted)", s.DeleteRepo(ctx, r.ID, at), domain.ErrNotFound)
	wantErr(t, "DeleteRepo(missing)", s.DeleteRepo(ctx, repoID(t, 99), at), domain.ErrNotFound)
	upd := got
	upd.Name, upd.PasswordEnc = "revived", []byte("x")
	_, err := s.UpdateRepo(ctx, upd)
	wantErr(t, "UpdateRepo(deleted)", err, domain.ErrNotFound)
	wantErr(t, "ReplaceSnapshots(deleted)", s.ReplaceSnapshots(ctx, r.ID, nil, false, at), domain.ErrNotFound)
	_, err = s.MarkDeleting(ctx, r.ID, jobID(t, 1), at)
	wantErr(t, "MarkDeleting(deleted)", err, domain.ErrNotFound)
}

func testRepoFKParity(t *testing.T, s Store) {
	alice := userID(t, 1)
	remote := requireRepos(t, s, alice)
	ctx := context.Background()

	noOwner := newRepo(t, 1, userID(t, 77), "Photos", remote, "a")
	wantErr(t, "CreateRepo(unknown owner)", s.CreateRepo(ctx, noOwner), domain.ErrNotFound)
	noRemote := newRepo(t, 2, alice, "Photos", remoteID(t, 77), "a")
	wantErr(t, "CreateRepo(unknown remote)", s.CreateRepo(ctx, noRemote), domain.ErrNotFound)
	gone := newRemote(t, 91, alice, "Gone")
	mustCreateRemote(t, s, gone)
	if err := s.DeleteRemote(ctx, gone.ID, t0()); err != nil {
		t.Fatal(err)
	}
	onDeleted := newRepo(t, 3, alice, "Photos", gone.ID, "a")
	wantErr(t, "CreateRepo(deleted remote)", s.CreateRepo(ctx, onDeleted), domain.ErrNotFound)
	if list, err := s.ListRepos(ctx, restic.ListFilter{}); err != nil || len(list) != 0 {
		t.Errorf("a rejected CreateRepo left %v, %v", repoIDs(list), err)
	}
}

func testRepoSnapshotsReplace(t *testing.T, s Store) {
	alice := userID(t, 1)
	remote := requireRepos(t, s, alice)
	ctx := context.Background()
	r := newRepo(t, 1, alice, "Photos", remote, "restic/photos")
	other := newRepo(t, 2, alice, "Other", remote, "other")
	mustCreateRepo(t, s, r)
	mustCreateRepo(t, s, other)

	// Never listed.
	list, err := s.ListSnapshots(ctx, r.ID)
	if err != nil || len(list.Snapshots) != 0 || !list.RefreshedAt.IsZero() || list.Truncated || !list.LastChangeAt.IsZero() || list.Stale {
		t.Fatalf("ListSnapshots(new) = %+v, %v", list, err)
	}
	_, err = s.ListSnapshots(ctx, repoID(t, 99))
	wantErr(t, "ListSnapshots(missing)", err, domain.ErrNotFound)

	at := t0()
	old, mid, newest := testSnapshot(1, at.Add(-2*time.Hour)), testSnapshot(2, at.Add(-time.Hour)), testSnapshot(3, at)
	tie := testSnapshot(4, at) // the same instant as newest: the ID breaks the tie
	bare := restic.Snapshot{ID: resticHex(5), Time: at.Add(-3 * time.Hour)}
	if err := s.ReplaceSnapshots(ctx, r.ID, []restic.Snapshot{mid, bare, tie, old, newest}, true, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	list, err = s.ListSnapshots(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []restic.Snapshot{newest, tie, mid, old, bare}
	if !reflect.DeepEqual(list.Snapshots, want) {
		t.Errorf("ListSnapshots =\n%+v\nwant, newest first,\n%+v", list.Snapshots, want)
	}
	if !list.RefreshedAt.Equal(at.Add(time.Minute)) || !list.Truncated {
		t.Errorf("refreshed at %s, truncated %v", list.RefreshedAt, list.Truncated)
	}
	if o, _ := s.ListSnapshots(ctx, other.ID); len(o.Snapshots) != 0 || !o.RefreshedAt.IsZero() {
		t.Error("a replacement touched another repository's cache")
	}

	// A replacement replaces everything, also with nothing.
	if err := s.ReplaceSnapshots(ctx, r.ID, []restic.Snapshot{old}, false, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	list, _ = s.ListSnapshots(ctx, r.ID)
	if !slices.Equal(snapshotIDs(list.Snapshots), []string{old.ID}) || list.Truncated || !list.RefreshedAt.Equal(at.Add(time.Hour)) {
		t.Errorf("after the second replacement: %v, truncated %v", snapshotIDs(list.Snapshots), list.Truncated)
	}

	// A rejected replacement changes nothing.
	badID := testSnapshot(6, at)
	badID.ID = "not-a-snapshot-id"
	wantErr(t, "ReplaceSnapshots(bad ID)", s.ReplaceSnapshots(ctx, r.ID, []restic.Snapshot{mid, badID}, true, at.Add(2*time.Hour)), domain.ErrInvalidArgument)
	wantErr(t, "ReplaceSnapshots(duplicate)", s.ReplaceSnapshots(ctx, r.ID, []restic.Snapshot{mid, newest, mid}, true, at.Add(2*time.Hour)), domain.ErrAlreadyExists)
	wantErr(t, "ReplaceSnapshots(missing)", s.ReplaceSnapshots(ctx, repoID(t, 99), nil, false, at), domain.ErrNotFound)
	list, _ = s.ListSnapshots(ctx, r.ID)
	if !slices.Equal(snapshotIDs(list.Snapshots), []string{old.ID}) || list.Truncated || !list.RefreshedAt.Equal(at.Add(time.Hour)) {
		t.Errorf("a rejected replacement changed the cache: %v", snapshotIDs(list.Snapshots))
	}
	if err := s.ReplaceSnapshots(ctx, r.ID, nil, false, at.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListSnapshots(ctx, r.ID); len(list.Snapshots) != 0 || !list.RefreshedAt.Equal(at.Add(3*time.Hour)) {
		t.Errorf("replacing with nothing left %v", snapshotIDs(list.Snapshots))
	}

	// Concurrent replacements leave one of the sets, never a mix.
	sets := make([][]restic.Snapshot, 6)
	for i := range sets {
		for k := range 40 {
			sets[i] = append(sets[i], testSnapshot(1000*(i+1)+k, at.Add(time.Duration(k)*time.Second)))
		}
	}
	var wg sync.WaitGroup
	errs := make([]error, len(sets))
	for i := range sets {
		wg.Go(func() {
			errs[i] = s.ReplaceSnapshots(ctx, r.ID, sets[i], i%2 == 0, at.Add(time.Duration(i+10)*time.Hour))
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent replacement %d: %v", i, err)
		}
	}
	list, err = s.ListSnapshots(ctx, r.ID)
	if err != nil || len(list.Snapshots) != 40 {
		t.Fatalf("after concurrent replacements: %d snapshots, %v", len(list.Snapshots), err)
	}
	winner := -1
	for i := range sets {
		if list.Snapshots[0].ID == sets[i][39].ID {
			winner = i
		}
	}
	if winner < 0 {
		t.Fatalf("the cache holds none of the sets: %v", snapshotIDs(list.Snapshots)[:3])
	}
	for k, sn := range list.Snapshots {
		if sn.ID != sets[winner][39-k].ID {
			t.Fatalf("the cache mixes sets: snapshot %d is %s", k, sn.ID)
		}
	}
	if !list.RefreshedAt.Equal(at.Add(time.Duration(winner+10)*time.Hour)) || list.Truncated != (winner%2 == 0) {
		t.Errorf("the refresh time or truncation is not the winning set's (%d): %s, %v", winner, list.RefreshedAt, list.Truncated)
	}
}

func testRepoStatsAndLastChange(t *testing.T, s Store) {
	alice := userID(t, 1)
	remote := requireRepos(t, s, alice)
	requireSchedules(t, s, alice)
	ctx := context.Background()
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	r := newRepo(t, 1, alice, "Photos", remote, "restic/photos")
	other := newRepo(t, 2, alice, "Other", remote, "other")
	mustCreateRepo(t, s, r)
	mustCreateRepo(t, s, other)

	if got := mustGetRepo(t, s, r.ID); !reflect.DeepEqual(got.Stats, restic.RepoStats{}) {
		t.Errorf("stats of a new repository = %+v, want none", got.Stats)
	}

	at := t0()
	noSummary := restic.Snapshot{ID: resticHex(3), Time: at.Add(-time.Hour)}
	if err := s.ReplaceSnapshots(ctx, r.ID, []restic.Snapshot{testSnapshot(1, at.Add(-2*time.Hour)), testSnapshot(2, at), noSummary}, true, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceSnapshots(ctx, other.ID, []restic.Snapshot{testSnapshot(9, at.Add(time.Hour))}, false, at); err != nil {
		t.Fatal(err)
	}

	finished := func(j jobs.Job, st domain.JobStatus, at time.Time) jobs.Job {
		j.Status, j.FinishedAt = st, at
		return j
	}
	for _, j := range []jobs.Job{
		finished(repoJob(t, 1, w.ID, alice, r.ID, restic.KindBackup), domain.JobSucceeded, at.Add(10*time.Minute)),
		finished(repoJob(t, 2, w.ID, alice, r.ID, "restic_forget"), domain.JobSucceeded, at.Add(30*time.Minute)),
		finished(repoJob(t, 3, w.ID, alice, r.ID, "restic_prune"), domain.JobFailed, at.Add(50*time.Minute)),
		finished(repoJob(t, 4, w.ID, alice, r.ID, restic.KindCheck), domain.JobSucceeded, at.Add(55*time.Minute)),
		finished(repoJob(t, 5, w.ID, alice, r.ID, "restic_restore"), domain.JobSucceeded, at.Add(56*time.Minute)),
		finished(repoJob(t, 6, w.ID, alice, r.ID, restic.KindCheck), domain.JobFailed, at.Add(57*time.Minute)),
		repoJob(t, 7, w.ID, alice, r.ID, restic.KindBackup), // queued: the newest backup
		finished(repoJob(t, 8, w.ID, alice, other.ID, restic.KindBackup), domain.JobSucceeded, at.Add(5*time.Hour)),
		finished(newJob(t, 9, w.ID), domain.JobSucceeded, at.Add(6*time.Hour)),
	} {
		mustCreateJob(t, s, j)
	}
	for n := range 2 {
		sc := newSchedule(t, n+1, alice, fmt.Sprintf("Nightly %d", n), w.ID)
		sc.RepositoryID = r.ID
		sc.Enabled, sc.NextRunAt = n == 0, time.Time{}
		if sc.Enabled {
			sc.NextRunAt = at.Add(time.Hour)
		}
		mustCreateSchedule(t, s, sc)
	}
	mustCreateSchedule(t, s, newSchedule(t, 3, alice, "Unrelated", w.ID))

	want := restic.RepoStats{
		SnapshotCount: 3, LatestSnapshotAt: at, LatestSnapshotBytes: 2000,
		SnapshotsRefreshedAt: at.Add(time.Minute), SnapshotsTruncated: true,
		LastChangeAt:  at.Add(30 * time.Minute), // the forget: the failed prune and the other kinds do not count
		LastBackup:    restic.JobRef{ID: jobID(t, 7), Status: domain.JobQueued, CreatedAt: at},
		LastCheck:     restic.JobRef{ID: jobID(t, 6), Status: domain.JobFailed, CreatedAt: at, FinishedAt: at.Add(57 * time.Minute)},
		ScheduleCount: 2,
	}
	if got := mustGetRepo(t, s, r.ID); !reflect.DeepEqual(got.Stats, want) {
		t.Errorf("GetRepo stats =\n%+v\nwant\n%+v", got.Stats, want)
	}
	list, err := s.ListRepos(ctx, restic.ListFilter{OwnerID: alice})
	if err != nil || len(list) != 2 || list[1].ID != r.ID || !reflect.DeepEqual(list[1].Stats, want) {
		t.Errorf("ListRepos stats = %+v, %v", list, err)
	}
	if o := list[0].Stats; o.SnapshotCount != 1 || o.LatestSnapshotBytes != 9000 || o.LastBackup.ID != jobID(t, 8) || !o.LastChangeAt.Equal(at.Add(5*time.Hour)) || o.ScheduleCount != 0 || !o.LastCheck.ID.IsZero() {
		t.Errorf("the other repository's stats = %+v", o)
	}
	sl, err := s.ListSnapshots(ctx, r.ID)
	if err != nil || !sl.LastChangeAt.Equal(at.Add(30*time.Minute)) || sl.Stale {
		t.Errorf("ListSnapshots: last change %s, stale %v, %v", sl.LastChangeAt, sl.Stale, err)
	}
	// GetRepos needs no statistics, but must not invent any.
	if some, _ := s.GetRepos(ctx, []domain.ResticRepoID{r.ID}); len(some) != 1 || (some[0].Stats != restic.RepoStats{} && !reflect.DeepEqual(some[0].Stats, want)) {
		t.Errorf("GetRepos stats = %+v", some)
	}

	// The newest snapshot without a summary has an unknown size.
	if err := s.ReplaceSnapshots(ctx, r.ID, []restic.Snapshot{testSnapshot(1, at.Add(-2*time.Hour)), {ID: resticHex(4), Time: at.Add(time.Hour)}}, false, at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := mustGetRepo(t, s, r.ID).Stats; got.SnapshotCount != 2 || got.LatestSnapshotBytes != 0 || !got.LatestSnapshotAt.Equal(at.Add(time.Hour)) || got.SnapshotsTruncated {
		t.Errorf("stats with a summary-less newest snapshot = %+v", got)
	}
}

func testRemoteDeleteBlockedByRepo(t *testing.T, s Store) {
	alice := userID(t, 1)
	remote := requireRepos(t, s, alice)
	ctx := context.Background()
	b := newRepo(t, 1, alice, "beta", remote, "b")
	a := newRepo(t, 2, alice, "Alpha", remote, "a")
	mustCreateRepo(t, s, b)
	mustCreateRepo(t, s, a)

	err := s.DeleteRemote(ctx, remote, t0())
	wantErr(t, "DeleteRemote(holding repositories)", err, domain.ErrFailedPrecondition)
	if err == nil || !strings.Contains(err.Error(), `the remote holds repository "Alpha"; delete the repository first`) {
		t.Errorf("DeleteRemote = %v, want the first repository by name in the message", err)
	}
	if r, _ := s.GetRemote(ctx, remote); r.Deleted() {
		t.Fatal("the remote was deleted")
	}
	// A deleting repository still holds it.
	if err := s.DeleteRepo(ctx, a.ID, t0()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkDeleting(ctx, b.ID, jobID(t, 1), t0()); err != nil {
		t.Fatal(err)
	}
	wantErr(t, "DeleteRemote(holding a deleting repository)", s.DeleteRemote(ctx, remote, t0()), domain.ErrFailedPrecondition)
	// Once every repository is gone the remote can go.
	if err := s.FinishDeletion(ctx, b.ID, jobID(t, 1), restic.DeletionOutcome{Succeeded: true}, t0()); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRemote(ctx, remote, t0().Add(time.Hour)); err != nil {
		t.Errorf("DeleteRemote after its repositories were deleted: %v", err)
	}
}

func testRepoMarkDeletingCAS(t *testing.T, s Store) {
	alice := userID(t, 1)
	remote := requireRepos(t, s, alice)
	requireSchedules(t, s, alice)
	ctx := context.Background()
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	r := newRepo(t, 1, alice, "Photos", remote, "restic/photos")
	other := newRepo(t, 2, alice, "Other", remote, "other")
	mustCreateRepo(t, s, r)
	mustCreateRepo(t, s, other)
	if err := s.ReplaceSnapshots(ctx, r.ID, []restic.Snapshot{testSnapshot(1, t0())}, false, t0()); err != nil {
		t.Fatal(err)
	}

	sched := func(n int, repo domain.ResticRepoID, enabled bool) schedules.Schedule {
		sc := newSchedule(t, n, alice, fmt.Sprintf("Schedule %d", n), w.ID)
		sc.RepositoryID, sc.Enabled = repo, enabled
		if !enabled {
			sc.NextRunAt, sc.PausedReason = time.Time{}, "by hand"
		}
		mustCreateSchedule(t, s, sc)
		return sc
	}
	enabled, paused := sched(1, r.ID, true), sched(2, r.ID, false)
	elsewhere, plain := sched(3, other.ID, true), sched(4, domain.ResticRepoID{}, true)
	pending := mustAddRun(t, s, newRun(t, 1, enabled.ID, schedules.RunPending))
	launching := newRun(t, 2, enabled.ID, schedules.RunLaunching)
	launching.JobID, launching.WorkerID = jobID(t, 50), w.ID
	launching = mustAddRun(t, s, launching)
	done := mustAddRun(t, s, newRun(t, 3, enabled.ID, schedules.RunMissed))
	pausedRun := mustAddRun(t, s, newRun(t, 4, paused.ID, schedules.RunPending))
	otherRun := mustAddRun(t, s, newRun(t, 5, elsewhere.ID, schedules.RunPending))
	plainRun := mustAddRun(t, s, newRun(t, 6, plain.ID, schedules.RunPending))

	at := t0().Add(time.Hour)
	_, err := s.MarkDeleting(ctx, r.ID, domain.JobID{}, at)
	wantErr(t, "MarkDeleting(no job)", err, domain.ErrInvalidArgument)
	_, err = s.MarkDeleting(ctx, repoID(t, 99), jobID(t, 1), at)
	wantErr(t, "MarkDeleting(missing)", err, domain.ErrNotFound)

	// Exactly one of the concurrent marks wins.
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() { _, errs[i] = s.MarkDeleting(ctx, r.ID, jobID(t, 10+i), at) })
	}
	wg.Wait()
	winner := -1
	for i, err := range errs {
		switch {
		case err == nil && winner < 0:
			winner = i
		case err == nil:
			t.Errorf("marks %d and %d both won", winner, i)
		case !errors.Is(err, domain.ErrConflict):
			t.Errorf("a losing mark failed with %v, want ErrConflict", err)
		}
	}
	if winner < 0 {
		t.Fatal("no mark won")
	}
	got := mustGetRepo(t, s, r.ID)
	if !got.Deleting || got.DeletionJobID != jobID(t, 10+winner) || !got.DeletionRequestedAt.Equal(at) || got.DeletionError != "" ||
		!got.UpdatedAt.Equal(at) || got.Version != 2 || got.Deleted() || got.PasswordEnc == nil || got.Stats.SnapshotCount != 1 {
		t.Errorf("the marked repository = %+v", got)
	}

	// Its enabled schedules are paused and their open runs cancelled.
	if sc := mustGetSchedule(t, s, enabled.ID); sc.Enabled || !sc.NextRunAt.IsZero() || sc.PausedReason != schedules.ReasonRepositoryDeleting ||
		!sc.UpdatedAt.Equal(at) || sc.Version != enabled.Version+1 {
		t.Errorf("the enabled schedule = %+v, want it paused", sc)
	}
	for _, run := range []schedules.Run{pending, launching} {
		if r := mustGetRun(t, s, run.ID); r.Status != schedules.RunCancelled || r.Reason != schedules.ReasonRepositoryDeleting || !r.UpdatedAt.Equal(at) {
			t.Errorf("open run %s = %+v, want it cancelled", run.ID, r)
		}
	}
	if r := mustGetRun(t, s, done.ID); r.Status != schedules.RunMissed || r.Reason == schedules.ReasonRepositoryDeleting {
		t.Errorf("a finished run changed: %+v", r)
	}
	// A schedule paused before keeps its reason, but its open run goes too.
	if sc := mustGetSchedule(t, s, paused.ID); sc.Enabled || sc.PausedReason != "by hand" || sc.Version != paused.Version {
		t.Errorf("the already paused schedule = %+v, want it untouched", sc)
	}
	if r := mustGetRun(t, s, pausedRun.ID); r.Status != schedules.RunCancelled {
		t.Errorf("the open run of a paused schedule = %+v, want it cancelled", r)
	}
	// Other repositories' schedules, and schedules without one, are not
	// touched.
	for _, sc := range []schedules.Schedule{elsewhere, plain} {
		if got := mustGetSchedule(t, s, sc.ID); !got.Enabled || got.PausedReason != "" || got.Version != sc.Version || got.NextRunAt.IsZero() {
			t.Errorf("schedule %s = %+v, want it untouched", sc.Name, got)
		}
	}
	for _, run := range []schedules.Run{otherRun, plainRun} {
		if r := mustGetRun(t, s, run.ID); r.Status != schedules.RunPending {
			t.Errorf("run %s of another schedule = %+v", run.ID, r)
		}
	}

	// While deleting: it is listed, keeps its name and location, and
	// cannot be forgotten.
	sameName := newRepo(t, 3, alice, "photos", remote, "elsewhere")
	wantErr(t, "CreateRepo(name of a deleting repository)", s.CreateRepo(ctx, sameName), domain.ErrAlreadyExists)
	sameLocation := newRepo(t, 4, alice, "New", remote, "restic/photos")
	wantErr(t, "CreateRepo(location of a deleting repository)", s.CreateRepo(ctx, sameLocation), domain.ErrAlreadyExists)
	wantErr(t, "DeleteRepo(deleting)", s.DeleteRepo(ctx, r.ID, at), domain.ErrFailedPrecondition)
	if got := mustGetRepo(t, s, r.ID); got.Deleted() || got.Version != 2 {
		t.Errorf("a refused DeleteRepo changed the repository: %+v", got)
	}
	// Its cache can still be replaced and read.
	if err := s.ReplaceSnapshots(ctx, r.ID, nil, false, at); err != nil {
		t.Errorf("ReplaceSnapshots(deleting): %v", err)
	}
}

func testRepoPendingDeletions(t *testing.T, s Store) {
	alice := userID(t, 1)
	remote := requireRepos(t, s, alice)
	ctx := context.Background()
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	at := t0()
	// Requested one minute apart, in this order.
	names := []string{"succeeded", "failed", "cancelled", "interrupted", "running", "queued", "missing-old", "missing-new", "idle"}
	repos := map[string]restic.Repo{}
	for i, name := range names {
		r := newRepo(t, i+1, alice, name, remote, name)
		mustCreateRepo(t, s, r)
		if name != "idle" {
			var err error
			if r, err = s.MarkDeleting(ctx, r.ID, jobID(t, i+1), at.Add(time.Duration(i)*time.Minute)); err != nil {
				t.Fatal(err)
			}
		}
		repos[name] = r
	}
	job := func(name string, st domain.JobStatus, code, msg string) {
		r := repos[name]
		j := repoJob(t, 0, w.ID, alice, r.ID, restic.KindDeleteData)
		j.ID, j.Status, j.Error = r.DeletionJobID, st, jobs.JobError{Code: code, Message: msg}
		if st.Terminal() {
			j.FinishedAt = at.Add(time.Hour)
		}
		mustCreateJob(t, s, j)
	}
	job("succeeded", domain.JobSucceeded, "", "")
	job("failed", domain.JobFailed, "repository_locked", "another client holds a lock")
	job("cancelled", domain.JobCancelled, "cancelled", "")
	job("interrupted", domain.JobInterrupted, "worker_lost", "gone")
	job("running", domain.JobRunning, "", "")
	job("queued", domain.JobQueued, "", "")
	// A terminal job that is not the deletion job of "idle".
	stray := repoJob(t, 90, w.ID, alice, repos["idle"].ID, restic.KindDeleteData)
	stray.Status, stray.FinishedAt = domain.JobSucceeded, at
	mustCreateJob(t, s, stray)

	// The cutoff lies between the two repositories without a job.
	cutoff := at.Add(6*time.Minute + 30*time.Second)
	got, err := s.ListPendingDeletions(ctx, cutoff, -1)
	if err != nil {
		t.Fatal(err)
	}
	pd := func(name string, st domain.JobStatus, code, msg string) restic.PendingDeletion {
		r := repos[name]
		return restic.PendingDeletion{RepoID: r.ID, OwnerID: alice, JobID: r.DeletionJobID, RequestedAt: r.DeletionRequestedAt,
			JobStatus: st, JobErrorCode: code, JobErrorMessage: msg}
	}
	want := []restic.PendingDeletion{
		pd("succeeded", domain.JobSucceeded, "", ""),
		pd("failed", domain.JobFailed, "repository_locked", "another client holds a lock"),
		pd("cancelled", domain.JobCancelled, "cancelled", ""),
		pd("interrupted", domain.JobInterrupted, "worker_lost", "gone"),
		pd("missing-old", "", "", ""),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListPendingDeletions =\n%+v\nwant, oldest first,\n%+v", got, want)
	}
	// Exactly at the request time is not yet "before".
	if got, _ := s.ListPendingDeletions(ctx, repos["missing-old"].DeletionRequestedAt, -1); len(got) != 4 {
		t.Errorf("with the cutoff at the request time: %d deletions, want 4", len(got))
	}
	if got, err := s.ListPendingDeletions(ctx, cutoff, 2); err != nil || len(got) != 2 || got[0].RepoID != repos["succeeded"].ID || got[1].RepoID != repos["failed"].ID {
		t.Errorf("limit 2 = %+v, %v", got, err)
	}
	if got, err := s.ListPendingDeletions(ctx, cutoff, 0); err != nil || len(got) != 0 {
		t.Errorf("limit 0 = %+v, %v", got, err)
	}
	// A finished deletion is no longer pending.
	if err := s.FinishDeletion(ctx, repos["succeeded"].ID, repos["succeeded"].DeletionJobID, restic.DeletionOutcome{Succeeded: true}, at); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishDeletion(ctx, repos["failed"].ID, repos["failed"].DeletionJobID, restic.DeletionOutcome{Error: "x"}, at); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ListPendingDeletions(ctx, cutoff, -1); len(got) != 3 || got[0].RepoID != repos["cancelled"].ID {
		t.Errorf("after finishing two: %+v", got)
	}
}

func testRepoFinishDeletion(t *testing.T, s Store) {
	alice := userID(t, 1)
	remote := requireRepos(t, s, alice)
	requireSchedules(t, s, alice)
	ctx := context.Background()
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	ok := newRepo(t, 1, alice, "Succeeds", remote, "ok")
	bad := newRepo(t, 2, alice, "Fails", remote, "bad")
	mustCreateRepo(t, s, ok)
	mustCreateRepo(t, s, bad)
	for _, r := range []restic.Repo{ok, bad} {
		if err := s.ReplaceSnapshots(ctx, r.ID, []restic.Snapshot{testSnapshot(1, t0())}, false, t0()); err != nil {
			t.Fatal(err)
		}
	}
	sc := newSchedule(t, 1, alice, "Nightly", w.ID)
	sc.RepositoryID = bad.ID
	mustCreateSchedule(t, s, sc)
	job, stale := jobID(t, 1), jobID(t, 2)
	at := t0().Add(time.Hour)

	// Not deleting: nothing to finish.
	wantErr(t, "FinishDeletion(not deleting)", s.FinishDeletion(ctx, ok.ID, job, restic.DeletionOutcome{Succeeded: true}, at), domain.ErrConflict)
	wantErr(t, "FinishDeletion(missing)", s.FinishDeletion(ctx, repoID(t, 99), job, restic.DeletionOutcome{Succeeded: true}, at), domain.ErrConflict)
	for _, r := range []restic.Repo{ok, bad} {
		if _, err := s.MarkDeleting(ctx, r.ID, job, t0()); err != nil {
			t.Fatal(err)
		}
	}
	// Another attempt's job.
	for _, o := range []restic.DeletionOutcome{{Succeeded: true}, {Error: "x"}} {
		wantErr(t, "FinishDeletion(stale job)", s.FinishDeletion(ctx, ok.ID, stale, o, at), domain.ErrConflict)
	}
	if got := mustGetRepo(t, s, ok.ID); !got.Deleting || got.Deleted() || got.PasswordEnc == nil || got.Version != 2 {
		t.Fatalf("a refused FinishDeletion changed the repository: %+v", got)
	}

	// Success: soft-deleted, the password and the snapshots gone.
	if err := s.FinishDeletion(ctx, ok.ID, job, restic.DeletionOutcome{Succeeded: true, Error: "ignored"}, at); err != nil {
		t.Fatal(err)
	}
	got := mustGetRepo(t, s, ok.ID)
	if got.Deleting || !got.DeletedAt.Equal(at) || !got.DataDeletedAt.Equal(at) || !got.UpdatedAt.Equal(at) || got.PasswordEnc != nil ||
		got.Version != 3 || got.DeletionJobID != job || got.DeletionError != "" || got.Stats.SnapshotCount != 0 {
		t.Errorf("after a succeeded deletion = %+v", got)
	}
	if list, _ := s.ListSnapshots(ctx, ok.ID); len(list.Snapshots) != 0 {
		t.Error("the snapshot cache survived the deletion")
	}
	wantErr(t, "FinishDeletion(again)", s.FinishDeletion(ctx, ok.ID, job, restic.DeletionOutcome{Succeeded: true}, at), domain.ErrConflict)
	// Its name and location are free again.
	mustCreateRepo(t, s, newRepo(t, 3, alice, "succeeds", remote, "ok"))

	// Failure: active again with the error, clipped; the password, the
	// snapshots, the job and the paused schedule are kept.
	long := strings.Repeat("é", restic.MaxDeletionErrorLen+50)
	if err := s.FinishDeletion(ctx, bad.ID, job, restic.DeletionOutcome{Error: long}, at); err != nil {
		t.Fatal(err)
	}
	got = mustGetRepo(t, s, bad.ID)
	if got.Deleting || got.Deleted() || !got.DataDeletedAt.IsZero() || string(got.PasswordEnc) != "encrypted password" || got.Version != 3 ||
		got.DeletionJobID != job || !got.DeletionRequestedAt.Equal(t0()) || got.DeletionError != long[:2*restic.MaxDeletionErrorLen] ||
		!got.UpdatedAt.Equal(at) || got.Stats.SnapshotCount != 1 {
		t.Errorf("after a failed deletion = %+v", got)
	}
	if sc := mustGetSchedule(t, s, sc.ID); sc.Enabled || sc.PausedReason != schedules.ReasonRepositoryDeleting {
		t.Errorf("the schedule was resumed: %+v", sc)
	}
	// A new attempt clears the error and takes a new job.
	again, err := s.MarkDeleting(ctx, bad.ID, stale, at.Add(time.Hour))
	if err != nil || !again.Deleting || again.DeletionJobID != stale || again.DeletionError != "" || again.Version != 4 {
		t.Errorf("MarkDeleting after a failure = %+v, %v", again, err)
	}
	wantErr(t, "FinishDeletion(the earlier attempt's job)", s.FinishDeletion(ctx, bad.ID, job, restic.DeletionOutcome{Succeeded: true}, at), domain.ErrConflict)
}

func testRepoListShowsDeleting(t *testing.T, s Store) {
	alice := userID(t, 1)
	remote := requireRepos(t, s, alice)
	ctx := context.Background()
	deleting := newRepo(t, 1, alice, "Deleting", remote, "a")
	failed := newRepo(t, 2, alice, "Failed", remote, "b")
	mustCreateRepo(t, s, deleting)
	mustCreateRepo(t, s, failed)
	at := t0().Add(time.Hour)
	marked, err := s.MarkDeleting(ctx, deleting.ID, jobID(t, 1), at)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkDeleting(ctx, failed.ID, jobID(t, 2), at); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishDeletion(ctx, failed.ID, jobID(t, 2), restic.DeletionOutcome{Error: "the deletion was cancelled"}, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if !marked.Deleting || marked.DeletionJobID != jobID(t, 1) || !marked.DeletionRequestedAt.Equal(at) || marked.Version != 2 {
		t.Errorf("MarkDeleting returned %+v", marked)
	}
	list, err := s.ListRepos(ctx, restic.ListFilter{OwnerID: alice})
	if err != nil || len(list) != 2 {
		t.Fatalf("ListRepos = %v, %v", repoIDs(list), err)
	}
	for i, r := range list {
		if got := mustGetRepo(t, s, r.ID); !reflect.DeepEqual(got, r) {
			t.Errorf("GetRepo and ListRepos disagree on %s:\n%+v\n%+v", r.Name, got, r)
		}
		switch i {
		case 0:
			if !reflect.DeepEqual(r, marked) {
				t.Errorf("the deleting repository = %+v, want %+v", r, marked)
			}
		case 1:
			if r.Deleting || r.DeletionJobID != jobID(t, 2) || !r.DeletionRequestedAt.Equal(at) || r.DeletionError != "the deletion was cancelled" || r.Version != 3 {
				t.Errorf("the repository after a failed attempt = %+v", r)
			}
		}
	}
	if some, _ := s.GetRepos(ctx, []domain.ResticRepoID{deleting.ID}); len(some) != 1 || !some[0].Deleting || some[0].DeletionJobID != jobID(t, 1) {
		t.Errorf("GetRepos(deleting) = %+v", some)
	}
}

func testRepoNoAliasing(t *testing.T, s Store) {
	alice := userID(t, 1)
	remote := requireRepos(t, s, alice)
	ctx := context.Background()
	r := newRepo(t, 1, alice, "Photos", remote, "restic/photos")
	mustCreateRepo(t, s, r)
	snaps := []restic.Snapshot{testSnapshot(1, t0())}
	if err := s.ReplaceSnapshots(ctx, r.ID, snaps, false, t0()); err != nil {
		t.Fatal(err)
	}

	// Mutating the arguments after the call changes nothing.
	r.PasswordEnc[0] = 'X'
	snaps[0].Paths[0], snaps[0].Tags[0], snaps[0].Summary.FilesNew = "mutated", "mutated", 99
	check := func(what string) {
		t.Helper()
		got := mustGetRepo(t, s, r.ID)
		list, err := s.ListSnapshots(ctx, r.ID)
		if err != nil || string(got.PasswordEnc) != "encrypted password" || len(list.Snapshots) != 1 ||
			list.Snapshots[0].Paths[0] != "/srv/photos" || list.Snapshots[0].Tags[0] != "nightly" || list.Snapshots[0].Summary.FilesNew != 1 {
			t.Errorf("%s: the store aliases repository values: %+v, %+v (%v)", what, got, list.Snapshots, err)
		}
	}
	check("arguments")

	// Mutating returned values changes nothing either.
	got := mustGetRepo(t, s, r.ID)
	got.PasswordEnc[0] = 'Y'
	list, _ := s.ListRepos(ctx, restic.ListFilter{})
	list[0].PasswordEnc[0] = 'Z'
	some, _ := s.GetRepos(ctx, []domain.ResticRepoID{r.ID})
	some[0].PasswordEnc[0] = 'W'
	sl, _ := s.ListSnapshots(ctx, r.ID)
	sl.Snapshots[0].Paths[0], sl.Snapshots[0].Tags[0], sl.Snapshots[0].Summary.FilesNew = "mutated", "mutated", 99
	check("results")

	upd := mustGetRepo(t, s, r.ID)
	upd.PasswordEnc = []byte("second password")
	stored, err := s.UpdateRepo(ctx, upd)
	if err != nil {
		t.Fatal(err)
	}
	upd.PasswordEnc[0], stored.PasswordEnc[0] = 'X', 'Y'
	if got := mustGetRepo(t, s, r.ID); string(got.PasswordEnc) != "second password" {
		t.Errorf("UpdateRepo aliases the password: %q", got.PasswordEnc)
	}
}

func testJobAndScheduleRepository(t *testing.T, s Store) {
	alice := userID(t, 1)
	remote := requireRepos(t, s, alice)
	requireSchedules(t, s, alice)
	ctx := context.Background()
	w := newWorker(t, 1)
	mustCreateWorker(t, s, w)
	r := newRepo(t, 1, alice, "Photos", remote, "restic/photos")
	other := newRepo(t, 2, alice, "Other", remote, "other")
	mustCreateRepo(t, s, r)
	mustCreateRepo(t, s, other)

	onRepo := repoJob(t, 1, w.ID, alice, r.ID, restic.KindBackup)
	onRepo.RemoteIDs = []domain.RemoteID{remote}
	onOther := repoJob(t, 2, w.ID, alice, other.ID, restic.KindCheck)
	plain := newJob(t, 3, w.ID)
	plain.OwnerID = alice
	for _, j := range []jobs.Job{onRepo, onOther, plain} {
		mustCreateJob(t, s, j)
	}
	unknown := repoJob(t, 4, w.ID, alice, repoID(t, 99), restic.KindBackup)
	wantErr(t, "CreateJob(unknown repository)", s.CreateJob(ctx, unknown), domain.ErrNotFound)

	got, err := s.GetJob(ctx, onRepo.ID)
	if err != nil || got.RepositoryID != r.ID {
		t.Errorf("GetJob repository = %s, %v", got.RepositoryID, err)
	}
	if got, _ := s.GetJob(ctx, plain.ID); !got.RepositoryID.IsZero() {
		t.Errorf("a job without a repository has %s", got.RepositoryID)
	}
	// An update never changes it.
	got.RepositoryID, got.Status = other.ID, domain.JobDispatched
	if stored, err := s.UpdateJob(ctx, got); err != nil || stored.RepositoryID != r.ID {
		t.Errorf("after UpdateJob the repository is %s, %v", stored.RepositoryID, err)
	}
	for _, tt := range []struct {
		name string
		f    jobs.ListFilter
		want []domain.JobID
	}{
		{"a repository", jobs.ListFilter{RepositoryID: r.ID}, []domain.JobID{onRepo.ID}},
		{"another", jobs.ListFilter{RepositoryID: other.ID}, []domain.JobID{onOther.ID}},
		{"none", jobs.ListFilter{RepositoryID: repoID(t, 99)}, []domain.JobID{}},
		{"with the owner", jobs.ListFilter{RepositoryID: r.ID, OwnerID: userID(t, 2)}, []domain.JobID{}},
		{"no filter", jobs.ListFilter{}, []domain.JobID{plain.ID, onOther.ID, onRepo.ID}},
	} {
		tt.f.Limit = 10
		list, err := s.ListJobs(ctx, tt.f)
		if err != nil || !slices.Equal(jobIDs(list), tt.want) {
			t.Errorf("ListJobs(%s) = %v, %v; want %v", tt.name, jobIDs(list), err, tt.want)
		}
	}
	// The job history survives the repository's deletion.
	if err := s.DeleteRepo(ctx, r.ID, t0()); err != nil {
		t.Fatal(err)
	}
	if list, err := s.ListJobs(ctx, jobs.ListFilter{RepositoryID: r.ID, Limit: 10}); err != nil || len(list) != 1 {
		t.Errorf("jobs of a deleted repository = %v, %v", jobIDs(list), err)
	}

	a := newSchedule(t, 1, alice, "On other", w.ID)
	a.RepositoryID = other.ID
	b := newSchedule(t, 2, alice, "Plain", w.ID)
	mustCreateSchedule(t, s, a)
	mustCreateSchedule(t, s, b)
	bad := newSchedule(t, 3, alice, "Unknown", w.ID)
	bad.RepositoryID = repoID(t, 99)
	wantErr(t, "CreateSchedule(unknown repository)", s.CreateSchedule(ctx, bad), domain.ErrNotFound)
	if got := mustGetSchedule(t, s, a.ID); got.RepositoryID != other.ID {
		t.Errorf("GetSchedule repository = %s", got.RepositoryID)
	}
	if got := mustGetSchedule(t, s, b.ID); !got.RepositoryID.IsZero() {
		t.Errorf("a schedule without a repository has %s", got.RepositoryID)
	}
	for _, tt := range []struct {
		f    schedules.ListFilter
		want []domain.ScheduleID
	}{
		{schedules.ListFilter{RepositoryID: other.ID}, []domain.ScheduleID{a.ID}},
		{schedules.ListFilter{RepositoryID: r.ID}, []domain.ScheduleID{}},
		{schedules.ListFilter{}, []domain.ScheduleID{a.ID, b.ID}},
	} {
		list, err := s.ListSchedules(ctx, tt.f)
		if err != nil || !slices.Equal(scheduleIDs(list), tt.want) {
			t.Errorf("ListSchedules(%+v) = %v, %v; want %v", tt.f, scheduleIDs(list), err, tt.want)
		}
	}
	// An update may move a schedule to another repository (its definition
	// changed) or to none.
	upd := mustGetSchedule(t, s, a.ID)
	upd.RepositoryID = domain.ResticRepoID{}
	if stored, err := s.UpdateSchedule(ctx, upd, false); err != nil || !stored.RepositoryID.IsZero() {
		t.Errorf("UpdateSchedule(no repository) = %s, %v", stored.RepositoryID, err)
	}
}
