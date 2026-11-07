package remotes

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// MaxNameLen is the longest remote name, in characters.
const MaxNameLen = 64

// Config tunes the service. Zero fields take the DefaultConfig values.
type Config struct {
	// MaxMembers caps the members of a union.
	MaxMembers int
}

// DefaultConfig returns the default configuration.
func DefaultConfig() Config {
	return Config{MaxMembers: 16}
}

// Deps are the service's collaborators. All are required except Logger.
type Deps struct {
	Remotes Repository
	Cipher  Cipher
	Clock   Clock
	IDs     IDGenerator
	// Backends are the backend types remotes can have.
	Backends *backend.Registry
	// Logger defaults to discarding. The service never logs an Input or a
	// configuration value.
	Logger *slog.Logger
}

// Service implements the remote use cases (see the package documentation).
// It is stateless apart from its dependencies and safe for concurrent use.
type Service struct {
	repo     Repository
	cipher   Cipher
	clock    Clock
	ids      IDGenerator
	backends *backend.Registry
	log      *slog.Logger
	cfg      Config
}

// New returns a Service.
func New(d Deps, cfg Config) (*Service, error) {
	if d.Remotes == nil || d.Cipher == nil || d.Clock == nil || d.IDs == nil || d.Backends == nil {
		return nil, errors.New("remotes: missing dependency")
	}
	cfg.MaxMembers = cmp.Or(cfg.MaxMembers, DefaultConfig().MaxMembers)
	if cfg.MaxMembers < 2 {
		return nil, fmt.Errorf("remotes: invalid config %+v: a union needs at least two members", cfg)
	}
	log := d.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{
		repo: d.Remotes, cipher: d.Cipher, clock: d.Clock, ids: d.IDs,
		backends: d.Backends, log: log, cfg: cfg,
	}, nil
}

// Create validates and stores a new remote of actor. Empty option and
// secret values count as unset.
func (s *Service) Create(ctx context.Context, actor domain.Actor, in Input) (Remote, error) {
	if actor.UserID.IsZero() {
		return Remote{}, fmt.Errorf("create remote: %w: remotes belong to a user account", domain.ErrPermissionDenied)
	}
	var v domain.Validation
	name := checkName(&v, in.Name)
	typ, ok := s.backends.Get(in.Type)
	switch {
	case in.Type == "":
		v.Add("type", "is required")
	case !ok:
		v.Add("type", "is not a known backend type")
	}
	for i := range in.ClearSecrets {
		v.Add(fmt.Sprintf("clear_secrets[%d]", i), "is only used when updating a remote")
	}
	if !ok {
		return Remote{}, fmt.Errorf("create remote: %w", v.Err())
	}

	id := s.ids.NewRemoteID()
	cfg := backend.Config{Options: nonEmpty(in.Options), Secrets: nonEmpty(in.Secrets)}
	addConfigErrors(&v, typ.Validate(cfg))
	members, err := s.checkMembers(ctx, &v, actor.UserID, id, typ, in.Members)
	if err != nil {
		return Remote{}, fmt.Errorf("create remote: %w", err)
	}
	if err := v.Err(); err != nil {
		return Remote{}, fmt.Errorf("create remote: %w", err)
	}

	now := s.clock.Now()
	r := Remote{
		ID: id, OwnerID: actor.UserID, Name: name, Type: typ.ID(), Options: cfg.Options, Members: members,
		CreatedAt: now, UpdatedAt: now, Version: 1,
	}
	if r.SecretsEnc, r.SecretKeys, err = s.seal(id, cfg.Secrets); err != nil {
		return Remote{}, fmt.Errorf("create remote: %w", err)
	}
	if err := s.repo.CreateRemote(ctx, r); err != nil {
		return Remote{}, fmt.Errorf("create remote: %w", err)
	}
	s.log.InfoContext(ctx, "remote created", "remote_id", id.String(), "owner_id", actor.UserID.String(), "backend_type", r.Type)
	return r, nil
}

// Get returns one of actor's remotes: ErrNotFound for a deleted remote or
// another user's, admins included.
func (s *Service) Get(ctx context.Context, actor domain.Actor, id domain.RemoteID) (Remote, error) {
	r, err := s.owned(ctx, actor, id)
	if err != nil {
		return Remote{}, fmt.Errorf("get remote: %w", err)
	}
	return r, nil
}

