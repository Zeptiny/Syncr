package restic

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/platform/redact"
)

// Probe error codes the service maps to field errors (link error code
// names, see ProbeError).
const (
	codeWrongPassword      = "wrong_password"
	codeRepositoryNotFound = "repository_not_found"
	codeAlreadyExists      = "already_exists"
	codePathNotAllowed     = "path_not_allowed"
)

// Errors of Create and Update that tell a client what is taken. Each
// matches domain.ErrAlreadyExists.
var (
	// ErrNameTaken: the owner has a live repository with this name.
	ErrNameTaken = fmt.Errorf("a repository with this name %w", domain.ErrAlreadyExists)
	// ErrLocationTaken: a live repository is stored for this remote and
	// path.
	ErrLocationTaken = fmt.Errorf("a repository at this location %w", domain.ErrAlreadyExists)
	// ErrSameRepository: the owner already has this restic repository
	// (the same restic ID), e.g. through another remote.
	ErrSameRepository = fmt.Errorf("this restic repository %w", domain.ErrAlreadyExists)
)

// DefaultFinishInterval is how often Run completes data deletions when
// nothing wakes it.
const DefaultFinishInterval = 30 * time.Second

// Config tunes the service. Zero fields take the defaults.
type Config struct {
	// FinishInterval is the pause between two FinishDeletions passes of
	// Run (default DefaultFinishInterval).
	FinishInterval time.Duration
}

// Deps are the service's collaborators. All are required except Deletions
// and Logger.
type Deps struct {
	Repo    Repository
	Remotes RemoteChecker
	Prober  Prober
	// Backends are the backend types remotes can have (their
	// ResticRepository and Local capabilities).
	Backends *backend.Registry
	// Deletions starts data deletion jobs; nil makes Delete with
	// DeleteData fail with ErrFailedPrecondition ("data deletion is not
	// available").
	Deletions Deletions
	Cipher    Cipher
	Clock     Clock
	IDs       IDGenerator
	// Logger defaults to discarding. The service never logs an Input, an
	// Update or a password.
	Logger *slog.Logger
}

// Service implements the repository use cases (see the package
// documentation). It is safe for concurrent use.
type Service struct {
	repo      Repository
	remotes   RemoteChecker
	prober    Prober
	backends  *backend.Registry
	deletions Deletions
	cipher    Cipher
	clock     Clock
	ids       IDGenerator
	log       *slog.Logger
	cfg       Config
	wake      chan struct{}
}

// New returns a Service.
func New(d Deps, cfg Config) (*Service, error) {
	if d.Repo == nil || d.Remotes == nil || d.Prober == nil || d.Backends == nil || d.Cipher == nil || d.Clock == nil || d.IDs == nil {
		return nil, errors.New("restic: missing dependency")
	}
	cfg.FinishInterval = cmp.Or(cfg.FinishInterval, DefaultFinishInterval)
	if cfg.FinishInterval < 0 {
		return nil, fmt.Errorf("restic: invalid config: finish interval %s", cfg.FinishInterval)
	}
	log := d.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{
		repo: d.Repo, remotes: d.Remotes, prober: d.Prober, backends: d.Backends, deletions: d.Deletions,
		cipher: d.Cipher, clock: d.Clock, ids: d.IDs, log: log, cfg: cfg, wake: make(chan struct{}, 1),
	}, nil
}

// Created is a new repository and the worker that initialized or verified
// it.
type Created struct {
	Repo       Repo
	WorkerID   domain.WorkerID
	WorkerName string
}

