package restic

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// Job kinds: the names of the JobSpec.operation fields.
const (
	kindInit      = "restic_init"
	kindBackup    = "restic_backup"
	kindForget    = "restic_forget"
	kindPrune     = "restic_prune"
	kindCheck     = "restic_check"
	kindRestore   = "restic_restore"
	kindSnapshots = "restic_snapshots"
)

// command is a validated restic invocation derived from a JobSpec operation.
type command struct {
	repo *syncrv1.ResticRepository
	// verb is the restic subcommand.
	verb string
	// args are the subcommand's flags and positional arguments. Every flag is
	// in "--name=value" form and positional arguments follow a "--"
	// separator, so no spec value can be parsed as a flag.
	args []string
	// noCache disables restic's local cache (the command gains nothing from
	// it).
	noCache bool
}

var (
	snapshotIDPattern = regexp.MustCompile(`^[0-9a-f]{4,64}$`)
	aliasPattern      = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
	jobIDPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
)

func invalid(format string, args ...any) error {
	return runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, format, args...)
}

// planFunc validates a spec's operation and builds the restic command.
type planFunc func(e *engine, spec *syncrv1.JobSpec) (command, error)

func planInit(_ *engine, spec *syncrv1.JobSpec) (command, error) {
	op := spec.GetResticInit()
	if op == nil {
		return command{}, noOperation(kindInit)
	}
	return command{repo: op.GetRepository(), verb: "init", noCache: true}, nil
}

func planBackup(e *engine, spec *syncrv1.JobSpec) (command, error) {
	op := spec.GetResticBackup()
	if op == nil {
		return command{}, noOperation(kindBackup)
	}
	if len(op.GetPaths()) == 0 {
		return command{}, invalid("backup needs at least one path")
	}
	var args []string
	tags, err := tagFlags("--tag=", op.GetTags())
	if err != nil {
		return command{}, err
	}
	args = append(args, tags...)
	if h := op.GetHost(); h != "" {
		if err := checkText("host", h); err != nil {
			return command{}, err
		}
		args = append(args, "--host="+h)
	}
	for _, pat := range op.GetExcludes() {
		if err := checkText("exclude pattern", pat); err != nil {
			return command{}, err
		}
		args = append(args, "--exclude="+pat)
	}
	if op.GetExcludeCaches() {
		args = append(args, "--exclude-caches")
	}
	if op.GetOneFileSystem() {
		args = append(args, "--one-file-system")
	}
	if op.ExcludeLargerThanBytes != nil {
		n := op.GetExcludeLargerThanBytes()
		if n <= 0 {
			return command{}, invalid("exclude_larger_than_bytes must be positive, got %d", n)
		}
		args = append(args, "--exclude-larger-than="+strconv.FormatInt(n, 10))
	}
	if op.GetDryRun() {
		args = append(args, "--dry-run")
	}
	if op.GetUseFsSnapshot() {
		if e.goos != "windows" {
			return command{}, invalid("use_fs_snapshot (VSS) is only available on Windows workers")
		}
		args = append(args, "--use-fs-snapshot")
	}
	args = append(args, "--")
	for _, p := range op.GetPaths() {
		resolved, err := e.resolve("backup path", p)
		if err != nil {
			return command{}, err
		}
		args = append(args, resolved)
	}
	return command{repo: op.GetRepository(), verb: "backup", args: args}, nil
}

