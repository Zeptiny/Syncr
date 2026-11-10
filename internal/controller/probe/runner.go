package probe

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
	"github.com/Zeptiny/Syncr/internal/link"
	"github.com/Zeptiny/Syncr/internal/platform/redact"
)

// Limits of Config.
const (
	// MinTimeout and MaxTimeout bound the probe timeouts (workers cap them
	// at MaxTimeout too).
	MinTimeout = time.Second
	MaxTimeout = 90 * time.Second
	// MaxListEntries is the most entries a listing may ask for.
	MaxListEntries = 5000
	// maxFailureRunes bounds a worker's failure message.
	maxFailureRunes = 500
)

// Probe operations (the names of ProbeSpec.op's fields; link.ProbeOps).
const (
	opListLocal       = "list_local"
	opListRemote      = "list_remote"
	opAbout           = "about"
	opResticSnapshots = "restic_snapshots"
	opResticLs        = "restic_ls"
	opResticInit      = "restic_init"
	opResticConfig    = "restic_config"
)

// localRemoteHint is the "worker_id" message of a probe on a remote that
// is a worker's own filesystem.
const localRemoteHint = "choose the worker whose disk holds this remote"

// Config tunes a Runner. Zero fields take the DefaultConfig values.
type Config struct {
	// Timeout bounds listings and usage requests, ResticTimeout the restic
	// operations; each between MinTimeout and MaxTimeout.
	Timeout       time.Duration
	ResticTimeout time.Duration
	// MaxEntries is the most entries a listing returns (at most
	// MaxListEntries); MaxSnapshots the most (newest) snapshots.
	MaxEntries   int
	MaxSnapshots int
}

// DefaultConfig returns the defaults: 30 s for listings, 60 s for restic,
// 2000 entries and 5000 snapshots.
func DefaultConfig() Config {
	return Config{Timeout: 30 * time.Second, ResticTimeout: 60 * time.Second, MaxEntries: 2000, MaxSnapshots: 5000}
}

// Deps are a Runner's collaborators. All are required except Logger.
type Deps struct {
	Transport Transport
	Workers   Workers
	Jobs      Jobs
	Remotes   Remotes
	Limiter   Limiter
	IDs       IDGenerator
	// Logger defaults to discarding. Specs and results are never logged.
	Logger *slog.Logger
}

// Runner runs probes (see the package documentation). Create it with
// NewRunner; safe for concurrent use.
type Runner struct {
	transport Transport
	workers   Workers
	jobs      Jobs
	remotes   Remotes
	limiter   Limiter
	ids       IDGenerator
	log       *slog.Logger
	cfg       Config
}

var _ restic.Prober = (*Runner)(nil)

// NewRunner returns a Runner.
func NewRunner(d Deps, cfg Config) (*Runner, error) {
	if d.Transport == nil || d.Workers == nil || d.Jobs == nil || d.Remotes == nil || d.Limiter == nil || d.IDs == nil {
		return nil, errors.New("probe: missing dependency")
	}
	def := DefaultConfig()
	cfg.Timeout = cmp.Or(cfg.Timeout, def.Timeout)
	cfg.ResticTimeout = cmp.Or(cfg.ResticTimeout, def.ResticTimeout)
	cfg.MaxEntries = cmp.Or(cfg.MaxEntries, def.MaxEntries)
	cfg.MaxSnapshots = cmp.Or(cfg.MaxSnapshots, def.MaxSnapshots)
	for _, t := range []time.Duration{cfg.Timeout, cfg.ResticTimeout} {
		if t < MinTimeout || t > MaxTimeout {
			return nil, fmt.Errorf("probe: invalid config: timeout %v is not between %v and %v", t, MinTimeout, MaxTimeout)
		}
	}
	if cfg.MaxEntries < 1 || cfg.MaxEntries > MaxListEntries || cfg.MaxSnapshots < 1 {
		return nil, fmt.Errorf("probe: invalid config: max_entries %d, max_snapshots %d", cfg.MaxEntries, cfg.MaxSnapshots)
	}
	log := d.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Runner{
		transport: d.Transport, workers: d.Workers, jobs: d.Jobs, remotes: d.Remotes,
		limiter: d.Limiter, ids: d.IDs, log: log, cfg: cfg,
	}, nil
}