// Create validates in, initializes (ModeInit) or verifies (ModeConnect)
// the repository on a worker and stores it for actor.
//
// Errors: a *domain.ValidationError on "name", "description", "remote_id",
// "path", "password", "mode" or "worker_id" (also for what the worker
// found: a wrong password, no repository at the location, a repository
// that already exists, a location outside the worker's folders);
// ErrNameTaken, ErrLocationTaken or ErrSameRepository; and the Prober's
// errors (ErrUnavailable, ErrFailedPrecondition, ErrRateLimited). The
// name and location are checked before the worker is asked, so a doomed
// request never initializes a repository.
func (s *Service) Create(ctx context.Context, actor domain.Actor, in Input) (Created, error) {
	const op = "create repository"
	if actor.UserID.IsZero() {
		return Created{}, fmt.Errorf("%s: %w: repositories belong to a user account", op, domain.ErrPermissionDenied)
	}
	var v domain.Validation
	name := checkName(&v, in.Name)
	description := checkDescription(&v, in.Description)
	path, err := NormalizePath(in.Path)
	addFields(&v, err)
	if !in.Mode.Valid() {
		v.Add("mode", "must be init or connect")
	} else {
		addFields(&v, checkPassword(in.Mode, in.Password))
	}
	local, objectStore := false, false
	if in.RemoteID.IsZero() {
		v.Add("remote_id", "is required")
	} else {
		typeID, err := s.remotes.RemoteType(ctx, actor.UserID, in.RemoteID)
		switch {
		case errors.Is(err, domain.ErrNotFound):
			// The same message for missing and foreign remotes.
			v.Add("remote_id", "is not one of your remotes")
		case err != nil:
			return Created{}, fmt.Errorf("%s: %w", op, err)
		default:
			typ, ok := s.backends.Get(typeID)
			switch {
			case !ok:
				v.Add("remote_id", "has a backend type that is no longer available")
			case !typ.Capabilities().ResticRepository:
				v.Add("remote_id", "this kind of remote cannot hold a restic repository")
			default:
				local, objectStore = typ.Capabilities().Local, typ.Capabilities().ObjectStore
			}
		}
	}
	if local && in.WorkerID.IsZero() {
		v.Add("worker_id", "choose the worker whose disk holds this repository")
	}
	if err := v.Err(); err != nil {
		return Created{}, fmt.Errorf("%s: %w", op, err)
	}

	// The owner's repositories are all the repositories on the owner's
	// remote: remotes are private.
	mine, err := s.repo.ListRepoRows(ctx, ListFilter{OwnerID: actor.UserID})
	if err != nil {
		return Created{}, fmt.Errorf("%s: %w", op, err)
	}
	for _, r := range mine {
		switch {
		case strings.EqualFold(r.Name, name):
			return Created{}, fmt.Errorf("%s: %w", op, ErrNameTaken)
		case r.RemoteID == in.RemoteID && r.Path == path:
			return Created{}, fmt.Errorf("%s: %w", op, ErrLocationTaken)
		}
	}
	if err := checkOverlap(mine, in.RemoteID, path); err != nil {
		return Created{}, fmt.Errorf("%s: %w", op, err)
	}

	res, err := s.prober.Verify(ctx, actor.UserID, Target{RemoteID: in.RemoteID, Path: path, Password: in.Password, WorkerID: in.WorkerID}, in.Mode == ModeInit)
	if err != nil {
		return Created{}, fmt.Errorf("%s: %w", op, verifyError(in.Mode, err, in.Password, objectStore))
	}
	if !hex64Pattern.MatchString(res.ResticID) {
		return Created{}, fmt.Errorf("%s: %w: the worker reported an invalid repository ID", op, domain.ErrUnavailable)
	}
	for _, r := range mine {
		if r.ResticID == res.ResticID {
			return Created{}, fmt.Errorf("%s: %w (as %q)", op, ErrSameRepository, r.Name)
		}
	}

	id := s.ids.NewResticRepoID()
	enc, err := s.cipher.Encrypt([]byte(in.Password), PasswordAD(id))
	if err != nil {
		return Created{}, fmt.Errorf("%s: encrypt the password: %w", op, err)
	}
	now := s.clock.Now()
	r := Repo{
		ID: id, OwnerID: actor.UserID, Name: name, Description: description, RemoteID: in.RemoteID, Path: path,
		PasswordEnc: enc, ResticID: res.ResticID, FormatVersion: max(res.FormatVersion, 0),
		CreatedAt: now, UpdatedAt: now, Version: 1,
	}
	if err := s.repo.CreateRepo(ctx, r); err != nil {
		// A race with another Create (the pre-check passed), or the
		// remote was deleted meanwhile.
		return Created{}, fmt.Errorf("%s: %w", op, err)
	}
	s.log.InfoContext(ctx, "repository created", "repository_id", id.String(), "owner_id", actor.UserID.String(),
		"mode", string(in.Mode), "worker_id", res.WorkerID.String())
	return Created{Repo: r, WorkerID: res.WorkerID, WorkerName: res.WorkerName}, nil
}