func planForget(_ *engine, spec *syncrv1.JobSpec) (command, error) {
	op := spec.GetResticForget()
	if op == nil {
		return command{}, noOperation(kindForget)
	}
	pol := op.GetPolicy()
	var args []string
	keep := []struct {
		flag string
		n    int32
	}{
		{"--keep-last=", pol.GetKeepLast()},
		{"--keep-hourly=", pol.GetKeepHourly()},
		{"--keep-daily=", pol.GetKeepDaily()},
		{"--keep-weekly=", pol.GetKeepWeekly()},
		{"--keep-monthly=", pol.GetKeepMonthly()},
		{"--keep-yearly=", pol.GetKeepYearly()},
	}
	rules := 0
	for _, k := range keep {
		switch {
		case k.n < 0:
			return command{}, invalid("retention rule %s must not be negative", strings.TrimSuffix(k.flag, "="))
		case k.n > 0:
			args = append(args, k.flag+strconv.Itoa(int(k.n)))
			rules++
		}
	}
	keepTags, err := tagFlags("--keep-tag=", pol.GetKeepTags())
	if err != nil {
		return command{}, err
	}
	args = append(args, keepTags...)
	rules += len(keepTags)
	if rules == 0 {
		// restic would refuse, but fail before starting a process; a forget
		// without a rule must never be able to delete everything.
		return command{}, invalid("forget needs at least one retention rule")
	}
	filter, err := tagFlags("--tag=", op.GetTags())
	if err != nil {
		return command{}, err
	}
	args = append(args, filter...)
	if h := op.GetHost(); h != "" {
		if err := checkText("host", h); err != nil {
			return command{}, err
		}
		args = append(args, "--host="+h)
	}
	if op.GetPrune() {
		args = append(args, "--prune")
	}
	if op.GetDryRun() {
		args = append(args, "--dry-run")
	}
	return command{repo: op.GetRepository(), verb: "forget", args: args}, nil
}

func planPrune(_ *engine, spec *syncrv1.JobSpec) (command, error) {
	op := spec.GetResticPrune()
	if op == nil {
		return command{}, noOperation(kindPrune)
	}
	var args []string
	if op.GetDryRun() {
		args = append(args, "--dry-run")
	}
	return command{repo: op.GetRepository(), verb: "prune", args: args}, nil
}

func planCheck(_ *engine, spec *syncrv1.JobSpec) (command, error) {
	op := spec.GetResticCheck()
	if op == nil {
		return command{}, noOperation(kindCheck)
	}
	var args []string
	switch pct := op.GetReadDataPercent(); {
	case pct < 0 || pct > 100:
		return command{}, invalid("read_data_percent must be between 0 and 100, got %d", pct)
	case pct > 0:
		args = append(args, "--read-data-subset="+strconv.Itoa(int(pct))+"%")
	}
	return command{repo: op.GetRepository(), verb: "check", args: args}, nil
}

func planRestore(e *engine, spec *syncrv1.JobSpec) (command, error) {
	op := spec.GetResticRestore()
	if op == nil {
		return command{}, noOperation(kindRestore)
	}
	id := op.GetSnapshotId()
	if id != "latest" && !snapshotIDPattern.MatchString(id) {
		return command{}, invalid("snapshot_id must be \"latest\" or a hexadecimal snapshot ID")
	}
	if op.GetTarget() == "" {
		return command{}, invalid("restore needs a target directory")
	}
	target, err := e.resolve("restore target", op.GetTarget())
	if err != nil {
		return command{}, err
	}
	args := []string{"--target=" + target}
	for _, pat := range op.GetIncludes() {
		if err := checkText("include pattern", pat); err != nil {
			return command{}, err
		}
		args = append(args, "--include="+pat)
	}
	for _, pat := range op.GetExcludes() {
		if err := checkText("exclude pattern", pat); err != nil {
			return command{}, err
		}
		args = append(args, "--exclude="+pat)
	}
	if op.GetVerify() {
		args = append(args, "--verify")
	}
	args = append(args, "--", id)
	return command{repo: op.GetRepository(), verb: "restore", args: args}, nil
}

func planSnapshots(_ *engine, spec *syncrv1.JobSpec) (command, error) {
	op := spec.GetResticSnapshots()
	if op == nil {
		return command{}, noOperation(kindSnapshots)
	}
	// Separate --tag flags match snapshots that have any of the tags.
	args, err := tagFlags("--tag=", op.GetTags())
	if err != nil {
		return command{}, err
	}
	if h := op.GetHost(); h != "" {
		if err := checkText("host", h); err != nil {
			return command{}, err
		}
		args = append(args, "--host="+h)
	}
	return command{repo: op.GetRepository(), verb: "snapshots", args: args}, nil
}

