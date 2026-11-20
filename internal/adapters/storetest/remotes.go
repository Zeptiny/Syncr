package storetest

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
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
	if err != nil {
		t.Fatalf("CreateRemote(%s): %v", r.Name, err)
	}
}

// requireRemotes creates the users of the test and checks that the remotes
// port works.
func requireRemotes(t *testing.T, s Store, owners ...domain.UserID) {
	t.Helper()
	for _, o := range owners {
		ensureUser(t, s, o)
	}
	_, err := s.ListRemotes(context.Background(), remotes.ListFilter{})
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

	using, err := s.ListRemotesUsing(ctx, m1.ID)
	if err != nil || !slices.Equal(remoteIDs(using), []domain.RemoteID{union.ID}) {
		t.Errorf("ListRemotesUsing(m1) = (%v, %v)", remoteIDs(using), err)
	}
	if using, err := s.ListRemotesUsing(ctx, m3.ID); err != nil || len(using) != 0 {
		t.Errorf("ListRemotesUsing(unused) = (%v, %v)", remoteIDs(using), err)
	}

	// Members are replaced as a whole on update.
	next := got
	next.Members = []remotes.Member{{RemoteID: m3.ID, Path: "x", Mode: backend.UnionNoCreate}, {RemoteID: m2.ID, Mode: backend.UnionReadWrite}}
	stored, err := s.UpdateRemote(ctx, next)
	if err != nil || !slices.Equal(stored.Members, next.Members) {
		t.Errorf("UpdateRemote(members) = (%+v, %v)", stored.Members, err)
	}
	if using, _ := s.ListRemotesUsing(ctx, m1.ID); len(using) != 0 {
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
	if using, _ := s.ListRemotesUsing(ctx, m3.ID); len(using) != 0 {
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
	using, _ := s.ListRemotesUsing(ctx, m1.ID)
	mutate(using[0])
	check("ListRemotesUsing result")

	cur, _ := s.GetRemote(ctx, u.ID)
	stored, err := s.UpdateRemote(ctx, cur)
	if err != nil {
		t.Fatal(err)
	}
	mutate(stored)
	mutate(cur)
	check("UpdateRemote argument and result")
}

// newWrapper is a crypt remote that stores its data at path on wrapped.
func newWrapper(t *testing.T, n int, owner domain.UserID, name string, wrapped domain.RemoteID, path string) remotes.Remote {
	r := newRemote(t, n, owner, name)
	r.Type, r.Options, r.SecretKeys = "crypt", map[string]string{"filename_encryption": "standard"}, []string{"password"}
	r.Wrapped = remotes.Wrapped{RemoteID: wrapped, Path: path}
	return r
}

func testRemoteWrapped(t *testing.T, s Store) {
	alice := userID(t, 1)
	requireRemotes(t, s, alice)
	ctx := context.Background()
	bucket, other := newRemote(t, 1, alice, "Bucket"), newRemote(t, 2, alice, "Other")
	mustCreateRemote(t, s, bucket)
	mustCreateRemote(t, s, other)

	vault := newWrapper(t, 10, alice, "Vault", bucket.ID, "vault/data")
	mustCreateRemote(t, s, vault)
	got, err := s.GetRemote(ctx, vault.ID)
	if err != nil || got.Wrapped != vault.Wrapped || got.Type != "crypt" {
		t.Errorf("GetRemote(wrapper) = (%+v, %v), want wrapped %+v", got, err, vault.Wrapped)
	}
	if got, _ := s.GetRemote(ctx, bucket.ID); !got.Wrapped.IsZero() {
		t.Errorf("a remote that wraps nothing has %+v", got.Wrapped)
	}
	for what, list := range map[string]func() ([]remotes.Remote, error){
		"GetRemotes": func() ([]remotes.Remote, error) { return s.GetRemotes(ctx, []domain.RemoteID{vault.ID}) },
		"ListRemotes": func() ([]remotes.Remote, error) {
			return s.ListRemotes(ctx, remotes.ListFilter{OwnerID: alice, Type: "crypt"})
		},
	} {
		if rs, err := list(); err != nil || len(rs) != 1 || rs[0].Wrapped != vault.Wrapped {
			t.Errorf("%s(wrapper) = (%+v, %v)", what, rs, err)
		}
	}

	// The wrapped remote must exist, be live and not be the remote itself;
	// a path needs a remote.
	gone := newRemote(t, 3, alice, "Gone")
	mustCreateRemote(t, s, gone)
	if err := s.DeleteRemote(ctx, gone.ID, t0()); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		r    remotes.Remote
		want error
	}{
		{"missing wrapped remote", newWrapper(t, 11, alice, "Missing", remoteID(t, 99), ""), domain.ErrNotFound},
		{"deleted wrapped remote", newWrapper(t, 12, alice, "Deleted", gone.ID, ""), domain.ErrNotFound},
		{"wraps itself", newWrapper(t, 13, alice, "Self", remoteID(t, 13), ""), domain.ErrInvalidArgument},
		{"a path without a remote", newWrapper(t, 14, alice, "Pathless", domain.RemoteID{}, "x"), domain.ErrInvalidArgument},
	} {
		wantErr(t, "CreateRemote("+tt.name+")", s.CreateRemote(ctx, tt.r), tt.want)
		if _, err := s.GetRemote(ctx, tt.r.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("%s: the remote was stored: %v", tt.name, err)
		}
	}

	// Unions and wrappers are both "using" a remote.
	pool := newRemote(t, 20, alice, "A pool")
	pool.Type, pool.Members = "union", []remotes.Member{{RemoteID: bucket.ID, Mode: backend.UnionReadWrite}, {RemoteID: other.ID, Mode: backend.UnionReadWrite}}
	mustCreateRemote(t, s, pool)
	using, err := s.ListRemotesUsing(ctx, bucket.ID)
	if err != nil || !slices.Equal(remoteIDs(using), []domain.RemoteID{pool.ID, vault.ID}) {
		t.Errorf("ListRemotesUsing(bucket) = (%v, %v), want the union and the wrapper by name", remoteIDs(using), err)
	}
	if len(using) == 2 && (using[1].Wrapped != vault.Wrapped || len(using[0].Members) != 2) {
		t.Errorf("ListRemotesUsing returned %+v", using)
	}
	if using, _ := s.ListRemotesUsing(ctx, vault.ID); len(using) != 0 {
		t.Errorf("ListRemotesUsing(the wrapper) = %v", remoteIDs(using))
	}
	if err := s.DeleteRemote(ctx, pool.ID, t0()); err != nil {
		t.Fatal(err)
	}

	// The wrapped remote cannot be deleted while a live wrapper uses it;
	// the refusal names the wrapper and changes nothing.
	err = s.DeleteRemote(ctx, bucket.ID, t0())
	wantErr(t, "DeleteRemote(wrapped remote)", err, domain.ErrFailedPrecondition)
	if err != nil && !strings.Contains(err.Error(), `encrypted remote "Vault"`) {
		t.Errorf("the refusal does not name the wrapper: %v", err)
	}
	if got, _ := s.GetRemote(ctx, bucket.ID); got.Deleted() || got.Version != 1 {
		t.Errorf("a refused delete changed the wrapped remote: %+v", got)
	}

	// Update moves the wrapper to another remote and folder.
	cur, _ := s.GetRemote(ctx, vault.ID)
	moved := cur
	moved.Wrapped = remotes.Wrapped{RemoteID: other.ID, Path: ""}
	stored, err := s.UpdateRemote(ctx, moved)
	if err != nil || stored.Wrapped != moved.Wrapped || stored.Version != cur.Version+1 {
		t.Errorf("UpdateRemote(wrapped) = (%+v, %v)", stored, err)
	}
	if got, _ := s.GetRemote(ctx, vault.ID); got.Wrapped != moved.Wrapped {
		t.Errorf("stored wrapped = %+v", got.Wrapped)
	}
	if using, _ := s.ListRemotesUsing(ctx, bucket.ID); len(using) != 0 {
		t.Errorf("the old wrapped remote is still used by %v", remoteIDs(using))
	}
	for _, tt := range []struct {
		name    string
		wrapped remotes.Wrapped
		want    error
	}{
		{"a deleted remote", remotes.Wrapped{RemoteID: gone.ID}, domain.ErrNotFound},
		{"itself", remotes.Wrapped{RemoteID: vault.ID}, domain.ErrInvalidArgument},
	} {
		bad := stored
		bad.Wrapped = tt.wrapped
		_, err := s.UpdateRemote(ctx, bad)
		wantErr(t, "UpdateRemote(wrapped: "+tt.name+")", err, tt.want)
	}
	if got, _ := s.GetRemote(ctx, vault.ID); got.Version != stored.Version || got.Wrapped != moved.Wrapped {
		t.Errorf("a failed update changed the wrapper: %+v", got)
	}
	if err := s.DeleteRemote(ctx, bucket.ID, t0()); err != nil {
		t.Errorf("DeleteRemote(no longer wrapped) = %v", err)
	}

	// Once the wrapper is deleted, its wrapped remote is free; the deleted
	// wrapper keeps it (job history).
	if err := s.DeleteRemote(ctx, vault.ID, t0()); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetRemote(ctx, vault.ID); got.Wrapped != moved.Wrapped {
		t.Errorf("the deleted wrapper lost its wrapped remote: %+v", got.Wrapped)
	}
	if using, _ := s.ListRemotesUsing(ctx, other.ID); len(using) != 0 {
		t.Errorf("a deleted wrapper still uses its remote: %v", remoteIDs(using))
	}
	if err := s.DeleteRemote(ctx, other.ID, t0()); err != nil {
		t.Errorf("DeleteRemote(wrapped by a deleted wrapper) = %v", err)
	}

	t.Run("DeleteRace", func(t *testing.T) { testRemoteWrappedDeleteRace(t, s, alice) })
}

// testRemoteWrappedDeleteRace races the deletion of a remote with the
// creation of a wrapper that stores its data in it, and with the update of
// a wrapper that moves to it: exactly one of them wins, so a live wrapper
// never wraps a deleted remote.
func testRemoteWrappedDeleteRace(t *testing.T, s Store, owner domain.UserID) {
	ctx := context.Background()
	anchor := newRemote(t, 400, owner, "Wrap race anchor")
	mustCreateRemote(t, s, anchor)
	mover := newWrapper(t, 401, owner, "Wrap race mover", anchor.ID, "")
	mustCreateRemote(t, s, mover)
	for i := range 10 {
		target := newRemote(t, 500+i, owner, fmt.Sprintf("Wrap race target %d", i))
		mustCreateRemote(t, s, target)
		var writeErr, deleteErr error
		var wg sync.WaitGroup
		if i%2 == 0 {
			w := newWrapper(t, 600+i, owner, fmt.Sprintf("Wrap race wrapper %d", i), target.ID, "x")
			wg.Go(func() { writeErr = s.CreateRemote(ctx, w) })
			wg.Go(func() { deleteErr = s.DeleteRemote(ctx, target.ID, t0()) })
			wg.Wait()
			if writeErr == nil {
				defer func() {
					if err := s.DeleteRemote(ctx, w.ID, t0()); err != nil {
						t.Error(err)
					}
				}()
			}
		} else {
			cur, err := s.GetRemote(ctx, mover.ID)
			if err != nil {
				t.Fatal(err)
			}
			cur.Wrapped = remotes.Wrapped{RemoteID: target.ID}
			wg.Go(func() { _, writeErr = s.UpdateRemote(ctx, cur) })
			wg.Go(func() { deleteErr = s.DeleteRemote(ctx, target.ID, t0()) })
			wg.Wait()
		}
		switch {
		case writeErr == nil && deleteErr == nil:
			t.Fatalf("round %d: the wrapper was written and its wrapped remote deleted", i)
		case writeErr == nil:
			wantErr(t, "DeleteRemote(wrapped by the new wrapper)", deleteErr, domain.ErrFailedPrecondition)
		case deleteErr == nil:
			wantErr(t, "writing a wrapper of the deleted remote", writeErr, domain.ErrNotFound)
		default:
			t.Fatalf("round %d: both failed: %v; %v", i, writeErr, deleteErr)
		}
		if i%2 == 1 && writeErr == nil {
			// Move the wrapper back, so the target is free again.
			cur, _ := s.GetRemote(ctx, mover.ID)
			cur.Wrapped = remotes.Wrapped{RemoteID: anchor.ID}
			if _, err := s.UpdateRemote(ctx, cur); err != nil {
				t.Fatal(err)
			}
		}
	}
}
