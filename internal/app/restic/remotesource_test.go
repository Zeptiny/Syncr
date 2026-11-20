package restic_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/domain"
)

func TestCheckRemoteSource(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	repo := e.create() // "Photos vault" at restic/photos on e.s3
	other := e.storedRepo("Elsewhere", "other")

	ok := []struct {
		name   string
		remote domain.RemoteID
		path   string
	}{
		{"beside the repository", e.s3, "photos"},
		{"a sibling sharing a prefix", e.s3, "restic/photos2"},
		{"the same path on another remote", e.union, "restic/photos"},
	}
	for _, tt := range ok {
		if err := e.svc.CheckRemoteSource(ctx, e.alice.UserID, repo.ID, tt.remote, tt.path); err != nil {
			t.Errorf("%s: CheckRemoteSource = %v", tt.name, err)
		}
	}

	for _, tt := range []struct {
		name, path, field, message string
		remote                     domain.RemoteID
	}{
		{"the repository's folder", "/restic//photos/", "path",
			`is the folder of repository "Photos vault" (at "restic/photos"): a backup cannot read its own repository`, e.s3},
		{"a folder holding it", "restic", "path",
			`holds repository "Photos vault" (at "restic/photos"): a backup cannot read its own repository; choose a folder beside it`, e.s3},
		{"the remote's root", "", "path",
			`holds repository "Photos vault" (at "restic/photos"): a backup cannot read its own repository; choose a folder beside it`, e.s3},
		{"a folder inside it", "restic/photos/data", "path",
			`is inside the folder of repository "Photos vault" (at "restic/photos"): a backup cannot read its own repository`, e.s3},
		{"a local remote", "x", "remote_id", restic.SourceLocalMessage, e.local},
	} {
		err := e.svc.CheckRemoteSource(ctx, e.alice.UserID, repo.ID, tt.remote, tt.path)
		var verr *domain.ValidationError
		if !errors.As(err, &verr) || len(verr.Fields) != 1 || verr.Fields[0].Field != tt.field || verr.Fields[0].Message != tt.message {
			t.Errorf("%s: CheckRemoteSource = %v, want %s: %s", tt.name, err, tt.field, tt.message)
		}
	}

	// Another user's remote or repository is not found, like a missing one.
	if err := e.svc.CheckRemoteSource(ctx, e.alice.UserID, repo.ID, e.bobS3, ""); !errors.Is(err, restic.ErrUnknownSourceRemote) || !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("bob's remote: %v", err)
	}
	if err := e.svc.CheckRemoteSource(ctx, e.alice.UserID, repo.ID, domain.NewRemoteID(), ""); !errors.Is(err, restic.ErrUnknownSourceRemote) {
		t.Errorf("a missing remote: %v", err)
	}
	if err := e.svc.CheckRemoteSource(ctx, e.bob.UserID, repo.ID, e.bobS3, ""); !errors.Is(err, restic.ErrUnknownRepository) {
		t.Errorf("alice's repository for bob: %v", err)
	}
	if err := e.svc.CheckRemoteSource(ctx, e.admin.UserID, other.ID, e.s3, ""); !errors.Is(err, restic.ErrUnknownRepository) {
		t.Errorf("an admin using a repository of alice: %v", err)
	}
}

// TestCheckRemoteSourceThroughWrapper: a source on an encrypted remote is
// compared by the folder its data is stored in. One stored in, around or
// inside the repository's folder is refused (as the dispatcher and the
// worker refuse it), with a message that says it is stored there.
func TestCheckRemoteSourceThroughWrapper(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	repo := e.create() // "Photos vault" at restic/photos on e.s3
	crypt := func(name string, on domain.RemoteID, path string) domain.RemoteID {
		t.Helper()
		id := e.ids.NewRemoteID()
		r := remotes.Remote{ID: id, OwnerID: e.alice.UserID, Name: name, Type: "crypt", SecretsEnc: []byte("enc"),
			Wrapped: remotes.Wrapped{RemoteID: on, Path: path}, CreatedAt: t0, UpdatedAt: t0, Version: 1}
		if err := e.store.CreateRemote(ctx, r); err != nil {
			t.Fatal(err)
		}
		return id
	}
	inRestic := crypt("Vault", e.s3, "/restic/")
	beside := crypt("Beside", e.s3, "vault")
	other := e.ids.NewRemoteID()
	if err := e.store.CreateRemote(ctx, remotes.Remote{ID: other, OwnerID: e.alice.UserID, Name: "Other bucket", Type: "s3",
		SecretsEnc: []byte("enc"), CreatedAt: t0, UpdatedAt: t0, Version: 1}); err != nil {
		t.Fatal(err)
	}
	elsewhere := crypt("Elsewhere", other, "restic/photos")

	for _, tt := range []struct {
		name   string
		remote domain.RemoteID
		path   string
	}{
		{"stored beside the repository", inRestic, "music"},
		{"stored in a sibling sharing a prefix", inRestic, "photos2"},
		{"stored in another folder of its remote", beside, ""},
		{"stored at the same path on another remote", elsewhere, ""},
	} {
		if err := e.svc.CheckRemoteSource(ctx, e.alice.UserID, repo.ID, tt.remote, tt.path); err != nil {
			t.Errorf("%s: CheckRemoteSource = %v", tt.name, err)
		}
	}
	for _, tt := range []struct {
		name, path, message string
	}{
		{"stored in the repository's folder", "photos/",
			`stores its data in the folder of repository "Photos vault" (at "restic/photos"): a backup cannot read its own repository`},
		{"stored in a folder holding it", "",
			`stores its data in a folder that holds repository "Photos vault" (at "restic/photos"): a backup cannot read its own repository; choose a folder beside it`},
		{"stored inside it", "photos/2026/june",
			`stores its data inside the folder of repository "Photos vault" (at "restic/photos"): a backup cannot read its own repository`},
	} {
		err := e.svc.CheckRemoteSource(ctx, e.alice.UserID, repo.ID, inRestic, tt.path)
		var verr *domain.ValidationError
		if !errors.As(err, &verr) || len(verr.Fields) != 1 || verr.Fields[0].Field != "path" || verr.Fields[0].Message != tt.message {
			t.Errorf("%s: CheckRemoteSource = %v, want path: %s", tt.name, err, tt.message)
		}
	}
}