func noOperation(kind string) error {
	return invalid("job spec has no %s operation", kind)
}

// tagFlags returns one prefix+tag flag per tag. restic splits a tag value at
// commas (meaning "all of"), so tags containing one are rejected.
func tagFlags(prefix string, tags []string) ([]string, error) {
	flags := make([]string, 0, len(tags))
	for _, t := range tags {
		if err := checkText("tag", t); err != nil {
			return nil, err
		}
		if strings.Contains(t, ",") {
			return nil, invalid("tag %q must not contain a comma", t)
		}
		flags = append(flags, prefix+t)
	}
	return flags, nil
}

// checkText rejects empty values and control characters (which includes NUL
// and newlines) in values that are passed as flag values.
func checkText(what, s string) error {
	if s == "" {
		return invalid("%s must not be empty", what)
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return invalid("%s contains a control character", what)
		}
	}
	return nil
}

// resolve passes a local path through the path policy and checks the result
// is usable as a path argument.
func (e *engine) resolve(what, path string) (string, error) {
	if path == "" {
		return "", invalid("%s must not be empty", what)
	}
	resolved, err := e.cfg.Paths.Resolve(path)
	if err != nil {
		return "", runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED, "%s %q: %w", what, path, err)
	}
	if !filepath.IsAbs(resolved) {
		return "", runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED,
			"%s %q: path policy returned a non-absolute path", what, path)
	}
	return resolved, nil
}

// repository returns the value of --repo for loc and whether it is served
// by rclone.
func (e *engine) repository(spec *syncrv1.JobSpec, repo *syncrv1.ResticRepository) (value string, viaRclone bool, err error) {
	if repo == nil || repo.GetLocation() == nil {
		return "", false, invalid("repository location is missing")
	}
	if repo.GetPassword() == "" {
		return "", false, invalid("repository password is missing")
	}
	loc := repo.GetLocation()
	if loc.GetRemote() == "" {
		path, err := e.resolve("repository path", loc.GetPath())
		if err != nil {
			return "", false, err
		}
		return path, false, nil
	}
	alias := loc.GetRemote()
	if !aliasPattern.MatchString(alias) {
		return "", false, invalid("invalid remote alias %q", alias)
	}
	if !hasRemote(spec, alias) {
		return "", false, invalid("repository remote %q is not defined in the job", alias)
	}
	if err := checkRemotePath(loc.GetPath()); err != nil {
		return "", false, err
	}
	return "rclone:" + alias + ":" + loc.GetPath(), true, nil
}

func hasRemote(spec *syncrv1.JobSpec, alias string) bool {
	for _, r := range spec.GetRemotes() {
		if r.GetAlias() == alias {
			return true
		}
	}
	return false
}

// checkRemotePath rejects paths that cannot be part of a repository string.
func checkRemotePath(path string) error {
	for _, r := range path {
		if unicode.IsControl(r) {
			return invalid("repository path contains a control character")
		}
	}
	return nil
}

func (c Config) validate() error {
	var errs []error
	for _, f := range []struct{ name, val string }{
		{"ResticPath", c.ResticPath},
		{"RclonePath", c.RclonePath},
		{"ScratchRoot", c.ScratchRoot},
	} {
		if f.val == "" {
			errs = append(errs, fmt.Errorf("restic: Config.%s is required", f.name))
		}
	}
	if c.Supervisor == nil {
		errs = append(errs, errors.New("restic: Config.Supervisor is required"))
	}
	if c.Remotes == nil {
		errs = append(errs, errors.New("restic: Config.Remotes is required"))
	}
	if c.Paths == nil {
		errs = append(errs, errors.New("restic: Config.Paths is required"))
	}
	return errors.Join(errs...)
}
