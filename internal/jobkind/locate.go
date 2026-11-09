package jobkind

import (
	"fmt"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Locate returns the remote definitions and the Location of path on remote
// id, built as Build builds them (see the package documentation): the
// remote gets the alias "r1" and the members of a union the aliases after
// it; on a local remote the Location is a local path (backend.JoinLocal of
// its root) and no definition is needed. path is relative to the remote's
// root ("" is the root).
//
// remotes must hold id and, for a union, its members. Errors wrap
// domain.ErrNotFound (a remote is missing from remotes) or
// domain.ErrInvalidArgument (a path that is invalid on the remote, as a
// *backend.PathError, or a remote of an unusable shape). Messages never
// contain option or secret values.
func Locate(id domain.RemoteID, path string, remotes map[domain.RemoteID]Remote) ([]*syncrv1.RemoteDefinition, *syncrv1.Location, error) {
	if id.IsZero() {
		return nil, nil, fmt.Errorf("locate: %w: no remote", domain.ErrInvalidArgument)
	}
	rs, err := newSpecRemotes([]domain.RemoteID{id}, remotes)
	if err != nil {
		return nil, nil, fmt.Errorf("locate: %w", err)
	}
	loc, err := rs.on(rs.remotes[id], path)
	if err != nil {
		return nil, nil, fmt.Errorf("locate: %w", err)
	}
	defs, err := rs.definitions()
	if err != nil {
		return nil, nil, fmt.Errorf("locate: %w", err)
	}
	return defs, loc, nil
}

// LocateRepository returns the remote definitions and the
// ResticRepository (location and password) of repo, which lives at
// repo.Path on remote repo.RemoteID (see Locate). The result holds the
// password: it belongs in a spec only and must never be logged.
func LocateRepository(repo Repository, remotes map[domain.RemoteID]Remote) ([]*syncrv1.RemoteDefinition, *syncrv1.ResticRepository, error) {
	defs, loc, err := Locate(repo.RemoteID, repo.Path, remotes)
	if err != nil {
		return nil, nil, fmt.Errorf("repository %s: %w", repo.ID, err)
	}
	return defs, &syncrv1.ResticRepository{Location: loc, Password: repo.Password}, nil
}