// ---------------------------------------------------------------------------
// restic.Prober
// ---------------------------------------------------------------------------

// Verify implements restic.Prober: restic init (init true) or cat config
// on t, through t.WorkerID or a picked worker.
func (r *Runner) Verify(ctx context.Context, owner domain.UserID, t restic.Target, init bool) (restic.VerifyResult, error) {
	what := "read the repository"
	op := opResticConfig
	if init {
		what, op = "create the repository", opResticInit
	}
	if err := r.allow(owner); err != nil {
		return restic.VerifyResult{}, fmt.Errorf("%s: %w", what, err)
	}
	rs, err := r.repository(ctx, owner, t)
	if err != nil {
		return restic.VerifyResult{}, fmt.Errorf("%s: %w", what, err)
	}
	spec := &syncrv1.ProbeSpec{Remotes: rs.defs, Op: &syncrv1.ProbeSpec_ResticConfig{ResticConfig: &syncrv1.ProbeResticConfig{Repository: rs.repo}}}
	if init {
		spec.Op = &syncrv1.ProbeSpec_ResticInit{ResticInit: &syncrv1.ProbeResticInit{Repository: rs.repo}}
	}
	w, res, err := r.onRepository(ctx, owner, t, rs, op, spec)
	if err != nil {
		return restic.VerifyResult{}, fmt.Errorf("%s: %w", what, err)
	}
	cfg := res.GetRepositoryConfig()
	if cfg.GetId() == "" {
		return restic.VerifyResult{}, fmt.Errorf("%s: %w: worker %q answered without the repository's config", what, domain.ErrUnavailable, w.Name)
	}
	return restic.VerifyResult{ResticID: cfg.GetId(), FormatVersion: int(cfg.GetVersion()), WorkerID: w.ID, WorkerName: w.Name}, nil
}

// Snapshots implements restic.Prober: the newest Config.MaxSnapshots
// snapshots of t.
func (r *Runner) Snapshots(ctx context.Context, owner domain.UserID, t restic.Target) (restic.SnapshotsResult, error) {
	const what = "list the snapshots"
	if err := r.allow(owner); err != nil {
		return restic.SnapshotsResult{}, fmt.Errorf("%s: %w", what, err)
	}
	rs, err := r.repository(ctx, owner, t)
	if err != nil {
		return restic.SnapshotsResult{}, fmt.Errorf("%s: %w", what, err)
	}
	spec := &syncrv1.ProbeSpec{Remotes: rs.defs, Op: &syncrv1.ProbeSpec_ResticSnapshots{ResticSnapshots: &syncrv1.ProbeResticSnapshots{
		Repository: rs.repo, MaxSnapshots: int32(r.cfg.MaxSnapshots), //nolint:gosec // G115: bounded by the configuration
	}}}
	w, res, err := r.onRepository(ctx, owner, t, rs, opResticSnapshots, spec)
	if err != nil {
		return restic.SnapshotsResult{}, fmt.Errorf("%s: %w", what, err)
	}
	out := restic.SnapshotsResult{Truncated: res.GetSnapshots().GetTruncated(), WorkerID: w.ID, WorkerName: w.Name}
	for _, s := range res.GetSnapshots().GetSnapshots() {
		out.Snapshots = append(out.Snapshots, snapshotFromProto(s))
	}
	return out, nil
}

