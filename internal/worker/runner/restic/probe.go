package restic

import (
	"context"
	pathpkg "path"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
	"github.com/Zeptiny/Syncr/internal/worker/runner/scrub"
)

// Probe kinds name the sessions of the probe operations; they are not job
// kinds.
const (
	kindProbeLs     = "probe_restic_ls"
	kindProbeConfig = "probe_restic_config"
)

// Prober runs the probe operations restic_snapshots, restic_ls, restic_init
// and restic_config with the runners' engine: the same binaries,
// environment, quoting and scrubbing. Reads use --no-lock, so they never
// conflict with a job on the repository. It is safe for concurrent use.
type Prober struct{ e *engine }

// NewProber returns a Prober for cfg.
func NewProber(cfg Config) (*Prober, error) {
	e, err := newEngine(cfg)
	if err != nil {
		return nil, err
	}
	return &Prober{e: e}, nil
}

// run executes one probe command; every error it returns is scrubbed.
func (p *Prober) run(ctx context.Context, kind, probeID string, remotes []*syncrv1.RemoteDefinition, cmd command) (*session, error) {
	secrets := append([]string{cmd.repo.GetPassword()}, remoteSecrets(remotes)...)
	if !jobIDPattern.MatchString(probeID) {
		return nil, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL, "invalid probe ID %q", probeID)
	}
	_, s, sc, err := p.e.exec(ctx, execution{
		kind: kind, scratch: "probe-" + probeID, logID: probeID,
		remotes: remotes, cmd: cmd, secrets: secrets,
	})
	if err != nil && !runner.Canceled(err) {
		err = sc.Error(err)
	}
	return s, err
}

func scrubbed(secretsOf *syncrv1.ResticRepository, remotes []*syncrv1.RemoteDefinition, err error) error {
	if err == nil || runner.Canceled(err) {
		return err
	}
	return scrub.New(append([]string{secretsOf.GetPassword()}, remoteSecrets(remotes)...)).Error(err)
}

// Snapshots lists the repository's snapshots (--no-lock snapshots), oldest
// first. Only the newest op.max_snapshots are kept when it is positive, and
// truncated is then set.
func (p *Prober) Snapshots(ctx context.Context, probeID string, remotes []*syncrv1.RemoteDefinition, op *syncrv1.ProbeResticSnapshots) (*syncrv1.ResticSnapshotsOutput, error) {
	repo := op.GetRepository()
	s, err := p.run(ctx, kindSnapshots, probeID, remotes, command{repo: repo, verb: "snapshots", global: []string{"--no-lock"}})
	if err != nil {
		return nil, err
	}
	out, err := s.snapshotsOutput(int(op.GetMaxSnapshots()))
	if err != nil {
		return nil, scrubbed(repo, remotes, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL, "restic snapshots: %w", err))
	}
	return out, nil
}

// Ls lists one directory of a snapshot (--no-lock ls): the direct children
// of op.path, directories first, then by name, at most op.max_entries.
func (p *Prober) Ls(ctx context.Context, probeID string, remotes []*syncrv1.RemoteDefinition, op *syncrv1.ProbeResticLs) (*syncrv1.ProbeListing, error) {
	repo := op.GetRepository()
	id := op.GetSnapshotId()
	if id != "latest" && !snapshotIDPattern.MatchString(id) {
		return nil, invalid("snapshot_id must be \"latest\" or a hexadecimal snapshot ID")
	}
	if err := checkTreePath("path", op.GetPath()); err != nil {
		return nil, err
	}
	dir := pathpkg.Clean(op.GetPath())
	filter := &lsFilter{dir: dir, max: int(op.GetMaxEntries())}
	if filter.max <= 0 {
		filter.max = defaultLsEntries
	}
	_, err := p.run(ctx, kindProbeLs, probeID, remotes, command{
		repo: repo, verb: "ls", global: []string{"--no-lock"}, args: []string{"--", id, dir}, ls: filter,
	})
	if err != nil {
		return nil, err
	}
	// restic lists a missing directory as an empty one; an existing one is
	// listed itself, except the root.
	if dir != "/" && !filter.found {
		return nil, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "the snapshot has no folder %q", dir)
	}
	return &syncrv1.ProbeListing{Path: dir, Entries: filter.sorted(), Truncated: filter.truncated}, nil
}

// defaultLsEntries bounds a listing whose request names no bound.
const defaultLsEntries = 2000

// Init creates the repository (init), then reads its config. A repository
// at the location already fails with ALREADY_EXISTS.
func (p *Prober) Init(ctx context.Context, probeID string, remotes []*syncrv1.RemoteDefinition, op *syncrv1.ProbeResticInit) (*syncrv1.ResticRepositoryConfig, error) {
	repo := op.GetRepository()
	if _, err := p.run(ctx, kindInit, probeID, remotes, command{repo: repo, verb: "init", noCache: true}); err != nil {
		return nil, err
	}
	return p.config(ctx, probeID, remotes, repo)
}

// Config reads the repository's config (--no-lock cat config), which
// verifies the location and the password.
func (p *Prober) Config(ctx context.Context, probeID string, remotes []*syncrv1.RemoteDefinition, op *syncrv1.ProbeResticConfig) (*syncrv1.ResticRepositoryConfig, error) {
	return p.config(ctx, probeID, remotes, op.GetRepository())
}

func (p *Prober) config(ctx context.Context, probeID string, remotes []*syncrv1.RemoteDefinition, repo *syncrv1.ResticRepository) (*syncrv1.ResticRepositoryConfig, error) {
	s, err := p.run(ctx, kindProbeConfig, probeID, remotes, catConfig(repo))
	if err != nil {
		return nil, err
	}
	cfg, err := s.repositoryConfig()
	if err != nil {
		return nil, scrubbed(repo, remotes, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL, "restic cat config: %w", err))
	}
	return cfg, nil
}

// catConfig is the command that reads a repository's config.
func catConfig(repo *syncrv1.ResticRepository) command {
	return command{repo: repo, verb: "cat", global: []string{"--no-lock"}, args: []string{"config"}, noCache: true, collect: true}
}