// Get returns one of actor's live repositories (deleting ones included)
// with its statistics: ErrNotFound for a deleted repository or another
// user's, admins included.
func (s *Service) Get(ctx context.Context, actor domain.Actor, id domain.ResticRepoID) (Repo, error) {
	r, err := ownedBy(ctx, s.repo.GetRepo, actor.UserID, id)
	if err != nil {
		return Repo{}, fmt.Errorf("get repository: %w", err)
	}
	return r, nil
}

// owned returns the live repository id of owner without its statistics,
// or ErrUnknownRepository.
func (s *Service) owned(ctx context.Context, owner domain.UserID, id domain.ResticRepoID) (Repo, error) {
	return ownedBy(ctx, s.repo.GetRepoRow, owner, id)
}

// ownedBy reads repository id with get and returns it if it is a live
// repository of owner, or ErrUnknownRepository.
func ownedBy(ctx context.Context, get func(context.Context, domain.ResticRepoID) (Repo, error), owner domain.UserID, id domain.ResticRepoID) (Repo, error) {
	if id.IsZero() {
		return Repo{}, fmt.Errorf("repository: %w", domain.ErrInvalidID)
	}
	r, err := get(ctx, id)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return Repo{}, fmt.Errorf("%s: %w", id, ErrUnknownRepository)
	case err != nil:
		return Repo{}, fmt.Errorf("repository %s: %w", id, err)
	case r.Deleted() || owner.IsZero() || r.OwnerID != owner:
		return Repo{}, fmt.Errorf("%s: %w", id, ErrUnknownRepository)
	}
	return r, nil
}

// List returns actor's live repositories matching f, by name, with their
// statistics; f.OwnerID is forced to the actor.
func (s *Service) List(ctx context.Context, actor domain.Actor, f ListFilter) ([]Repo, error) {
	if actor.UserID.IsZero() {
		return []Repo{}, nil
	}
	f.OwnerID = actor.UserID
	rs, err := s.repo.ListRepos(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("list repositories: %w", err)
	}
	return rs, nil
}