// List implements restic.Prober: the directory path (a tree path) of
// snapshot snapshotID of t, at most Config.MaxEntries entries.
func (r *Runner) List(ctx context.Context, owner domain.UserID, t restic.Target, snapshotID, path string) (restic.Listing, error) {
	const what = "list the snapshot"
	if err := r.allow(owner); err != nil {
		return restic.Listing{}, fmt.Errorf("%s: %w", what, err)
	}
	rs, err := r.repository(ctx, owner, t)
	if err != nil {
		return restic.Listing{}, fmt.Errorf("%s: %w", what, err)
	}
	spec := &syncrv1.ProbeSpec{Remotes: rs.defs, Op: &syncrv1.ProbeSpec_ResticLs{ResticLs: &syncrv1.ProbeResticLs{
		Repository: rs.repo, SnapshotId: snapshotID, Path: path, MaxEntries: int32(r.cfg.MaxEntries), //nolint:gosec // G115: bounded by the configuration
	}}}
	w, res, err := r.onRepository(ctx, owner, t, rs, opResticLs, spec)
	if err != nil {
		return restic.Listing{}, fmt.Errorf("%s: %w", what, err)
	}
	l := res.GetListing()
	out := restic.Listing{WorkerID: w.ID, WorkerName: w.Name, Path: l.GetPath(), Truncated: l.GetTruncated()}
	for _, e := range l.GetEntries() {
		out.Entries = append(out.Entries, entryFromProto(e))
	}
	return out, nil
}

// resolvedRepository is a probe target ready for a spec.
type resolvedRepository struct {
	defs    []*syncrv1.RemoteDefinition
	repo    *syncrv1.ResticRepository
	local   bool
	secrets []string
}

// repository resolves t's remote for owner and locates the repository on
// it.
func (r *Runner) repository(ctx context.Context, owner domain.UserID, t restic.Target) (resolvedRepository, error) {
	if t.RemoteID.IsZero() {
		return resolvedRepository{}, fmt.Errorf("remote %w", domain.ErrNotFound)
	}
	rs, err := r.remotes.Resolve(ctx, owner, []domain.RemoteID{t.RemoteID})
	if err != nil {
		return resolvedRepository{}, fmt.Errorf("resolve the remote: %w", err)
	}
	defs, repo, err := jobkind.LocateRepository(jobkind.Repository{
		ID: t.RepositoryID, RemoteID: t.RemoteID, Path: t.Path, Password: t.Password,
	}, kindRemotes(rs))
	if err != nil {
		return resolvedRepository{}, pathError(err)
	}
	return resolvedRepository{
		defs: defs, repo: repo, local: rs[t.RemoteID].Rclone.Backend == "",
		secrets: append(secretsOf(rs), t.Password),
	}, nil
}

// onRepository picks the worker of a repository probe and runs it.
func (r *Runner) onRepository(ctx context.Context, owner domain.UserID, t restic.Target, rs resolvedRepository, op string, spec *syncrv1.ProbeSpec) (workers.Worker, *syncrv1.ProbeResult, error) {
	w, err := r.worker(ctx, selection{
		owner: owner, op: op, workerID: t.WorkerID, repositoryID: t.RepositoryID,
		explicit: cond(rs.local, "choose the worker whose disk holds this repository"),
	})
	if err != nil {
		return workers.Worker{}, nil, err
	}
	res, err := r.exec(ctx, w, spec, r.cfg.ResticTimeout, rs.secrets)
	return w, res, err
}

// cond returns msg if on, "" otherwise.
func cond(on bool, msg string) string {
	if on {
		return msg
	}
	return ""
}

// ---------------------------------------------------------------------------
// The steps of a probe
// ---------------------------------------------------------------------------

// allow applies the user's rate limit.
func (r *Runner) allow(user domain.UserID) error {
	if user.IsZero() {
		return fmt.Errorf("%w: the caller is not a user", domain.ErrInvalidID)
	}
	if !r.limiter.Allow(user.String()) {
		return fmt.Errorf("%w: too many requests to workers; wait a moment", domain.ErrRateLimited)
	}
	return nil
}

