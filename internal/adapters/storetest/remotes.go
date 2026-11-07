package storetest

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// This file is the contract suite of remotes.Repository.

func newRemote(t *testing.T, n int, owner domain.UserID, name string) remotes.Remote {
	return remotes.Remote{
		ID: remoteID(t, n), OwnerID: owner, Name: name, Type: "s3",
		Options:    map[string]string{"provider": "Minio", "endpoint": "https://minio.example"},
		SecretsEnc: []byte("encrypted secrets"), SecretKeys: []string{"secret_access_key"},
		CreatedAt: t0(), UpdatedAt: t0(), Version: 1,
	}
}

func mustCreateRemote(t *testing.T, s Store, r remotes.Remote) {
	t.Helper()
	err := s.CreateRemote(context.Background(), r)
	skipIfUnsupported(t, err)
	if err != nil {
		t.Fatalf("CreateRemote(%s): %v", r.Name, err)
	}
}

// requireRemotes creates the users of the test and skips it if the store
// does not implement the remotes port yet.
func requireRemotes(t *testing.T, s Store, owners ...domain.UserID) {
	t.Helper()
	for _, o := range owners {
		ensureUser(t, s, o)
	}
	_, err := s.ListRemotes(context.Background(), remotes.ListFilter{})
	skipIfUnsupported(t, err)
	if err != nil {
		t.Fatalf("ListRemotes: %v", err)
	}
}