// owned returns the live remote id of actor, or ErrNotFound.
func (s *Service) owned(ctx context.Context, actor domain.Actor, id domain.RemoteID) (Remote, error) {
	if id.IsZero() {
		return Remote{}, fmt.Errorf("remote: %w", domain.ErrInvalidID)
	}
	r, err := s.repo.GetRemote(ctx, id)
	if err != nil {
		return Remote{}, fmt.Errorf("remote %s: %w", id, err)
	}
	if r.Deleted() || !actor.Owns(r.OwnerID) {
		return Remote{}, fmt.Errorf("remote %s: %w", id, domain.ErrNotFound)
	}
	return r, nil
}

// List returns actor's live remotes matching f, by name; f.OwnerID is
// forced to the actor.
func (s *Service) List(ctx context.Context, actor domain.Actor, f ListFilter) ([]Remote, error) {
	if actor.UserID.IsZero() {
		return []Remote{}, nil
	}
	f.OwnerID = actor.UserID
	rs, err := s.repo.ListRemotes(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("list remotes: %w", err)
	}
	return rs, nil
}

// Update changes one of actor's remotes. version is the one the caller read
// (ErrConflict if the remote changed since). The type cannot change (an
// empty in.Type means the remote's). Options and members are replaced;
// secrets are merged: a non-empty value replaces the stored one, an absent
// or empty one keeps it, and in.ClearSecrets removes optional ones. The
// merged configuration is validated.
func (s *Service) Update(ctx context.Context, actor domain.Actor, id domain.RemoteID, version int64, in Input) (Remote, error) {
	cur, err := s.owned(ctx, actor, id)
	if err != nil {
		return Remote{}, fmt.Errorf("update remote: %w", err)
	}
	if version != cur.Version {
		// Merging secrets onto another version than the caller's would keep
		// or drop secrets the caller never saw.
		return Remote{}, fmt.Errorf("update remote %s: at version %d, not %d: %w", id, cur.Version, version, domain.ErrConflict)
	}
	typ, ok := s.backends.Get(cur.Type)
	if !ok {
		return Remote{}, fmt.Errorf("update remote %s: %w: backend type %q is no longer available", id, domain.ErrFailedPrecondition, cur.Type)
	}
	var v domain.Validation
	name := checkName(&v, in.Name)
	if in.Type != "" && in.Type != cur.Type {
		v.Add("type", "cannot be changed")
	}

	secrets, err := s.open(cur)
	if err != nil {
		return Remote{}, fmt.Errorf("update remote %s: %w", id, err)
	}
	fields := map[string]backend.Field{}
	for _, f := range typ.Fields() {
		fields[f.Name] = f
	}
	for i, key := range in.ClearSecrets {
		path := fmt.Sprintf("clear_secrets[%d]", i)
		switch f, ok := fields[key]; {
		case !ok || !f.Secret:
			v.Add(path, "is not a secret field of this backend")
		case f.Required:
			v.Add(path, "is required and cannot be cleared")
		case in.Secrets[key] != "":
			v.Add(path, "is also set in secrets")
		default:
			delete(secrets, key)
		}
	}
	maps.Copy(secrets, nonEmpty(in.Secrets))

	cfg := backend.Config{Options: nonEmpty(in.Options), Secrets: secrets}
	addConfigErrors(&v, typ.Validate(cfg))
	members, err := s.checkMembers(ctx, &v, actor.UserID, id, typ, in.Members)
	if err != nil {
		return Remote{}, fmt.Errorf("update remote %s: %w", id, err)
	}
	if err := v.Err(); err != nil {
		return Remote{}, fmt.Errorf("update remote %s: %w", id, err)
	}

	next := cur
	next.Name, next.Options, next.Members, next.UpdatedAt = name, cfg.Options, members, s.clock.Now()
	if next.SecretsEnc, next.SecretKeys, err = s.seal(id, secrets); err != nil {
		return Remote{}, fmt.Errorf("update remote %s: %w", id, err)
	}
	stored, err := s.repo.UpdateRemote(ctx, next)
	if err != nil {
		return Remote{}, fmt.Errorf("update remote %s: %w", id, err)
	}
	s.log.InfoContext(ctx, "remote updated", "remote_id", id.String(), "version", stored.Version)
	return stored, nil
}