// selection says how to choose a probe's worker.
type selection struct {
	owner domain.UserID
	op    string
	// workerID is the caller's choice; zero lets the runner pick.
	workerID domain.WorkerID
	// explicit, if not empty, is why the caller must choose (the
	// "worker_id" message).
	explicit string
	// repositoryID prefers the worker of its newest succeeded job.
	repositoryID domain.ResticRepoID
}

// worker returns the worker of a probe (§6.5 of the M5 design).
func (r *Runner) worker(ctx context.Context, s selection) (workers.Worker, error) {
	if !s.workerID.IsZero() {
		w, err := r.workers.Get(ctx, s.workerID)
		found := err == nil && !w.Removed()
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return workers.Worker{}, fmt.Errorf("load worker %s: %w", s.workerID, err)
		}
		if err := CheckExplicit(r.candidate(w), found, s.op); err != nil {
			return workers.Worker{}, err
		}
		return w, nil
	}
	if s.explicit != "" {
		return workers.Worker{}, fieldError("worker_id", s.explicit)
	}
	ws, err := r.workers.List(ctx, workers.ListFilter{})
	if err != nil {
		return workers.Worker{}, fmt.Errorf("list the workers: %w", err)
	}
	preferred, err := lastWorker(ctx, r.jobs, s.owner, jobs.ListFilter{RepositoryID: s.repositoryID}, !s.repositoryID.IsZero())
	if err != nil {
		return workers.Worker{}, err
	}
	byID := make(map[domain.WorkerID]workers.Worker, len(ws))
	cands := make([]Candidate, 0, len(ws))
	for _, w := range ws {
		if w.Removed() {
			continue
		}
		c := r.candidate(w)
		c.Preferred = !preferred.IsZero() && w.ID == preferred
		if c.Status == domain.WorkerOnline && c.canProbe(s.op) {
			js, err := r.jobs.Unfinished(ctx, w.ID)
			if err != nil {
				return workers.Worker{}, fmt.Errorf("count the jobs of worker %s: %w", w.ID, err)
			}
			c.Unfinished = len(js)
		}
		byID[w.ID] = w
		cands = append(cands, c)
	}
	c, err := Pick(cands, s.op)
	if err != nil {
		return workers.Worker{}, err
	}
	return byID[c.ID], nil
}

// lastWorker returns the worker of owner's newest succeeded job matching
// f; zero if there is none or want is false.
func lastWorker(ctx context.Context, js Jobs, owner domain.UserID, f jobs.ListFilter, want bool) (domain.WorkerID, error) {
	if !want {
		return domain.WorkerID{}, nil
	}
	f.Statuses, f.Limit = []domain.JobStatus{domain.JobSucceeded}, 1
	page, err := js.List(ctx, domain.Actor{UserID: owner, Role: domain.RoleUser}, f)
	if err != nil {
		return domain.WorkerID{}, fmt.Errorf("find the last worker: %w", err)
	}
	if len(page.Jobs) == 0 {
		return domain.WorkerID{}, nil
	}
	return page.Jobs[0].WorkerID, nil
}

func (r *Runner) candidate(w workers.Worker) Candidate {
	return candidateOf(w, r.workers.Status(w))
}

func candidateOf(w workers.Worker, st domain.WorkerStatus) Candidate {
	return Candidate{
		ID: w.ID, Name: w.Name, Status: st, Version: w.Info.Version,
		Features: w.Info.Features, ProbeOps: w.Info.ProbeOps, JobKinds: w.Info.JobKinds,
	}
}