// Update renames one of actor's repositories, changes its description or
// replaces its stored password. version is the one the caller read
// (ErrConflict if the repository changed since). A new password is
// verified on a worker first (restic cat config) and must open the same
// repository; a wrong one is a *domain.ValidationError on "password".
// ErrRepositoryDeleting while the repository's data is being deleted.
func (s *Service) Update(ctx context.Context, actor domain.Actor, id domain.ResticRepoID, version int64, u Update) (Repo, error) {
	const op = "update repository"
	cur, err := s.owned(ctx, actor.UserID, id)
	if err != nil {
		return Repo{}, fmt.Errorf("%s: %w", op, err)
	}
	if cur.Deleting {
		return Repo{}, fmt.Errorf("%s %s: %w", op, id, ErrRepositoryDeleting)
	}
	if version != cur.Version {
		return Repo{}, fmt.Errorf("%s %s: at version %d, not %d: %w", op, id, cur.Version, version, domain.ErrConflict)
	}
	var v domain.Validation
	name := checkName(&v, u.Name)
	description := checkDescription(&v, u.Description)
	if u.Password != "" {
		addFields(&v, checkPassword(ModeConnect, u.Password))
	}
	if err := v.Err(); err != nil {
		return Repo{}, fmt.Errorf("%s %s: %w", op, id, err)
	}
	if !strings.EqualFold(name, cur.Name) {
		mine, err := s.repo.ListRepoRows(ctx, ListFilter{OwnerID: actor.UserID})
		if err != nil {
			return Repo{}, fmt.Errorf("%s %s: %w", op, id, err)
		}
		if slices.ContainsFunc(mine, func(r Repo) bool { return r.ID != id && strings.EqualFold(r.Name, name) }) {
			return Repo{}, fmt.Errorf("%s %s: %w", op, id, ErrNameTaken)
		}
	}

	next := cur
	next.Name, next.Description, next.UpdatedAt = name, description, s.clock.Now()
	if u.Password != "" {
		res, err := s.prober.Verify(ctx, actor.UserID, Target{
			RepositoryID: id, RemoteID: cur.RemoteID, Path: cur.Path, Password: u.Password, WorkerID: u.WorkerID,
		}, false)
		if err != nil {
			objectStore := isRepositoryNotFound(err) && s.objectStore(ctx, actor.UserID, cur.RemoteID)
			return Repo{}, fmt.Errorf("%s %s: %w", op, id, verifyError(ModeConnect, err, u.Password, objectStore))
		}
		if res.ResticID != cur.ResticID {
			return Repo{}, fmt.Errorf("%s %s: %w: the location now holds a different restic repository than the one stored", op, id, domain.ErrFailedPrecondition)
		}
		if next.PasswordEnc, err = s.cipher.Encrypt([]byte(u.Password), PasswordAD(id)); err != nil {
			return Repo{}, fmt.Errorf("%s %s: encrypt the password: %w", op, id, err)
		}
	}
	stored, err := s.repo.UpdateRepo(ctx, next)
	if err != nil {
		return Repo{}, fmt.Errorf("%s %s: %w", op, id, err)
	}
	s.log.InfoContext(ctx, "repository updated", "repository_id", id.String(), "version", stored.Version, "password_changed", u.Password != "")
	return stored, nil
}

// Snapshots returns the cached snapshot list of one of actor's
// repositories. It refreshes the cache through a worker first when
// o.Refresh is set or the snapshots were never listed. When the refresh
// fails and the cache was filled before, the cache is returned with
// RefreshError; otherwise the error. While the repository's data is being
// deleted the cache is served, and an explicit refresh fails with
// ErrRepositoryDeleting.
func (s *Service) Snapshots(ctx context.Context, actor domain.Actor, id domain.ResticRepoID, o SnapshotsOptions) (SnapshotList, error) {
	const op = "list snapshots"
	r, err := s.owned(ctx, actor.UserID, id)
	if err != nil {
		return SnapshotList{}, fmt.Errorf("%s: %w", op, err)
	}
	list, err := s.repo.ListSnapshots(ctx, id)
	if err != nil {
		return SnapshotList{}, fmt.Errorf("%s of repository %s: %w", op, id, err)
	}
	switch {
	case r.Deleting && o.Refresh:
		return SnapshotList{}, fmt.Errorf("%s of repository %s: %w", op, id, ErrRepositoryDeleting)
	case r.Deleting, !o.Refresh && !list.RefreshedAt.IsZero():
		list.Stale = stale(list)
		return list, nil
	}

	// list.CurrentMark was read before the worker is asked: a job that ends
	// while it lists makes the next read refresh again.
	res, err := s.refresh(ctx, r, o.WorkerID, list.CurrentMark)
	if err != nil {
		if list.RefreshedAt.IsZero() || ctx.Err() != nil {
			return SnapshotList{}, fmt.Errorf("%s of repository %s: %w", op, id, err)
		}
		list.RefreshError = s.refreshMessage(ctx, id, err)
		list.Stale = stale(list)
		return list, nil
	}
	list, err = s.repo.ListSnapshots(ctx, id)
	if err != nil {
		return SnapshotList{}, fmt.Errorf("%s of repository %s: %w", op, id, err)
	}
	list.WorkerID, list.WorkerName = res.WorkerID, res.WorkerName
	list.Stale = stale(list)
	return list, nil
}

