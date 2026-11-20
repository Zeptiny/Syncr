package restic

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
	// storedIsRepository, storedHoldsRepository and storedInRepository are
	// the same for a source on a wrapper remote (crypt) whose data is
	// stored on the repository's remote, by the folder it is stored in.
	storedIsRepository    = "stores its data in the folder of repository %q (at %s): a backup cannot read its own repository"
	storedHoldsRepository = "stores its data in a folder that holds repository %q (at %s): a backup cannot read its own repository; choose a folder beside it"
	storedInRepository    = "stores its data inside the folder of repository %q (at %s): a backup cannot read its own repository"
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
// A source on a wrapper remote (crypt) is compared by the folder its data
// is stored in, on the wrapped remote (as the dispatcher's spec builder
// and the worker do). Callers prefix the field paths (the definition's
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
		if msg := sourceOverlap(path, repo, false); msg != "" {
			return fieldError("path", msg)
		}
	}
	storedOn, storedPath, err := s.remotes.Storage(ctx, owner, remoteID)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return fmt.Errorf("%s: %s: %w", op, remoteID, ErrUnknownSourceRemote)
	case err != nil:
		return fmt.Errorf("%s: %w", op, err)
	}
	if !storedOn.IsZero() && storedOn == repo.RemoteID {
		stored := strings.Join(append(segments(storedPath), segments(path)...), "/")
		if msg := sourceOverlap(stored, repo, true); msg != "" {
			return fieldError("path", msg)
		}
	}
	return nil
}

// sourceOverlap returns the message of a source folder path on the remote
// of repo that is repo's folder, holds it or lies inside it; "" when
// neither holds the other. stored says path is the folder a wrapper remote
// stores the source's data in, not the source itself (the stored*
// messages).
func sourceOverlap(path string, repo Repo, stored bool) string {
	is, holdsIt, inIt := sourceIsRepository, sourceHoldsRepository, sourceInRepository
	if stored {
		is, holdsIt, inIt = storedIsRepository, storedHoldsRepository, storedInRepository
	}
	in, holds := within(path, repo.Path), within(repo.Path, path)
	var format string
	switch {
	case in && holds:
		format = is
	case holds:
		format = holdsIt
	case in:
		format = inIt
	default:
		return ""
	}
	return fmt.Sprintf(format, repo.Name, displayPath(repo.Path))
}