// exec sends spec to w and returns its result if the probe succeeded.
// Every error message is scrubbed of secrets.
func (r *Runner) exec(ctx context.Context, w workers.Worker, spec *syncrv1.ProbeSpec, timeout time.Duration, secrets []string) (*syncrv1.ProbeResult, error) {
	op := link.ProbeOpOf(spec)
	run := &syncrv1.RunProbe{ProbeId: r.ids.NewProbeID().String(), Spec: spec, Timeout: durationpb.New(timeout)}
	res, err := r.transport.Probe(ctx, w.ID, run)
	if err != nil {
		r.log.InfoContext(ctx, "probe not answered", "worker_id", w.ID.String(), "op", op, "probe_id", run.GetProbeId(),
			"error", redact.Scrub(err.Error(), secrets...))
		return nil, scrubError(err, secrets)
	}
	if res.GetStatus() == syncrv1.ProbeStatus_PROBE_STATUS_OK {
		return res, nil
	}
	f := failure(w.Name, res, secrets)
	r.log.InfoContext(ctx, "probe failed", "worker_id", w.ID.String(), "op", op, "probe_id", run.GetProbeId(),
		"error_code", f.probe.Code, "error", f.probe.Message)
	return nil, f
}

// ---------------------------------------------------------------------------
// Failures
// ---------------------------------------------------------------------------

// workerError is a failure the worker reported: it matches the domain
// error it maps to (errors.Is, and errors.As for a
// *domain.ValidationError) and carries the worker's code and scrubbed
// message as a *restic.ProbeError (errors.As).
type workerError struct {
	mapped error
	probe  *restic.ProbeError
}

func (e *workerError) Error() string   { return e.mapped.Error() }
func (e *workerError) Unwrap() []error { return []error{e.mapped, e.probe} }

// failure maps a result that is not OK (M5 design §6.7).
func failure(workerName string, res *syncrv1.ProbeResult, secrets []string) *workerError {
	code := link.ErrorCodeName(res.GetError().GetCode())
	msg := clip(redact.Scrub(res.GetError().GetMessage(), secrets...))
	pe := &restic.ProbeError{Code: code, Message: msg}
	var mapped error
	switch {
	case res.GetStatus() == syncrv1.ProbeStatus_PROBE_STATUS_CANCELLED:
		pe.Code = cmp.Or(nonUnspecified(code), jobs.CodeCancelled)
		mapped = fmt.Errorf("%w: the request was stopped on worker %q before it finished", domain.ErrUnavailable, workerName)
	case res.GetError().GetCode() == syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED:
		mapped = fieldError("path", "is outside the worker's allowed folders")
	case res.GetError().GetCode() == syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND:
		mapped = fieldError("path", "does not exist")
	case res.GetError().GetCode() == syncrv1.JobErrorCode_JOB_ERROR_CODE_WRONG_PASSWORD:
		mapped = fmt.Errorf("%w: the stored password no longer opens this repository", domain.ErrFailedPrecondition)
	case res.GetError().GetCode() == syncrv1.JobErrorCode_JOB_ERROR_CODE_REPOSITORY_NOT_FOUND:
		mapped = fmt.Errorf("%w: no repository at the stored location", domain.ErrFailedPrecondition)
	case res.GetError().GetCode() == syncrv1.JobErrorCode_JOB_ERROR_CODE_TIMEOUT:
		mapped = fmt.Errorf("%w: worker %q did not finish in time", domain.ErrUnavailable, workerName)
	case res.GetError().GetCode() == syncrv1.JobErrorCode_JOB_ERROR_CODE_BUSY:
		mapped = fmt.Errorf("%w: worker %q is busy with other requests; try again in a moment", domain.ErrUnavailable, workerName)
	default:
		mapped = fmt.Errorf("%w: %s", domain.ErrFailedPrecondition, cmp.Or(msg, "the worker reported a failure without details"))
	}
	return &workerError{mapped: mapped, probe: pe}
}

func nonUnspecified(code string) string {
	if code == jobs.CodeUnspecified {
		return ""
	}
	return code
}