// Delete soft-deletes one of actor's remotes. ErrFailedPrecondition, naming
// the union, while the remote is a member of a live union.
func (s *Service) Delete(ctx context.Context, actor domain.Actor, id domain.RemoteID) error {
	if _, err := s.owned(ctx, actor, id); err != nil {
		return fmt.Errorf("delete remote: %w", err)
	}
	unions, err := s.repo.ListUnionsUsing(ctx, id)
	if err != nil {
		return fmt.Errorf("delete remote %s: %w", id, err)
	}
	if len(unions) > 0 {
		return fmt.Errorf("%w: the remote is a member of union %q; remove it from the union first", domain.ErrFailedPrecondition, unions[0].Name)
	}
	// The repository repeats the check in its transaction, against a union
	// created meanwhile.
	if err := s.repo.DeleteRemote(ctx, id, s.clock.Now()); err != nil {
		return fmt.Errorf("delete remote %s: %w", id, err)
	}
	s.log.InfoContext(ctx, "remote deleted", "remote_id", id.String())
	return nil
}

// UnionsUsing returns actor's live unions that have remote id as a member,
// by name. ErrNotFound if id is not one of actor's live remotes.
func (s *Service) UnionsUsing(ctx context.Context, actor domain.Actor, id domain.RemoteID) ([]Remote, error) {
	if _, err := s.owned(ctx, actor, id); err != nil {
		return nil, fmt.Errorf("unions using remote: %w", err)
	}
	unions, err := s.repo.ListUnionsUsing(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("unions using remote %s: %w", id, err)
	}
	return slices.DeleteFunc(unions, func(u Remote) bool { return !actor.Owns(u.OwnerID) }), nil
}

// Usable returns the names of ids, which must all be live remotes of owner:
// ErrNotFound otherwise. It implements the jobs service's RemoteChecker.
func (s *Service) Usable(ctx context.Context, owner domain.UserID, ids []domain.RemoteID) (map[domain.RemoteID]string, error) {
	rs, err := s.usable(ctx, owner, ids)
	if err != nil {
		return nil, fmt.Errorf("check remotes: %w", err)
	}
	out := make(map[domain.RemoteID]string, len(rs))
	for id, r := range rs {
		out[id] = r.Name
	}
	return out, nil
}

