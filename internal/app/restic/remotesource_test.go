package restic_test

import (
	"context"
	"errors"
	"testing"

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