// scrubError returns err unless its message contains a secret; then an
// error of the same domain class with the scrubbed message.
func scrubError(err error, secrets []string) error {
	msg := err.Error()
	clean := redact.Scrub(msg, secrets...)
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

// fieldError returns a validation error on one field.
func fieldError(field, msg string) error {
	return &domain.ValidationError{Fields: []domain.FieldError{{Field: field, Message: msg}}}
}

// pathError turns an invalid path (a *backend.PathError from Locate) into
// a validation error on "path"; other errors pass.
func pathError(err error) error {
	var pe *backend.PathError
	if errors.As(err, &pe) {
		return fieldError("path", pe.Reason)
	}
	return err
}

// clip bounds a message to maxFailureRunes runes.
func clip(s string) string {
	if utf8.RuneCountInString(s) <= maxFailureRunes {
		return s
	}
	n := 0
	for i := range s {
		if n == maxFailureRunes {
			return s[:i] + "…"
		}
		n++
	}
	return s
}

// ---------------------------------------------------------------------------
// Conversions
// ---------------------------------------------------------------------------

// kindRemotes converts resolved remotes for jobkind.Locate.
func kindRemotes(rs map[domain.RemoteID]remotes.Resolved) map[domain.RemoteID]jobkind.Remote {
	out := make(map[domain.RemoteID]jobkind.Remote, len(rs))
	for id, r := range rs {
		members := make([]jobkind.Member, len(r.Members))
		for i, m := range r.Members {
			members[i] = jobkind.Member{RemoteID: m.RemoteID, Path: m.Path, Mode: m.Mode}
		}
		out[id] = jobkind.Remote{ID: id, Rclone: r.Rclone, Members: members}
	}
	return out
}

// secretsOf returns every secret value of the resolved remotes.
func secretsOf(rs map[domain.RemoteID]remotes.Resolved) []string {
	var out []string
	for _, r := range rs {
		for _, v := range r.Rclone.Secrets {
			if v != "" {
				out = append(out, v)
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func snapshotFromProto(s *syncrv1.ResticSnapshot) restic.Snapshot {
	out := restic.Snapshot{
		ID: s.GetId(), Time: timeOf(s.GetTime()), Hostname: s.GetHost(), Username: s.GetUsername(),
		Parent: s.GetParent(), ProgramVersion: s.GetProgramVersion(),
		Paths: slices.Clone(s.GetPaths()), Tags: slices.Clone(s.GetTags()),
	}
	if sum := s.GetSummary(); sum != nil {
		out.Summary = &restic.SnapshotSummary{
			BackupStart: timeOf(sum.GetBackupStart()), BackupEnd: timeOf(sum.GetBackupEnd()),
			FilesNew: sum.GetFilesNew(), FilesChanged: sum.GetFilesChanged(), FilesUnmodified: sum.GetFilesUnmodified(),
			DirsNew: sum.GetDirsNew(), DirsChanged: sum.GetDirsChanged(), DirsUnmodified: sum.GetDirsUnmodified(),
			DataBlobs: sum.GetDataBlobs(), TreeBlobs: sum.GetTreeBlobs(),
			DataAdded: sum.GetDataAddedBytes(), DataAddedPacked: sum.GetDataAddedPackedBytes(),
			TotalFilesProcessed: sum.GetTotalFilesProcessed(), TotalBytesProcessed: sum.GetTotalBytesProcessed(),
		}
	}
	return out
}

func entryFromProto(e *syncrv1.ProbeEntry) restic.Entry {
	out := restic.Entry{Name: e.GetName(), Path: e.GetPath(), Size: e.GetSize(), ModTime: timeOf(e.GetModTime()), Mode: e.GetMode()}
	switch e.GetType() {
	case syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_FILE:
		out.Type = restic.EntryFile
	case syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_DIR:
		out.Type = restic.EntryDir
	case syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_SYMLINK:
		out.Type = restic.EntrySymlink
	default:
		out.Type = restic.EntryOther
	}
	return out
}