// refresh lists r's snapshots on a worker and replaces the cache with the
// newest MaxSnapshots of them, stored with mark: r's ChangeMark, read
// before this call.
func (s *Service) refresh(ctx context.Context, r Repo, workerID domain.WorkerID, mark ChangeMark) (SnapshotsResult, error) {
	password, err := s.open(r)
	if err != nil {
		return SnapshotsResult{}, err
	}
	// The cache is as old as the moment the worker was asked (shown to
	// people; staleness comes from the mark, not from clocks).
	asked := s.clock.Now()
	res, err := s.prober.Snapshots(ctx, r.OwnerID, Target{RepositoryID: r.ID, RemoteID: r.RemoteID, Path: r.Path, Password: password, WorkerID: workerID})
	if err != nil {
		return SnapshotsResult{}, scrubError(err, password)
	}
	seen := make(map[string]bool, len(res.Snapshots))
	snaps := make([]Snapshot, 0, len(res.Snapshots))
	for _, sn := range res.Snapshots {
		// The store accepts full IDs once; a worker never reports
		// anything else.
		if hex64Pattern.MatchString(sn.ID) && !seen[sn.ID] {
			seen[sn.ID] = true
			snaps = append(snaps, sn)
		}
	}
	slices.SortFunc(snaps, func(a, b Snapshot) int { return cmp.Or(b.Time.Compare(a.Time), strings.Compare(a.ID, b.ID)) })
	truncated := res.Truncated
	if len(snaps) > MaxSnapshots {
		snaps, truncated = snaps[:MaxSnapshots], true
	}
	if err := s.repo.ReplaceSnapshots(ctx, r.ID, snaps, truncated, asked, mark); err != nil {
		return SnapshotsResult{}, err
	}
	return res, nil
}

// refreshMessage is the SnapshotList.RefreshError of a failed refresh:
// the message of a domain error (written for people, scrubbed by the
// prober), a generic one for anything else, which is logged.
func (s *Service) refreshMessage(ctx context.Context, id domain.ResticRepoID, err error) string {
	var verr *domain.ValidationError
	for _, class := range []error{domain.ErrUnavailable, domain.ErrFailedPrecondition, domain.ErrRateLimited, domain.ErrNotFound} {
		if errors.Is(err, class) {
			return clipRunes(err.Error(), MaxDeletionErrorLen)
		}
	}
	if errors.As(err, &verr) {
		return clipRunes(err.Error(), MaxDeletionErrorLen)
	}
	s.log.ErrorContext(ctx, "snapshot refresh failed", "repository_id", id.String(), "error", err.Error())
	return "the snapshots could not be refreshed"
}

// Browse lists the directory path (a snapshot tree path; "" is the root
// "/") of snapshot snapshotID of one of actor's repositories, through
// workerID or a worker the prober picks. Invalid input is a
// *domain.ValidationError on "snapshot_id" or "path";
// ErrRepositoryDeleting while the repository's data is being deleted.
func (s *Service) Browse(ctx context.Context, actor domain.Actor, id domain.ResticRepoID, snapshotID, path string, workerID domain.WorkerID) (Listing, error) {
	const op = "browse snapshot"
	r, err := s.owned(ctx, actor.UserID, id)
	if err != nil {
		return Listing{}, fmt.Errorf("%s: %w", op, err)
	}
	var v domain.Validation
	if !snapshotIDPattern.MatchString(snapshotID) {
		v.Add("snapshot_id", "must be a snapshot ID of 8 to 64 hex digits")
	}
	if path == "" {
		path = "/"
	}
	if msg := checkTreePath(path); msg != "" {
		v.Add("path", msg)
	}
	if err := v.Err(); err != nil {
		return Listing{}, fmt.Errorf("%s: %w", op, err)
	}
	if r.Deleting {
		return Listing{}, fmt.Errorf("%s of repository %s: %w", op, id, ErrRepositoryDeleting)
	}
	password, err := s.open(r)
	if err != nil {
		return Listing{}, fmt.Errorf("%s of repository %s: %w", op, id, err)
	}
	l, err := s.prober.List(ctx, actor.UserID, Target{RepositoryID: id, RemoteID: r.RemoteID, Path: r.Path, Password: password, WorkerID: workerID}, snapshotID, path)
	if err != nil {
		return Listing{}, fmt.Errorf("%s of repository %s: %w", op, id, scrubError(err, password))
	}
	if l.Path == "" {
		l.Path = path
	}
	return l, nil
}