func remoteIDs(rs []remotes.Remote) []domain.RemoteID {
	out := make([]domain.RemoteID, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}

func testRemoteCreateGetList(t *testing.T, s Store) {
	alice, bob := userID(t, 1), userID(t, 2)
	requireRemotes(t, s, alice, bob)
	ctx := context.Background()
	photos, archive, beta, bobs := newRemote(t, 1, alice, "Photos"), newRemote(t, 2, alice, "archive"), newRemote(t, 3, alice, "Beta"), newRemote(t, 4, bob, "Photos")
	photos.Type, photos.Options, photos.SecretsEnc, photos.SecretKeys = "local", map[string]string{"path": "/srv/photos"}, nil, nil
	for _, r := range []remotes.Remote{photos, archive, beta, bobs} {
		mustCreateRemote(t, s, r)
	}
	wantErr(t, "CreateRemote(duplicate id)", s.CreateRemote(ctx, newRemote(t, 1, alice, "Other")), domain.ErrAlreadyExists)
	wantErr(t, "CreateRemote(unknown owner)", s.CreateRemote(ctx, newRemote(t, 5, userID(t, 99), "X")), domain.ErrNotFound)

	got, err := s.GetRemote(ctx, archive.ID)
	if err != nil || got.OwnerID != alice || got.Name != "archive" || got.Type != "s3" || !maps.Equal(got.Options, archive.Options) ||
		string(got.SecretsEnc) != "encrypted secrets" || !slices.Equal(got.SecretKeys, archive.SecretKeys) || len(got.Members) != 0 ||
		!got.CreatedAt.Equal(t0()) || !got.UpdatedAt.Equal(t0()) || got.Deleted() || got.Version != 1 {
		t.Errorf("GetRemote = (%+v, %v)", got, err)
	}
	if got, err := s.GetRemote(ctx, photos.ID); err != nil || got.SecretsEnc != nil || len(got.SecretKeys) != 0 {
		t.Errorf("GetRemote(no secrets) = (%+v, %v)", got, err)
	}
	_, err = s.GetRemote(ctx, remoteID(t, 99))
	wantErr(t, "GetRemote(missing)", err, domain.ErrNotFound)

	some, err := s.GetRemotes(ctx, []domain.RemoteID{beta.ID, photos.ID, remoteID(t, 99), photos.ID})
	if err != nil || !slices.Equal(remoteIDs(some), []domain.RemoteID{photos.ID, beta.ID}) {
		t.Errorf("GetRemotes = (%v, %v), want existing ones by ID, once", remoteIDs(some), err)
	}
	if none, err := s.GetRemotes(ctx, nil); err != nil || len(none) != 0 {
		t.Errorf("GetRemotes(nil) = (%v, %v)", none, err)
	}

	for _, tt := range []struct {
		f    remotes.ListFilter
		want []domain.RemoteID
	}{
		{remotes.ListFilter{OwnerID: alice}, []domain.RemoteID{archive.ID, beta.ID, photos.ID}},
		{remotes.ListFilter{OwnerID: alice, Type: "s3"}, []domain.RemoteID{archive.ID, beta.ID}},
		{remotes.ListFilter{OwnerID: bob}, []domain.RemoteID{bobs.ID}},
		{remotes.ListFilter{OwnerID: userID(t, 99)}, []domain.RemoteID{}},
		{remotes.ListFilter{}, []domain.RemoteID{archive.ID, beta.ID, photos.ID, bobs.ID}},
	} {
		list, err := s.ListRemotes(ctx, tt.f)
		if err != nil || !slices.Equal(remoteIDs(list), tt.want) {
			t.Errorf("ListRemotes(%+v) = (%v, %v), want %v (by lower-cased name, then ID)", tt.f, remoteIDs(list), err, tt.want)
		}
	}
}

func testRemoteNamesUniqueAmongLive(t *testing.T, s Store) {
	alice, bob := userID(t, 1), userID(t, 2)
	requireRemotes(t, s, alice, bob)
	ctx := context.Background()
	first := newRemote(t, 1, alice, "Photos")
	mustCreateRemote(t, s, first)
	wantErr(t, "CreateRemote(same name, other case)", s.CreateRemote(ctx, newRemote(t, 2, alice, "PHOTOS")), domain.ErrAlreadyExists)
	mustCreateRemote(t, s, newRemote(t, 3, bob, "photos")) // other owner

	other := newRemote(t, 4, alice, "Other")
	mustCreateRemote(t, s, other)
	renamed := other
	renamed.Name = "photos"
	_, err := s.UpdateRemote(ctx, renamed)
	wantErr(t, "UpdateRemote(name taken)", err, domain.ErrAlreadyExists)
	if got, _ := s.GetRemote(ctx, other.ID); got.Name != "Other" || got.Version != 1 {
		t.Errorf("a failed rename changed the remote: %+v", got)
	}

	// After a soft delete the name is free again; the deleted remote keeps it.
	if err := s.DeleteRemote(ctx, first.ID, t0()); err != nil {
		t.Fatal(err)
	}
	mustCreateRemote(t, s, newRemote(t, 5, alice, "photos"))
	if got, _ := s.GetRemote(ctx, first.ID); got.Name != "Photos" {
		t.Errorf("deleted remote name = %q", got.Name)
	}
	// Renaming to its own name in another case is fine.
	self := newRemote(t, 6, alice, "Docs")
	mustCreateRemote(t, s, self)
	self.Name = "DOCS"
	if _, err := s.UpdateRemote(ctx, self); err != nil {
		t.Errorf("UpdateRemote(own name, other case) = %v", err)
	}
}

func testRemoteUpdateVersioned(t *testing.T, s Store) {
	alice := userID(t, 1)
	requireRemotes(t, s, alice, userID(t, 2))
	ctx := context.Background()
	r := newRemote(t, 1, alice, "Archive")
	mustCreateRemote(t, s, r)

	next := r
	next.Name, next.Options = "Archive 2", map[string]string{"provider": "AWS"}
	next.SecretsEnc, next.SecretKeys, next.UpdatedAt = []byte("new secrets"), []string{"access_key_id", "secret_access_key"}, t0().Add(time.Hour)
	// Immutable fields are never written.
	next.Type, next.OwnerID, next.CreatedAt = "sftp", userID(t, 2), t0().Add(time.Minute)
	stored, err := s.UpdateRemote(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Version != 2 || stored.Name != "Archive 2" || !maps.Equal(stored.Options, next.Options) ||
		string(stored.SecretsEnc) != "new secrets" || !slices.Equal(stored.SecretKeys, next.SecretKeys) || !stored.UpdatedAt.Equal(t0().Add(time.Hour)) {
		t.Errorf("UpdateRemote returned %+v", stored)
	}
	if stored.Type != "s3" || stored.OwnerID != alice || !stored.CreatedAt.Equal(t0()) {
		t.Errorf("UpdateRemote wrote immutable fields: %+v", stored)
	}
	// Secrets can be cleared.
	cleared := stored
	cleared.SecretsEnc, cleared.SecretKeys = nil, nil
	if got, err := s.UpdateRemote(ctx, cleared); err != nil || got.SecretsEnc != nil || len(got.SecretKeys) != 0 || got.Version != 3 {
		t.Errorf("UpdateRemote(clear secrets) = (%+v, %v)", got, err)
	}

	_, err = s.UpdateRemote(ctx, r) // version 1
	wantErr(t, "UpdateRemote(stale version)", err, domain.ErrConflict)
	_, err = s.UpdateRemote(ctx, newRemote(t, 9, alice, "Missing"))
	wantErr(t, "UpdateRemote(missing)", err, domain.ErrNotFound)

	cur, _ := s.GetRemote(ctx, r.ID)
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := range 20 {
		wg.Go(func() {
			next := cur
			next.Name = fmt.Sprintf("racer %d", i)
			if _, err := s.UpdateRemote(ctx, next); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			} else if !errors.Is(err, domain.ErrConflict) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("%d concurrent updates of one version succeeded, want 1", wins)
	}
}

func testRemoteUnionMembers(t *testing.T, s Store) {
	alice := userID(t, 1)
	requireRemotes(t, s, alice)
	ctx := context.Background()
	m1, m2, m3 := newRemote(t, 1, alice, "Disk 1"), newRemote(t, 2, alice, "Disk 2"), newRemote(t, 3, alice, "Disk 3")
	for _, r := range []remotes.Remote{m1, m2, m3} {
		mustCreateRemote(t, s, r)
	}
	union := newRemote(t, 10, alice, "Pool")
	union.Type, union.Options, union.SecretsEnc, union.SecretKeys = "union", map[string]string{"action_policy": "epall"}, nil, nil
	union.Members = []remotes.Member{
		{RemoteID: m2.ID, Path: "a/b", Mode: backend.UnionReadOnly},
		{RemoteID: m1.ID, Path: "", Mode: backend.UnionReadWrite},
	}
	mustCreateRemote(t, s, union)
	got, err := s.GetRemote(ctx, union.ID)
	if err != nil || !slices.Equal(got.Members, union.Members) {
		t.Errorf("GetRemote(union).Members = %+v (%v), want %+v in order", got.Members, err, union.Members)
	}

	missing := newRemote(t, 11, alice, "Broken")
	missing.Members = []remotes.Member{{RemoteID: m1.ID, Mode: backend.UnionReadWrite}, {RemoteID: remoteID(t, 99), Mode: backend.UnionReadWrite}}
	wantErr(t, "CreateRemote(unknown member)", s.CreateRemote(ctx, missing), domain.ErrNotFound)
	self := newRemote(t, 12, alice, "Self")
	self.Members = []remotes.Member{{RemoteID: self.ID, Mode: backend.UnionReadWrite}, {RemoteID: m1.ID, Mode: backend.UnionReadWrite}}
	wantErr(t, "CreateRemote(contains itself)", s.CreateRemote(ctx, self), domain.ErrInvalidArgument)
	if _, err := s.GetRemote(ctx, self.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("a rejected union was stored: %v", err)
	}

	using, err := s.ListUnionsUsing(ctx, m1.ID)
	if err != nil || !slices.Equal(remoteIDs(using), []domain.RemoteID{union.ID}) {
		t.Errorf("ListUnionsUsing(m1) = (%v, %v)", remoteIDs(using), err)
	}
	if using, err := s.ListUnionsUsing(ctx, m3.ID); err != nil || len(using) != 0 {
		t.Errorf("ListUnionsUsing(unused) = (%v, %v)", remoteIDs(using), err)
	}

	// Members are replaced as a whole on update.
	next := got
	next.Members = []remotes.Member{{RemoteID: m3.ID, Path: "x", Mode: backend.UnionNoCreate}, {RemoteID: m2.ID, Mode: backend.UnionReadWrite}}
	stored, err := s.UpdateRemote(ctx, next)
	if err != nil || !slices.Equal(stored.Members, next.Members) {
		t.Errorf("UpdateRemote(members) = (%+v, %v)", stored.Members, err)
	}
	if using, _ := s.ListUnionsUsing(ctx, m1.ID); len(using) != 0 {
		t.Errorf("m1 is still used by %v after it was removed from the union", remoteIDs(using))
	}
	bad := stored
	bad.Members = []remotes.Member{{RemoteID: remoteID(t, 99), Mode: backend.UnionReadWrite}, {RemoteID: m1.ID, Mode: backend.UnionReadWrite}}
	_, err = s.UpdateRemote(ctx, bad)
	wantErr(t, "UpdateRemote(unknown member)", err, domain.ErrNotFound)
	if got, _ := s.GetRemote(ctx, union.ID); got.Version != stored.Version || !slices.Equal(got.Members, stored.Members) {
		t.Errorf("a failed update changed the union: %+v", got)
	}

	// A member of a live union cannot be deleted; nothing changes.
	err = s.DeleteRemote(ctx, m3.ID, t0())
	wantErr(t, "DeleteRemote(member of a live union)", err, domain.ErrFailedPrecondition)
	if got, _ := s.GetRemote(ctx, m3.ID); got.Deleted() || got.Version != 1 {
		t.Errorf("a refused delete changed the member: %+v", got)
	}
	// Once the union is deleted, its members are free.
	if err := s.DeleteRemote(ctx, union.ID, t0()); err != nil {
		t.Fatal(err)
	}
	if using, _ := s.ListUnionsUsing(ctx, m3.ID); len(using) != 0 {
		t.Errorf("a deleted union still uses m3: %v", remoteIDs(using))
	}
	if err := s.DeleteRemote(ctx, m3.ID, t0()); err != nil {
		t.Errorf("DeleteRemote(member of a deleted union) = %v", err)
	}
	// The deleted union keeps its members (job history).
	if got, _ := s.GetRemote(ctx, union.ID); len(got.Members) != 2 {
		t.Errorf("deleted union members = %+v", got.Members)
	}
	// A deleted remote cannot become a member.
	withDeleted := newRemote(t, 13, alice, "With deleted")
	withDeleted.Members = []remotes.Member{{RemoteID: m2.ID, Mode: backend.UnionReadWrite}, {RemoteID: m3.ID, Mode: backend.UnionReadWrite}}
	wantErr(t, "CreateRemote(deleted member)", s.CreateRemote(ctx, withDeleted), domain.ErrNotFound)

	t.Run("DeleteRace", func(t *testing.T) { testRemoteMemberDeleteRace(t, s, alice) })
}

// testRemoteMemberDeleteRace races the deletion of a remote with the
// creation of a union that has it as a member: exactly one of them wins, so
// a live union never has a deleted member.
func testRemoteMemberDeleteRace(t *testing.T, s Store, owner domain.UserID) {
	ctx := context.Background()
	other := newRemote(t, 100, owner, "Race anchor")
	mustCreateRemote(t, s, other)
	for i := range 10 {
		member := newRemote(t, 200+i, owner, fmt.Sprintf("Race member %d", i))
		mustCreateRemote(t, s, member)
		union := newRemote(t, 300+i, owner, fmt.Sprintf("Race union %d", i))
		union.Members = []remotes.Member{{RemoteID: member.ID, Mode: backend.UnionReadWrite}, {RemoteID: other.ID, Mode: backend.UnionReadWrite}}
		var createErr, deleteErr error
		var wg sync.WaitGroup
		wg.Go(func() { createErr = s.CreateRemote(ctx, union) })
		wg.Go(func() { deleteErr = s.DeleteRemote(ctx, member.ID, t0()) })
		wg.Wait()
		switch {
		case createErr == nil && deleteErr == nil:
			t.Fatalf("round %d: the union was created and its member deleted", i)
		case createErr == nil:
			wantErr(t, "DeleteRemote(member of the new union)", deleteErr, domain.ErrFailedPrecondition)
		case deleteErr == nil:
			wantErr(t, "CreateRemote(with the deleted member)", createErr, domain.ErrNotFound)
		default:
			t.Fatalf("round %d: both failed: %v; %v", i, createErr, deleteErr)
		}
		if createErr == nil {
			if err := s.DeleteRemote(ctx, union.ID, t0()); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func testRemoteSoftDelete(t *testing.T, s Store) {
	alice := userID(t, 1)
	requireRemotes(t, s, alice)
	ctx := context.Background()
	r := newRemote(t, 1, alice, "Archive")
	mustCreateRemote(t, s, r)
	at := t0().Add(time.Hour)
	if err := s.DeleteRemote(ctx, r.ID, at); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRemote(ctx, r.ID)
	if err != nil || !got.DeletedAt.Equal(at) || got.SecretsEnc != nil || got.Version != 2 || got.Name != "Archive" {
		t.Errorf("GetRemote(deleted) = (%+v, %v): want DeletedAt set, secrets wiped, version 2", got, err)
	}
	if list, _ := s.ListRemotes(ctx, remotes.ListFilter{OwnerID: alice}); len(list) != 0 {
		t.Errorf("ListRemotes lists a deleted remote: %v", remoteIDs(list))
	}
	if some, _ := s.GetRemotes(ctx, []domain.RemoteID{r.ID}); len(some) != 1 || !some[0].Deleted() {
		t.Errorf("GetRemotes(deleted) = %+v, want the deleted remote", some)
	}
	wantErr(t, "DeleteRemote(deleted)", s.DeleteRemote(ctx, r.ID, at), domain.ErrNotFound)
	wantErr(t, "DeleteRemote(missing)", s.DeleteRemote(ctx, remoteID(t, 99), at), domain.ErrNotFound)
	upd := got
	upd.Name = "revived"
	_, err = s.UpdateRemote(ctx, upd)
	wantErr(t, "UpdateRemote(deleted)", err, domain.ErrNotFound)
}

func testRemoteNoAliasing(t *testing.T, s Store) {
	alice := userID(t, 1)
	requireRemotes(t, s, alice)
	ctx := context.Background()
	m1, m2 := newRemote(t, 1, alice, "M1"), newRemote(t, 2, alice, "M2")
	mustCreateRemote(t, s, m1)
	mustCreateRemote(t, s, m2)
	u := newRemote(t, 3, alice, "Union")
	u.Members = []remotes.Member{{RemoteID: m1.ID, Path: "p", Mode: backend.UnionReadWrite}, {RemoteID: m2.ID, Mode: backend.UnionReadWrite}}
	mustCreateRemote(t, s, u)

	// Mutating the arguments after the call changes nothing.
	u.Options["provider"], u.SecretsEnc[0], u.SecretKeys[0], u.Members[0].Path = "mutated", 'X', "mutated", "mutated"
	check := func(what string) {
		t.Helper()
		got, err := s.GetRemote(ctx, u.ID)
		if err != nil || got.Options["provider"] != "Minio" || string(got.SecretsEnc) != "encrypted secrets" ||
			got.SecretKeys[0] != "secret_access_key" || got.Members[0].Path != "p" {
			t.Errorf("%s: store aliases remote values: %+v (%v)", what, got, err)
		}
	}
	check("CreateRemote argument")

	mutate := func(r remotes.Remote) {
		r.Options["provider"], r.SecretsEnc[0], r.SecretKeys[0], r.Members[0].Path = "mutated", 'X', "mutated", "mutated"
	}
	got, _ := s.GetRemote(ctx, u.ID)
	mutate(got)
	check("GetRemote result")
	list, _ := s.ListRemotes(ctx, remotes.ListFilter{OwnerID: alice, Type: "s3"})
	for _, r := range list {
		if r.ID == u.ID {
			mutate(r)
		}
	}
	check("ListRemotes result")
	some, _ := s.GetRemotes(ctx, []domain.RemoteID{u.ID})
	mutate(some[0])
	check("GetRemotes result")
	using, _ := s.ListUnionsUsing(ctx, m1.ID)
	mutate(using[0])
	check("ListUnionsUsing result")

	cur, _ := s.GetRemote(ctx, u.ID)
	stored, err := s.UpdateRemote(ctx, cur)
	if err != nil {
		t.Fatal(err)
	}
	mutate(stored)
	mutate(cur)
	check("UpdateRemote argument and result")
}
