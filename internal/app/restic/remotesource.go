package restic

import (
	"context"
	"errors"
	"fmt"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// ErrUnknownSourceRemote reports a backup of a remote whose source remote
// is not a live remote of the caller (missing, deleted or another user's).
// It matches domain.ErrNotFound, so another user's remote is
// indistinguishable from a missing one.
var ErrUnknownSourceRemote = fmt.Errorf("remote %w", domain.ErrNotFound)

// Messages of CheckRemoteSource's field errors, for people.
const (
	// SourceLocalMessage is the error on "remote_id" of a source remote
	// of a type on the worker's own disk (Local directory): back up the
	// worker's folder instead.
	SourceLocalMessage = "this remote is a folder on a worker; back up that folder instead"
	// sourceIsRepository, sourceHoldsRepository and sourceInRepository are
	// the errors on "path" of a source on the repository's remote that is
	// its folder, holds it or lies inside it (with the repository's name
	// and folder).
	sourceIsRepository    = "is the folder of repository %q (at %s): a backup cannot read its own repository"
	sourceHoldsRepository = "holds repository %q (at %s): a backup cannot read its own repository; choose a folder beside it"
	sourceInRepository    = "is inside the folder of repository %q (at %s): a backup cannot read its own repository"
)

// CheckRemoteSource checks the folder that a backup of a remote reads, a
// path (relative to the remote's root, "" for the root) on remoteID,
// against owner's repository repoID that it backs up into.
//
// Errors: ErrUnknownRepository when repoID is not a live repository of
// owner, ErrUnknownSourceRemote when remoteID is not a live remote of owner
// (both match domain.ErrNotFound); a *domain.ValidationError on
// "remote_id" (SourceLocalMessage) when the remote is of a type on the
// worker's disk, or on "path" when the remote is the repository's and one
// of the two folders is the other or holds it (which, the message says).
// Callers prefix the field paths (the definition's
// "definition.restic_backup.remote_source"). Overlaps through unions or two
// remotes of the same storage are not detected.
func (s *Service) CheckRemoteSource(ctx context.Context, owner domain.UserID, repoID domain.ResticRepoID, remoteID domain.RemoteID, path string) error {
	const op = "check the remote source"
	repo, err := s.owned(ctx, owner, repoID)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	typeID, err := s.remotes.RemoteType(ctx, owner, remoteID)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return fmt.Errorf("%s: %s: %w", op, remoteID, ErrUnknownSourceRemote)
	case err != nil:
		return fmt.Errorf("%s: %w", op, err)
	}
	if typ, ok := s.backends.Get(typeID); ok && typ.Capabilities().Local {
		return fieldError("remote_id", SourceLocalMessage)
	}
	if repo.RemoteID == remoteID {
		if msg := sourceOverlap(path, repo); msg != "" {
			return fieldError("path", msg)
		}
	}
	return nil
}

// sourceOverlap returns the message of a source folder path on the remote
// of repo that is repo's folder, holds it or lies inside it; "" when
// neither holds the other.
func sourceOverlap(path string, repo Repo) string {
	in, holds := within(path, repo.Path), within(repo.Path, path)
	switch {
	case in && holds:
		return fmt.Sprintf(sourceIsRepository, repo.Name, displayPath(repo.Path))
	case holds:
		return fmt.Sprintf(sourceHoldsRepository, repo.Name, displayPath(repo.Path))
	case in:
		return fmt.Sprintf(sourceInRepository, repo.Name, displayPath(repo.Path))
	}
	return ""
}