// UsableRepository returns the name and remote of id, which must be a live
// repository of owner: ErrUnknownRepository otherwise. A repository whose
// data is being deleted is usable only by its own deletion job (jobID
// equal to its DeletionJobID; zero for jobs run by hand and for
// schedules): ErrRepositoryDeleting otherwise. It implements the jobs and
// schedules services' RepositoryChecker.
func (s *Service) UsableRepository(ctx context.Context, owner domain.UserID, id domain.ResticRepoID, jobID domain.JobID) (name string, remoteID domain.RemoteID, err error) {
	r, err := s.owned(ctx, owner, id)
	if err != nil {
		return "", domain.RemoteID{}, fmt.Errorf("check repository: %w", err)
	}
	if r.Deleting && (jobID.IsZero() || jobID != r.DeletionJobID) {
		return "", domain.RemoteID{}, fmt.Errorf("check repository %s: %w", id, ErrRepositoryDeleting)
	}
	return r.Name, r.RemoteID, nil
}

// Names returns display references of ids, of any owner and deleted ones
// included; missing ones are omitted. It is for listings of objects that
// refer to repositories (jobs, schedules), never for using them.
func (s *Service) Names(ctx context.Context, ids []domain.ResticRepoID) (map[domain.ResticRepoID]Ref, error) {
	out := make(map[domain.ResticRepoID]Ref, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rs, err := s.repo.GetRepos(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("repository names: %w", err)
	}
	for _, r := range rs {
		out[r.ID] = Ref{ID: r.ID, OwnerID: r.OwnerID, Name: r.Name, RemoteID: r.RemoteID, Deleted: r.Deleted()}
	}
	return out, nil
}

// Resolve returns the live repository id of owner with its decrypted
// password, for a job or probe spec: ErrUnknownRepository (which matches
// ErrNotFound) otherwise. A repository whose data is being deleted
// resolves too: its older jobs and its deletion job still run. A stored
// password that cannot be decrypted (a lost or rotated-out secrets key)
// is ErrFailedPrecondition: retrying cannot help. The result holds the
// password: never log or store it.
func (s *Service) Resolve(ctx context.Context, owner domain.UserID, id domain.ResticRepoID) (Resolved, error) {
	r, err := s.owned(ctx, owner, id)
	if err != nil {
		return Resolved{}, fmt.Errorf("resolve repository: %w", err)
	}
	password, err := s.open(r)
	if err != nil {
		return Resolved{}, fmt.Errorf("resolve repository %s: %w", id, err)
	}
	return Resolved{ID: r.ID, OwnerID: r.OwnerID, Name: r.Name, RemoteID: r.RemoteID, Path: r.Path, Password: password, ResticID: r.ResticID}, nil
}

// open decrypts r's password. The cause of a failure names no secret.
func (s *Service) open(r Repo) (string, error) {
	if len(r.PasswordEnc) == 0 {
		return "", fmt.Errorf("%w: the repository has no stored password", domain.ErrFailedPrecondition)
	}
	plain, err := s.cipher.Decrypt(r.PasswordEnc, PasswordAD(r.ID))
	if err != nil {
		return "", fmt.Errorf("%w: the repository's password cannot be read: %w", domain.ErrFailedPrecondition, err)
	}
	return string(plain), nil
}

// ---------------------------------------------------------------------------
// Pure helpers
// ---------------------------------------------------------------------------

var (
	// hex64Pattern matches a full restic or snapshot ID.
	hex64Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// snapshotIDPattern matches a snapshot ID as callers may give it.
	snapshotIDPattern = regexp.MustCompile(`^[0-9a-f]{8,64}$`)
)

// segments returns the folders of a repository path, without empty and "."
// segments ("" is the remote's root: no segment).
func segments(p string) []string {
	var out []string
	for seg := range strings.SplitSeq(p, "/") {
		if seg != "" && seg != "." {
			out = append(out, seg)
		}
	}
	return out
}

// within reports whether path p is dir or lies inside it, comparing whole
// folders ("data2" is not inside "data"; everything is inside the root).
func within(p, dir string) bool {
	ps, ds := segments(p), segments(dir)
	return len(ps) >= len(ds) && slices.Equal(ps[:len(ds)], ds)
}

// displayPath names a repository path for a message.
func displayPath(p string) string {
	if p == "" {
		return "the remote's root"
	}
	return strconv.Quote(p)
}

// checkOverlap returns a *domain.ValidationError on "path" if a repository
// at path on remote would lie inside the folder of one of rs on the same
// remote, or hold one inside its own: a data deletion of the outer one
// could reach the inner one. An equal location is ErrLocationTaken,
// checked before.
func checkOverlap(rs []Repo, remote domain.RemoteID, path string) error {
	for _, r := range rs {
		if r.RemoteID != remote || r.Deleted() {
			continue
		}
		switch {
		case within(path, r.Path):
			return fieldError("path", fmt.Sprintf("is inside the folder of repository %q (at %s); repositories on a remote cannot be nested", r.Name, displayPath(r.Path)))
		case within(r.Path, path):
			return fieldError("path", fmt.Sprintf("holds repository %q (at %s) in its folder; repositories on a remote cannot be nested", r.Name, displayPath(r.Path)))
		}
	}
	return nil
}

// addFields adds the fields of a validation error to v.
func addFields(v *domain.Validation, err error) {
	var verr *domain.ValidationError
	if errors.As(err, &verr) {
		v.AddAll("", verr.Fields)
	}
}

// fieldError returns a validation error on one field.
func fieldError(field, msg string) error {
	return &domain.ValidationError{Fields: []domain.FieldError{{Field: field, Message: msg}}}
}

// checkName validates a repository name and returns it without
// surrounding white space.
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

// checkDescription validates a description and returns it trimmed.
func checkDescription(v *domain.Validation, d string) string {
	d = strings.TrimSpace(d)
	switch {
	case !utf8.ValidString(d):
		v.Add("description", "is not valid UTF-8")
	case utf8.RuneCountInString(d) > MaxDescriptionLen:
		v.Add("description", fmt.Sprintf("must be at most %d characters", MaxDescriptionLen))
	case strings.ContainsRune(d, 0):
		v.Add("description", "must not contain NUL bytes")
	}
	return d
}

// checkPassword validates a repository password: not empty, at most
// MaxPasswordLen bytes, no NUL; ModeInit also needs MinInitPasswordLen
// characters (ModeConnect accepts what the repository has). The error is a
// *domain.ValidationError on "password" and never contains the password.
func checkPassword(mode Mode, pw string) error {
	switch {
	case pw == "":
		return fieldError("password", "is required")
	case len(pw) > MaxPasswordLen:
		return fieldError("password", fmt.Sprintf("must be at most %d bytes", MaxPasswordLen))
	case strings.ContainsRune(pw, 0):
		return fieldError("password", "must not contain NUL bytes")
	case !utf8.ValidString(pw):
		return fieldError("password", "is not valid UTF-8")
	case mode == ModeInit && utf8.RuneCountInString(pw) < MinInitPasswordLen:
		return fieldError("password", fmt.Sprintf("must be at least %d characters for a new repository", MinInitPasswordLen))
	}
	return nil
}

// checkTreePath returns what is wrong with a snapshot tree path ("" if
// nothing): it starts with "/", has no ".." segment and no control
// character.
func checkTreePath(p string) string {
	switch {
	case !strings.HasPrefix(p, "/"):
		return `must be a snapshot path starting with "/"`
	case len(p) > 4*MaxPathLen:
		return fmt.Sprintf("must be at most %d bytes", 4*MaxPathLen)
	case strings.ContainsFunc(p, unicode.IsControl):
		return "must not contain control characters"
	}
	for seg := range strings.SplitSeq(p, "/") {
		if seg == ".." {
			return `must not contain ".." segments`
		}
	}
	return ""
}

// noRepository is the message of a location without a restic repository;
// objectStoreHint is added on bucket storage (backend.Capabilities
// ObjectStore), where a mistyped folder does not fail but lists as empty.
const (
	noRepository    = "no restic repository at this location"
	objectStoreHint = "; on this kind of remote a folder that does not exist looks empty, so check the path for typos"
)

// verifyErrorFields maps what a worker reported while initializing or
// verifying a repository to the form field it concerns; anything else is
// ErrFailedPrecondition with the worker's (scrubbed) message. objectStore
// says the remote is bucket storage, for the message of a location without
// a repository.
func verifyErrorFields(mode Mode, pe *ProbeError, objectStore bool) error {
	switch {
	case pe.Code == codeWrongPassword:
		return fieldError("password", "is not this repository's password")
	case pe.Code == codeRepositoryNotFound && mode == ModeConnect:
		if objectStore {
			return fieldError("path", noRepository+objectStoreHint)
		}
		return fieldError("path", noRepository)
	case pe.Code == codeAlreadyExists && mode == ModeInit:
		return fieldError("mode", "a repository already exists here; choose Connect")
	case pe.Code == codePathNotAllowed:
		return fieldError("worker_id", "the location is outside the worker's allowed folders")
	}
	return fmt.Errorf("%w: %s", domain.ErrFailedPrecondition, cmp.Or(pe.Message, pe.Code, "the worker reported a failure without details"))
}

// verifyError maps an error of Prober.Verify: a *ProbeError through
// verifyErrorFields, anything else unchanged; the password never appears
// in the result. The prober scrubs what workers report; this is the last
// line of defense for a password a worker echoed.
func verifyError(mode Mode, err error, password string, objectStore bool) error {
	var pe *ProbeError
	if errors.As(err, &pe) {
		clean := *pe
		clean.Message = redact.Scrub(pe.Message, password)
		return verifyErrorFields(mode, &clean, objectStore)
	}
	return scrubError(err, password)
}

// isRepositoryNotFound reports whether err is the worker's report that a
// location holds no restic repository.
func isRepositoryNotFound(err error) bool {
	var pe *ProbeError
	return errors.As(err, &pe) && pe.Code == codeRepositoryNotFound
}

// objectStore reports whether owner's remote is bucket storage (see
// verifyErrorFields); false when that cannot be told.
func (s *Service) objectStore(ctx context.Context, owner domain.UserID, remoteID domain.RemoteID) bool {
	typeID, err := s.remotes.RemoteType(ctx, owner, remoteID)
	if err != nil {
		return false
	}
	typ, ok := s.backends.Get(typeID)
	return ok && typ.Capabilities().ObjectStore
}

// scrubError returns err unless its message contains secret (in any form
// redact.Scrub replaces); then an error of the same domain class with the
// scrubbed message.
func scrubError(err error, secret string) error {
	msg := err.Error()
	clean := redact.Scrub(msg, secret)
	if clean == msg {
		return err
	}
	for _, class := range []error{
		domain.ErrNotFound, domain.ErrUnavailable, domain.ErrFailedPrecondition, domain.ErrInvalidArgument,
		domain.ErrRateLimited, context.Canceled, context.DeadlineExceeded,
	} {
		if errors.Is(err, class) {
			return fmt.Errorf("%w: %s", class, clean)
		}
	}
	return errors.New(clean)
}

// stale reports whether l's cache may be out of date: it was never
// filled, or a job that may have changed the snapshots ended since it was
// taken (its mark differs from the repository's).
func stale(l SnapshotList) bool {
	return l.RefreshedAt.IsZero() || !l.Mark.Equal(l.CurrentMark)
}

// clipRunes returns s cut to at most n runes.
func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
