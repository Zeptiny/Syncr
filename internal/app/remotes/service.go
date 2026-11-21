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

// FixedMessage is the field error of a change to a fixed field
// (backend.Field.Fixed): another value, a new secret or a clear.
const FixedMessage = "cannot be changed after the remote is created"

// Field errors of a wrapper's wrapped remote (Input.Wrapped).
const (
	// notWrapperMessage: a type that is not a wrapper got a wrapped
	// remote (on "wrapped").
	notWrapperMessage = "only encrypted remotes store their data in another remote"
	// notYoursMessage: a missing, deleted or foreign remote, told apart by
	// nothing (as members are).
	notYoursMessage = "is not one of your remotes"
)

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
	wrapped, err := s.checkWrapped(ctx, &v, actor.UserID, id, typ, in.Wrapped)
	if err != nil {
		return Remote{}, fmt.Errorf("create remote: %w", err)
	}
	if err := v.Err(); err != nil {
		return Remote{}, fmt.Errorf("create remote: %w", err)
	}

	now := s.clock.Now()
	r := Remote{
		ID: id, OwnerID: actor.UserID, Name: name, Type: typ.ID(), Options: cfg.Options, Members: members, Wrapped: wrapped,
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
// empty in.Type means the remote's). Options, members and the wrapped
// remote are replaced; secrets are merged: a non-empty value replaces the
// stored one, an absent or empty one keeps it, and in.ClearSecrets removes
// optional ones. Fixed fields keep their values (checkFixedOptions,
// mergeSecrets). The merged configuration is validated.
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

	secrets, err := s.mergeSecrets(&v, typ, cur, in)
	if err != nil {
		return Remote{}, fmt.Errorf("update remote %s: %w", id, err)
	}

	cfg := backend.Config{Options: nonEmpty(in.Options), Secrets: secrets}
	addConfigErrors(&v, typ.Validate(cfg))
	checkFixedOptions(&v, typ, cur.Options, cfg.Options)
	members, err := s.checkMembers(ctx, &v, actor.UserID, id, typ, in.Members)
	if err != nil {
		return Remote{}, fmt.Errorf("update remote %s: %w", id, err)
	}
	wrapped, err := s.checkWrapped(ctx, &v, actor.UserID, id, typ, in.Wrapped)
	if err != nil {
		return Remote{}, fmt.Errorf("update remote %s: %w", id, err)
	}
	if err := v.Err(); err != nil {
		return Remote{}, fmt.Errorf("update remote %s: %w", id, err)
	}

	next := cur
	next.Name, next.Options, next.Members, next.Wrapped, next.UpdatedAt = name, cfg.Options, members, wrapped, s.clock.Now()
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

// Delete soft-deletes one of actor's remotes. ErrFailedPrecondition while
// a live remote uses it (InUseMessage: naming the first of them by name,
// an encrypted remote that stores its data in it or a union of which it is
// a member), or naming the repository while a live restic repository is on
// it (the repository checks both in its transaction).
func (s *Service) Delete(ctx context.Context, actor domain.Actor, id domain.RemoteID) error {
	if _, err := s.owned(ctx, actor, id); err != nil {
		return fmt.Errorf("delete remote: %w", err)
	}
	using, err := s.repo.ListRemotesUsing(ctx, id)
	if err != nil {
		return fmt.Errorf("delete remote %s: %w", id, err)
	}
	if len(using) > 0 {
		return fmt.Errorf("%w: %s", domain.ErrFailedPrecondition, InUseMessage(id, using[0]))
	}
	// The repository repeats the check in its transaction, against a union
	// or a wrapper created meanwhile.
	if err := s.repo.DeleteRemote(ctx, id, s.clock.Now()); err != nil {
		return fmt.Errorf("delete remote %s: %w", id, err)
	}
	s.log.InfoContext(ctx, "remote deleted", "remote_id", id.String())
	return nil
}

// InUseMessage says why remote id cannot be deleted while user, a live
// remote of ListRemotesUsing, uses it: "the remote stores the data of
// encrypted remote %q; delete that remote first" for a wrapper, else "the
// remote is a member of union %q; remove it from the union first". Stores
// use it too, for the check they repeat.
func InUseMessage(id domain.RemoteID, user Remote) string {
	if user.Wrapped.RemoteID == id {
		return fmt.Sprintf("the remote stores the data of encrypted remote %q; delete that remote first", user.Name)
	}
	return fmt.Sprintf("the remote is a member of union %q; remove it from the union first", user.Name)
}

// RemotesUsing returns actor's live remotes that use remote id, by name:
// the unions that have it as a member and the wrappers that store their
// data in it (Remote.Wrapped). ErrNotFound if id is not one of actor's
// live remotes.
func (s *Service) RemotesUsing(ctx context.Context, actor domain.Actor, id domain.RemoteID) ([]Remote, error) {
	if _, err := s.owned(ctx, actor, id); err != nil {
		return nil, fmt.Errorf("remotes using remote: %w", err)
	}
	using, err := s.repo.ListRemotesUsing(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("remotes using remote %s: %w", id, err)
	}
	return slices.DeleteFunc(using, func(u Remote) bool { return !actor.Owns(u.OwnerID) }), nil
}

// Infos returns what the checks of a job definition need to know about
// ids (Info: the type, the wrapped remote, the type's capabilities), which
// must all be live remotes of owner: ErrNotFound otherwise. A remote whose
// type is no longer available has no capabilities. A union keeps no
// checksums when one of its members does not (Capabilities.NoChecksum,
// see unionNoChecksum). The API and the scheduler pass them to jobkind's
// Registry.CheckWith.
func (s *Service) Infos(ctx context.Context, owner domain.UserID, ids []domain.RemoteID) (map[domain.RemoteID]Info, error) {
	rs, err := s.usable(ctx, owner, ids)
	if err != nil {
		return nil, fmt.Errorf("remote infos: %w", err)
	}
	var memberIDs []domain.RemoteID
	for _, r := range rs {
		for _, m := range r.Members {
			if !slices.Contains(memberIDs, m.RemoteID) {
				memberIDs = append(memberIDs, m.RemoteID)
			}
		}
	}
	// The members' types only: a union's members are live remotes of its
	// owner (the store refuses deleting a member), and they are not part of
	// the result.
	memberTypes := map[domain.RemoteID]string{}
	if len(memberIDs) > 0 {
		ms, err := s.repo.GetRemotes(ctx, memberIDs)
		if err != nil {
			return nil, fmt.Errorf("remote infos: %w", err)
		}
		for _, m := range ms {
			memberTypes[m.ID] = m.Type
		}
	}
	out := make(map[domain.RemoteID]Info, len(rs))
	for id, r := range rs {
		info := Info{Type: r.Type, Wrapped: r.Wrapped}
		if typ, ok := s.backends.Get(r.Type); ok {
			info.Capabilities = typ.Capabilities()
		}
		info.Capabilities.NoChecksum = info.Capabilities.NoChecksum || slices.ContainsFunc(r.Members, func(m Member) bool {
			mt, ok := s.backends.Get(memberTypes[m.RemoteID])
			return ok && mt.Capabilities().NoChecksum
		})
		out[id] = info
	}
	return out, nil
}

// unionNoChecksum sets Capabilities.NoChecksum on the unions of rs that
// have a member in rs that keeps no checksums: rclone's union then keeps
// none either (it hashes only with what every upstream supports), so a
// checksum comparison through it would fall back to sizes. Infos applies
// the same rule.
func unionNoChecksum(rs map[domain.RemoteID]Resolved) {
	for id, r := range rs {
		if !r.Capabilities.NoChecksum && slices.ContainsFunc(r.Members, func(m Member) bool { return rs[m.RemoteID].Capabilities.NoChecksum }) {
			r.Capabilities.NoChecksum = true
			rs[id] = r
		}
	}
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

// RemoteType returns the backend type ID of id, which must be a live remote
// of owner: ErrNotFound otherwise (missing, deleted or another user's). It
// implements the restic service's RemoteChecker.
func (s *Service) RemoteType(ctx context.Context, owner domain.UserID, id domain.RemoteID) (string, error) {
	if id.IsZero() {
		return "", fmt.Errorf("remote type: %w", domain.ErrInvalidID)
	}
	rs, err := s.usable(ctx, owner, []domain.RemoteID{id})
	if err != nil {
		return "", fmt.Errorf("remote type: %w", err)
	}
	return rs[id].Type, nil
}

// Storage returns where id, a live remote of owner, stores its data when
// its type is a wrapper (crypt): the wrapped remote and the folder on it,
// relative to that remote's root. The ID is zero for a remote of any other
// type. Errors as RemoteType. It implements the restic service's
// RemoteChecker.
func (s *Service) Storage(ctx context.Context, owner domain.UserID, id domain.RemoteID) (domain.RemoteID, string, error) {
	if id.IsZero() {
		return domain.RemoteID{}, "", fmt.Errorf("remote storage: %w", domain.ErrInvalidID)
	}
	rs, err := s.usable(ctx, owner, []domain.RemoteID{id})
	if err != nil {
		return domain.RemoteID{}, "", fmt.Errorf("remote storage: %w", err)
	}
	w := rs[id].Wrapped
	return w.RemoteID, w.Path, nil
}

// mergeSecrets returns the secrets of cur (a stored remote of type typ)
// with in's merged over them, as Update stores them: a non-empty value
// replaces the stored one, an absent or empty one keeps it, and
// in.ClearSecrets removes optional ones (problems are added to v). A fixed
// secret field (backend.Field.Fixed) takes neither a new value nor a clear
// (FixedMessage).
func (s *Service) mergeSecrets(v *domain.Validation, typ backend.Type, cur Remote, in Input) (map[string]string, error) {
	secrets, err := s.open(cur)
	if err != nil {
		return nil, err
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
		case f.Fixed:
			v.Add(path, FixedMessage)
		case f.Required:
			v.Add(path, "is required and cannot be cleared")
		case in.Secrets[key] != "":
			v.Add(path, "is also set in secrets")
		default:
			delete(secrets, key)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(nonEmpty(in.Secrets))) {
		if f := fields[key]; f.Secret && f.Fixed {
			v.Add("secrets."+key, FixedMessage)
		}
	}
	maps.Copy(secrets, nonEmpty(in.Secrets))
	return secrets, nil
}

// checkFixedOptions adds FixedMessage on "options.<name>" to v for every
// fixed option field (backend.Field.Fixed) of typ whose effective value
// (its value, or its default when empty) in next differs from that in
// stored.
func checkFixedOptions(v *domain.Validation, typ backend.Type, stored, next map[string]string) {
	for _, f := range typ.Fields() {
		if !f.Fixed || f.Secret {
			continue
		}
		effective := func(m map[string]string) string { return cmp.Or(m[f.Name], f.Default) }
		if effective(stored) != effective(next) {
			v.Add("options."+f.Name, FixedMessage)
		}
	}
}

// Draft resolves a remote as a form describes it, without storing
// anything, for "Test remote" on unsaved values. With a zero id, in is a
// new remote: it is validated as Create validates it and gets a fresh ID
// that is never stored. Otherwise id is one of actor's live remotes
// (ErrNotFound otherwise) and in its edited values, validated as Update
// validates them: the type cannot change, options, members and the
// wrapped remote are replaced, fixed fields keep their values, and in's
// secrets merge over the stored ones. The name is not checked in either
// case: a remote can be tested before it is named.
//
// The result is what Resolve returns for the draft and, for a union, its
// members or, for a wrapper, its wrapped remote (which must be stored
// remotes of actor), together with the draft's ID. Errors are those of Create and Update (a
// *domain.ValidationError with their field paths) and of Resolve. The
// result holds secrets: never log or store it.
func (s *Service) Draft(ctx context.Context, actor domain.Actor, id domain.RemoteID, in Input) (map[domain.RemoteID]Resolved, domain.RemoteID, error) {
	const op = "draft remote"
	if actor.UserID.IsZero() {
		return nil, domain.RemoteID{}, fmt.Errorf("%s: %w: remotes belong to a user account", op, domain.ErrPermissionDenied)
	}
	var (
		v       domain.Validation
		typ     backend.Type
		secrets map[string]string
		stored  map[string]string // the stored options of an existing remote
	)
	if id.IsZero() {
		var ok bool
		typ, ok = s.backends.Get(in.Type)
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
			return nil, domain.RemoteID{}, fmt.Errorf("%s: %w", op, v.Err())
		}
		id = s.ids.NewRemoteID()
		secrets = nonEmpty(in.Secrets)
	} else {
		cur, err := s.owned(ctx, actor, id)
		if err != nil {
			return nil, domain.RemoteID{}, fmt.Errorf("%s: %w", op, err)
		}
		var ok bool
		if typ, ok = s.backends.Get(cur.Type); !ok {
			return nil, domain.RemoteID{}, fmt.Errorf("%s %s: %w: backend type %q is no longer available", op, id, domain.ErrFailedPrecondition, cur.Type)
		}
		if in.Type != "" && in.Type != cur.Type {
			v.Add("type", "cannot be changed")
		}
		if secrets, err = s.mergeSecrets(&v, typ, cur, in); err != nil {
			return nil, domain.RemoteID{}, fmt.Errorf("%s %s: %w: its credentials cannot be read: %w", op, id, domain.ErrFailedPrecondition, err)
		}
		stored = cur.Options
	}
	cfg := backend.Config{Options: nonEmpty(in.Options), Secrets: secrets}
	addConfigErrors(&v, typ.Validate(cfg))
	if stored != nil {
		checkFixedOptions(&v, typ, stored, cfg.Options)
	}
	members, err := s.checkMembers(ctx, &v, actor.UserID, id, typ, in.Members)
	if err != nil {
		return nil, domain.RemoteID{}, fmt.Errorf("%s: %w", op, err)
	}
	wrapped, err := s.checkWrapped(ctx, &v, actor.UserID, id, typ, in.Wrapped)
	if err != nil {
		return nil, domain.RemoteID{}, fmt.Errorf("%s: %w", op, err)
	}
	if err := v.Err(); err != nil {
		return nil, domain.RemoteID{}, fmt.Errorf("%s: %w", op, err)
	}
	rc, err := typ.ToRclone(cfg)
	if err != nil {
		return nil, domain.RemoteID{}, fmt.Errorf("%s: %w: the configuration cannot be mapped: %w", op, domain.ErrInvalidArgument, err)
	}
	used := make([]domain.RemoteID, 0, len(members)+1)
	for _, m := range members {
		used = append(used, m.RemoteID)
	}
	if !wrapped.IsZero() {
		used = append(used, wrapped.RemoteID)
	}
	// The members and the wrapped remote are stored remotes: Resolve checks
	// that they are the actor's and decrypts them. They are never unions or
	// wrappers (checkMembers, checkWrapped) and never the draft itself, so
	// the draft's own ID cannot be among the result.
	out, err := s.Resolve(ctx, actor.UserID, used)
	if err != nil {
		return nil, domain.RemoteID{}, fmt.Errorf("%s: %w", op, err)
	}
	out[id] = Resolved{
		ID: id, Name: strings.TrimSpace(in.Name), Type: typ.ID(), Rclone: rc, Members: slices.Clone(members), Wrapped: wrapped,
		Capabilities: typ.Capabilities(),
	}
	unionNoChecksum(out)
	return out, id, nil
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

// maxResolveDepth is how deep Resolve follows union members and wrapped
// remotes: nothing nests, so they are never unions or wrappers.
const maxResolveDepth = 1

// Resolve returns ids, the members of the unions among them and the
// remotes the wrappers among them store their data in, decrypted and
// mapped to rclone remotes, for building a job's spec. Every remote must
// be a live remote of owner: ErrNotFound otherwise (also for a remote whose
// backend type is no longer available). A stored configuration that the
// type cannot map is ErrInvalidArgument. Stored credentials that cannot be
// decrypted or decoded (a lost or rotated-out secrets key, corrupt data)
// are ErrFailedPrecondition: retrying cannot help, so the dispatcher fails
// the job at once. Other errors (the store) are worth a retry. The result
// holds secrets: it must never be logged or stored.
func (s *Service) Resolve(ctx context.Context, owner domain.UserID, ids []domain.RemoteID) (map[domain.RemoteID]Resolved, error) {
	out := map[domain.RemoteID]Resolved{}
	visited := map[domain.RemoteID]bool{}
	level := ids
	for depth := 0; len(level) > 0; depth++ {
		if depth > maxResolveDepth {
			return nil, fmt.Errorf("resolve remotes: %w: remotes are nested deeper than %d", domain.ErrInvalidArgument, maxResolveDepth)
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
			if !res.Wrapped.IsZero() {
				next = append(next, res.Wrapped.RemoteID)
			}
		}
		level = next
	}
	unionNoChecksum(out)
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
		// The cause names no secret (see open).
		return Resolved{}, fmt.Errorf("remote %s: %w: its credentials cannot be read: %w", r.ID, domain.ErrFailedPrecondition, err)
	}
	rc, err := typ.ToRclone(backend.Config{Options: maps.Clone(r.Options), Secrets: secrets})
	if err != nil {
		return Resolved{}, fmt.Errorf("remote %s: %w: the stored configuration cannot be mapped: %w", r.ID, domain.ErrInvalidArgument, err)
	}
	return Resolved{
		ID: r.ID, Name: r.Name, Type: r.Type, Rclone: rc, Members: slices.Clone(r.Members), Wrapped: r.Wrapped,
		Capabilities: typ.Capabilities(),
	}, nil
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
		out[r.ID] = Ref{ID: r.ID, OwnerID: r.OwnerID, Name: r.Name, Type: r.Type, Deleted: r.Deleted()}
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

// checkWrapped validates the wrapped remote w of remote self (of type typ,
// owned by owner) and returns it, adding problems to v on "wrapped",
// "wrapped.remote_id" and "wrapped.path". A type that is not a wrapper
// takes none; a wrapper needs one: a live remote of owner other than
// self, of a known type that is not local, a union or a wrapper, and a
// path that joins with its root (backend.JoinRemote). Errors other than
// validation problems (the repository) are returned.
func (s *Service) checkWrapped(ctx context.Context, v *domain.Validation, owner domain.UserID, self domain.RemoteID, typ backend.Type, w Wrapped) (Wrapped, error) {
	if !typ.Capabilities().Wrapper {
		if !w.IsZero() {
			v.Add("wrapped", notWrapperMessage)
		}
		return Wrapped{}, nil
	}
	switch {
	case w.RemoteID.IsZero():
		v.Add("wrapped.remote_id", "is required")
		return Wrapped{}, nil
	case w.RemoteID == self:
		v.Add("wrapped.remote_id", "an encrypted remote cannot store its data in itself")
		return Wrapped{}, nil
	}
	r, err := s.repo.GetRemote(ctx, w.RemoteID)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		v.Add("wrapped.remote_id", notYoursMessage)
		return Wrapped{}, nil
	case err != nil:
		return Wrapped{}, fmt.Errorf("load the wrapped remote: %w", err)
	case r.Deleted() || r.OwnerID != owner:
		// The same message as a missing remote: it must not tell another
		// user's remotes apart.
		v.Add("wrapped.remote_id", notYoursMessage)
		return Wrapped{}, nil
	}
	wt, ok := s.backends.Get(r.Type)
	if !ok {
		v.Add("wrapped.remote_id", "has a backend type that is no longer available")
		return Wrapped{}, nil
	}
	switch caps := wt.Capabilities(); {
	case caps.Local:
		v.Add("wrapped.remote_id", "cannot be a folder of a worker")
		return Wrapped{}, nil
	case caps.Union || caps.Wrapper:
		v.Add("wrapped.remote_id", "cannot be a union or another encrypted remote")
		return Wrapped{}, nil
	}
	// The root comes from the options alone: secrets are never part of
	// it.
	var root string
	if rc, err := wt.ToRclone(backend.Config{Options: maps.Clone(r.Options)}); err == nil {
		root = rc.Root
	}
	if _, err := backend.JoinRemote(root, w.Path); err != nil {
		v.Add("wrapped.path", err.Error())
		return Wrapped{}, nil
	}
	return w, nil
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
