package jobkind

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// unionBackend is the rclone backend type of union remotes.
const unionBackend = "union"

// specRemotes assigns the aliases of a spec's remotes and builds their
// RemoteDefinitions and the Locations on them. Kinds use it from Build.
type specRemotes struct {
	remotes map[domain.RemoteID]Remote
	aliases map[domain.RemoteID]string
	order   []domain.RemoteID // aliased remotes, in alias order
}

// newSpecRemotes prepares the remotes of refs, which must all be in
// remotes along with the members of the unions among them. Aliases are
// "r1", "r2", ... in reference order, then the members of the unions (in
// union, then member order); a remote has one alias however often it is
// used. Local remotes get none: their locations are local paths.
func newSpecRemotes(refs []domain.RemoteID, remotes map[domain.RemoteID]Remote) (*specRemotes, error) {
	s := &specRemotes{remotes: remotes, aliases: make(map[domain.RemoteID]string)}
	var unions []Remote
	for _, id := range refs {
		r, err := s.remote(id)
		if err != nil {
			return nil, err
		}
		if err := checkShape(r); err != nil {
			return nil, err
		}
		if isUnion(r) && !slices.ContainsFunc(unions, func(u Remote) bool { return u.ID == r.ID }) {
			unions = append(unions, r)
		}
		s.alias(r)
	}
	for _, u := range unions {
		for i, m := range u.Members {
			r, err := s.remote(m.RemoteID)
			if err != nil {
				return nil, fmt.Errorf("member %d of union remote %s: %w", i, u.ID, err)
			}
			if err := checkShape(r); err != nil {
				return nil, err
			}
			if isUnion(r) {
				return nil, fmt.Errorf("member %d of union remote %s: %w: unions cannot contain unions", i, u.ID, domain.ErrInvalidArgument)
			}
			s.alias(r)
		}
	}
	return s, nil
}

// remote returns the resolved remote id.
func (s *specRemotes) remote(id domain.RemoteID) (Remote, error) {
	r, ok := s.remotes[id]
	if !ok {
		return Remote{}, fmt.Errorf("remote %s: %w: it was not resolved", id, domain.ErrNotFound)
	}
	if r.ID != id {
		return Remote{}, fmt.Errorf("remote %s: %w: resolved as remote %s", id, domain.ErrInvalidArgument, r.ID)
	}
	return r, nil
}

// alias gives r the next alias unless it has one or is local.
func (s *specRemotes) alias(r Remote) {
	if isLocal(r) {
		return
	}
	if _, ok := s.aliases[r.ID]; ok {
		return
	}
	s.order = append(s.order, r.ID)
	s.aliases[r.ID] = "r" + strconv.Itoa(len(s.order))
}

func isLocal(r Remote) bool { return r.Rclone.Backend == "" }
func isUnion(r Remote) bool { return r.Rclone.Backend == unionBackend }

// checkShape rejects remotes the worker would reject for their structure:
// a union without members, members on anything but a union.
func checkShape(r Remote) error {
	switch {
	case isUnion(r) && len(r.Members) == 0:
		return fmt.Errorf("union remote %s: %w: it has no members", r.ID, domain.ErrInvalidArgument)
	case !isUnion(r) && len(r.Members) > 0:
		return fmt.Errorf("remote %s: %w: only unions have members", r.ID, domain.ErrInvalidArgument)
	}
	return nil
}

// definitions returns the RemoteDefinitions of the aliased remotes, in
// alias order. They hold copies of the remotes' options and secrets.
func (s *specRemotes) definitions() ([]*syncrv1.RemoteDefinition, error) {
	out := make([]*syncrv1.RemoteDefinition, 0, len(s.order))
	for _, id := range s.order {
		r := s.remotes[id]
		def := &syncrv1.RemoteDefinition{
			Alias:       s.aliases[id],
			BackendType: r.Rclone.Backend,
			Options:     maps.Clone(r.Rclone.Options),
			Secrets:     maps.Clone(r.Rclone.Secrets),
			ObscureKeys: slices.Clone(r.Rclone.ObscureKeys),
		}
		if isUnion(r) {
			for i, m := range r.Members {
				up, err := s.upstream(m)
				if err != nil {
					return nil, fmt.Errorf("member %d of union remote %s: %w", i, id, err)
				}
				def.UnionUpstreams = append(def.UnionUpstreams, up)
			}
		}
		out = append(out, def)
	}
	return out, nil
}

// upstream builds a union upstream: a path on an aliased member, or a
// local path for a local member.
func (s *specRemotes) upstream(m Member) (*syncrv1.UnionUpstream, error) {
	mode, err := upstreamMode(m.Mode)
	if err != nil {
		return nil, err
	}
	r := s.remotes[m.RemoteID]
	loc, err := s.on(r, m.Path)
	if err != nil {
		return nil, err
	}
	return &syncrv1.UnionUpstream{Location: loc, Mode: mode}, nil
}

// location builds the spec Location of a definition location: a path on
// one of the remotes, or (no remote) an absolute path on the worker. A
// path error is reported on field.
func (s *specRemotes) location(field string, loc *syncrv1.JobLocation) (*syncrv1.Location, error) {
	var (
		out *syncrv1.Location
		err error
	)
	if loc.GetRemoteId() == "" {
		var p string
		p, err = backend.JoinLocal(loc.GetPath(), "")
		out = &syncrv1.Location{Path: p}
	} else {
		var id domain.RemoteID
		if id, err = domain.ParseRemoteID(loc.GetRemoteId()); err != nil {
			return nil, invalid(field+".remote_id", "is not a valid remote ID")
		}
		var r Remote
		if r, err = s.remote(id); err != nil {
			return nil, err
		}
		out, err = s.on(r, loc.GetPath())
	}
	if pe := (*backend.PathError)(nil); errors.As(err, &pe) {
		return nil, invalid(field+".path", pe.Reason)
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// on returns the Location of path on r: below its root, through its alias
// or, for a local remote, as a local path.
func (s *specRemotes) on(r Remote, path string) (*syncrv1.Location, error) {
	if isLocal(r) {
		p, err := backend.JoinLocal(r.Rclone.Root, path)
		if err != nil {
			return nil, fmt.Errorf("path on local remote %s: %w", r.ID, err)
		}
		return &syncrv1.Location{Path: p}, nil
	}
	p, err := backend.JoinRemote(r.Rclone.Root, path)
	if err != nil {
		return nil, fmt.Errorf("path on remote %s: %w", r.ID, err)
	}
	return &syncrv1.Location{Remote: s.aliases[r.ID], Path: p}, nil
}

func upstreamMode(m backend.UnionMode) (syncrv1.UnionUpstreamMode, error) {
	switch m {
	case backend.UnionReadWrite:
		return syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_READ_WRITE, nil
	case backend.UnionReadOnly:
		return syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_READ_ONLY, nil
	case backend.UnionNoCreate:
		return syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_NO_CREATE, nil
	}
	return 0, fmt.Errorf("%w: unknown union member mode %q", domain.ErrInvalidArgument, m)
}