// usable loads ids, which must all be live remotes of owner.
func (s *Service) usable(ctx context.Context, owner domain.UserID, ids []domain.RemoteID) (map[domain.RemoteID]Remote, error) {
	out := make(map[domain.RemoteID]Remote, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rs, err := s.repo.GetRemotes(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, r := range rs {
		out[r.ID] = r
	}
	for _, id := range ids {
		r, ok := out[id]
		if !ok || r.Deleted() || owner.IsZero() || r.OwnerID != owner {
			return nil, fmt.Errorf("remote %s: %w", id, domain.ErrNotFound)
		}
	}
	return out, nil
}

// maxUnionDepth is how deep Resolve follows union members: unions cannot
// nest, so their members are never unions.
const maxUnionDepth = 1

// Resolve returns ids and the members of the unions among them, decrypted
// and mapped to rclone remotes, for building a job's spec. Every remote must
// be a live remote of owner: ErrNotFound otherwise (also for a remote whose
// backend type is no longer available). A stored configuration that the
// type cannot map is ErrInvalidArgument. The result holds secrets: it must
// never be logged or stored.
func (s *Service) Resolve(ctx context.Context, owner domain.UserID, ids []domain.RemoteID) (map[domain.RemoteID]Resolved, error) {
	out := map[domain.RemoteID]Resolved{}
	visited := map[domain.RemoteID]bool{}
	level := ids
	for depth := 0; len(level) > 0; depth++ {
		if depth > maxUnionDepth {
			return nil, fmt.Errorf("resolve remotes: %w: unions are nested deeper than %d", domain.ErrInvalidArgument, maxUnionDepth)
		}
		var todo []domain.RemoteID
		for _, id := range level {
			if !visited[id] {
				visited[id] = true
				todo = append(todo, id)
			}
		}
		rs, err := s.usable(ctx, owner, todo)
		if err != nil {
			return nil, fmt.Errorf("resolve remotes: %w", err)
		}
		var next []domain.RemoteID
		for _, id := range todo {
			res, err := s.resolve(rs[id])
			if err != nil {
				return nil, fmt.Errorf("resolve remotes: %w", err)
			}
			out[id] = res
			for _, m := range res.Members {
				next = append(next, m.RemoteID)
			}
		}
		level = next
	}
	return out, nil
}

// resolve decrypts and maps one remote.
func (s *Service) resolve(r Remote) (Resolved, error) {
	typ, ok := s.backends.Get(r.Type)
	if !ok {
		return Resolved{}, fmt.Errorf("remote %s: backend type %q is no longer available: %w", r.ID, r.Type, domain.ErrNotFound)
	}
	secrets, err := s.open(r)
	if err != nil {
		return Resolved{}, fmt.Errorf("remote %s: %w", r.ID, err)
	}
	rc, err := typ.ToRclone(backend.Config{Options: maps.Clone(r.Options), Secrets: secrets})
	if err != nil {
		return Resolved{}, fmt.Errorf("remote %s: %w: the stored configuration cannot be mapped: %w", r.ID, domain.ErrInvalidArgument, err)
	}
	return Resolved{ID: r.ID, Name: r.Name, Type: r.Type, Rclone: rc, Members: slices.Clone(r.Members)}, nil
}

// Names returns display references of ids, of any owner and deleted ones
// included; missing ones are omitted. It is for listings of objects that
// refer to remotes (jobs), never for using them.
func (s *Service) Names(ctx context.Context, ids []domain.RemoteID) (map[domain.RemoteID]Ref, error) {
	out := make(map[domain.RemoteID]Ref, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rs, err := s.repo.GetRemotes(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("remote names: %w", err)
	}
	for _, r := range rs {
		out[r.ID] = Ref{ID: r.ID, Name: r.Name, Type: r.Type, Deleted: r.Deleted()}
	}
	return out, nil
}

// checkName validates a remote name and returns it without surrounding
// white space.
func checkName(v *domain.Validation, name string) string {
	name = strings.TrimSpace(name)
	switch n := utf8.RuneCountInString(name); {
	case !utf8.ValidString(name):
		v.Add("name", "is not valid UTF-8")
	case n == 0:
		v.Add("name", "is required")
	case n > MaxNameLen:
		v.Add("name", fmt.Sprintf("must be at most %d characters", MaxNameLen))
	case strings.ContainsFunc(name, unicode.IsControl):
		v.Add("name", "must not contain control characters")
	}
	return name
}

// addConfigErrors adds the field errors of a Type.Validate result to v. Any
// other error is reported as a configuration error without its text, which
// could contain values.
func addConfigErrors(v *domain.Validation, err error) {
	var verr *domain.ValidationError
	switch {
	case err == nil:
	case errors.As(err, &verr):
		v.AddAll("", verr.Fields)
	default:
		v.Add("options", "the configuration is not valid")
	}
}

// checkMembers validates the members of remote self (of type typ, owned by
// owner) and returns them, adding problems to v. Errors other than
// validation problems (the repository) are returned.
func (s *Service) checkMembers(ctx context.Context, v *domain.Validation, owner domain.UserID, self domain.RemoteID, typ backend.Type, members []Member) ([]Member, error) {
	if !typ.Capabilities().Union {
		if len(members) > 0 {
			v.Add("members", "only union remotes have members")
		}
		return nil, nil
	}
	switch {
	case len(members) < 2:
		v.Add("members", "a union needs at least two members")
	case len(members) > s.cfg.MaxMembers:
		v.Add("members", fmt.Sprintf("a union has at most %d members", s.cfg.MaxMembers))
		return nil, nil
	}
	ids := make([]domain.RemoteID, 0, len(members))
	for _, m := range members {
		if !m.RemoteID.IsZero() && m.RemoteID != self {
			ids = append(ids, m.RemoteID)
		}
	}
	found := map[domain.RemoteID]Remote{}
	if len(ids) > 0 {
		rs, err := s.repo.GetRemotes(ctx, ids)
		if err != nil {
			return nil, fmt.Errorf("load members: %w", err)
		}
		for _, r := range rs {
			found[r.ID] = r
		}
	}

	out := make([]Member, len(members))
	type use struct {
		id   domain.RemoteID
		path string
	}
	seen := map[use]bool{}
	for i, m := range members {
		field := func(name string) string { return fmt.Sprintf("members[%d].%s", i, name) }
		if !m.Mode.Valid() {
			v.Add(field("mode"), "is not a known mode")
		}
		r, ok := found[m.RemoteID]
		switch {
		case m.RemoteID.IsZero():
			v.Add(field("remote_id"), "is required")
			continue
		case m.RemoteID == self:
			v.Add(field("remote_id"), "a union cannot contain itself")
			continue
		case !ok || r.Deleted() || r.OwnerID != owner:
			// The same message for missing and foreign remotes: it must not
			// tell another user's remotes apart.
			v.Add(field("remote_id"), "is not one of your remotes")
			continue
		}
		mt, ok := s.backends.Get(r.Type)
		switch {
		case !ok:
			v.Add(field("remote_id"), "has a backend type that is no longer available")
			continue
		case mt.Capabilities().Union:
			v.Add(field("remote_id"), "is a union; unions cannot contain unions")
			continue
		case !mt.Capabilities().UnionMember:
			v.Add(field("remote_id"), "cannot be a union member")
			continue
		}
		if msg := checkMemberPath(mt, r, m); msg != "" {
			v.Add(field("path"), msg)
		}
		u := use{m.RemoteID, m.Path}
		if seen[u] {
			v.Add(field("path"), "the union already has this remote and path")
		}
		seen[u] = true
		out[i] = m
	}
	return out, nil
}

// unionSuffixes are the mode suffixes rclone strips from union upstreams
// (the worker rejects them at the end of a read-write upstream's path).
func unionSuffixes() []string { return []string{":ro", ":nc", ":writeback"} }

// checkMemberPath checks a member's path on remote r (of type typ) the way
// the job builder will join it; it returns a message or "". The member's
// Root comes from its options alone: secrets are never part of a Root.
func checkMemberPath(typ backend.Type, r Remote, m Member) string {
	var root string
	if rc, err := typ.ToRclone(backend.Config{Options: maps.Clone(r.Options)}); err == nil {
		root = rc.Root
	}
	join := backend.JoinRemote
	if typ.Capabilities().Local {
		join = backend.JoinLocal
	}
	joined, err := join(root, m.Path)
	if err != nil {
		return err.Error()
	}
	if m.Mode == backend.UnionReadWrite {
		for _, sfx := range unionSuffixes() {
			if strings.HasSuffix(joined, sfx) {
				return fmt.Sprintf("must not end with %q in read-write mode", sfx)
			}
		}
	}
	return ""
}

// nonEmpty returns the entries of m with a non-empty value, as a new map.
func nonEmpty(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		if v != "" {
			out[k] = v
		}
	}
	return out
}

// seal encrypts secrets as one JSON object bound to remote id; nil and no
// keys when there are none.
func (s *Service) seal(id domain.RemoteID, secrets map[string]string) ([]byte, []string, error) {
	if len(secrets) == 0 {
		return nil, nil, nil
	}
	plain, err := json.Marshal(secrets)
	if err != nil {
		return nil, nil, fmt.Errorf("encode secrets: %w", err)
	}
	enc, err := s.cipher.Encrypt(plain, SecretsAD(id))
	clear(plain)
	if err != nil {
		return nil, nil, fmt.Errorf("encrypt secrets: %w", err)
	}
	return enc, slices.Sorted(maps.Keys(secrets)), nil
}

// open decrypts r's secrets into a new map (empty when it has none).
func (s *Service) open(r Remote) (map[string]string, error) {
	out := map[string]string{}
	if len(r.SecretsEnc) == 0 {
		return out, nil
	}
	plain, err := s.cipher.Decrypt(r.SecretsEnc, SecretsAD(r.ID))
	if err != nil {
		return nil, fmt.Errorf("decrypt secrets: %w", err)
	}
	defer clear(plain)
	if err := json.Unmarshal(plain, &out); err != nil {
		// The JSON error may quote the plaintext.
		return nil, errors.New("decode secrets: not a JSON object of strings")
	}
	return out, nil
}
