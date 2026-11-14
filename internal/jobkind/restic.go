package jobkind

import (
	"fmt"
	pathpkg "path"
	"regexp"
	"strings"
	"unicode"

	"google.golang.org/protobuf/proto"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/link"
)

// ResticRepoKeyPrefix starts the concurrency key of a restic repository:
// every job on the repository holds ResticRepoKey(id), so the jobs of one
// repository run one at a time, oldest first.
const ResticRepoKeyPrefix = "restic-repo:"

// ResticRepoKey returns the concurrency key of jobs on repository id
// ("restic-repo:<uuid>").
func ResticRepoKey(id domain.ResticRepoID) string { return ResticRepoKeyPrefix + id.String() }

// Limits of the restic definitions. The worker's restic runner checks the
// values again; checking here gives the caller a field error instead of a
// failed job.
const (
	maxBackupPaths    = 100
	maxResticExcludes = 1000
	maxResticTags     = 32
	maxTagBytes       = 128
	maxHostBytes      = 255
	maxRestorePaths   = 1000
	maxKeep           = 100000
	maxLocalPathBytes = 4096
)

// unknownRepository names a repository Describe has no name for.
const unknownRepository = "Unknown repository"

// ResticKinds returns the restic job kinds: restic_backup, restic_forget,
// restic_prune, restic_check, restic_restore and restic_delete_data. Each
// refers to one of the owner's repositories (Refs.RepositoryID), holds its
// ResticRepoKey (Keyed) and requires the worker feature
// link.FeatureResticV2 (restic_delete_data also link.FeatureResticDelete).
// A backup with use_fs_snapshot needs a Windows worker (OSBound).
// restic_restore and restic_delete_data are Unschedulable;
// restic_delete_data is SystemOnly.
func ResticKinds() []Kind {
	return []Kind{ResticBackup{}, ResticForget{}, ResticPrune{}, ResticCheck{}, ResticRestore{}, ResticDeleteData{}}
}

// resticBase is what every restic kind shares: the required worker
// feature.
type resticBase struct{}

// RequiredFeatures implements Requirer: the Milestone 5 restic specs.
func (resticBase) RequiredFeatures() []string { return []string{link.FeatureResticV2} }

// repositoryKeys returns the concurrency key of the repository raw names;
// nil if raw is not a repository ID.
func repositoryKeys(raw string) []string {
	id, err := domain.ParseResticRepoID(raw)
	if err != nil || id.IsZero() {
		return nil
	}
	return []string{ResticRepoKey(id)}
}

// checkRepositoryID parses a definition's repository_id, recording a
// problem on field+".repository_id".
func checkRepositoryID(v *domain.Validation, field, raw string) domain.ResticRepoID {
	if raw == "" {
		v.Add(field+".repository_id", "is required")
		return domain.ResticRepoID{}
	}
	id, err := domain.ParseResticRepoID(raw)
	if err != nil || id.IsZero() {
		v.Add(field+".repository_id", "is not a valid repository ID")
		return domain.ResticRepoID{}
	}
	return id
}

// repositorySpec looks up the repository raw names in in and locates it:
// the remote definitions and the ResticRepository of the spec, and the
// resolved repository.
func repositorySpec(kind, raw string, in Resolved) ([]*syncrv1.RemoteDefinition, *syncrv1.ResticRepository, Repository, error) {
	id, err := domain.ParseResticRepoID(raw)
	if err != nil || id.IsZero() {
		return nil, nil, Repository{}, fmt.Errorf("build %s: %w: the repository ID is not valid", kind, domain.ErrInvalidArgument)
	}
	repo, ok := in.Repositories[id]
	if !ok {
		return nil, nil, Repository{}, fmt.Errorf("build %s: repository %s: %w: it was not resolved", kind, id, domain.ErrNotFound)
	}
	if repo.ID != id {
		return nil, nil, Repository{}, fmt.Errorf("build %s: repository %s: %w: resolved as repository %s", kind, id, domain.ErrInvalidArgument, repo.ID)
	}
	if repo.Password == "" {
		return nil, nil, Repository{}, fmt.Errorf("build %s: repository %s: %w: it has no password", kind, id, domain.ErrInvalidArgument)
	}
	defs, rr, err := LocateRepository(repo, in.Remotes)
	if err != nil {
		return nil, nil, Repository{}, fmt.Errorf("build %s: %w", kind, err)
	}
	return defs, rr, repo, nil
}

// repositoryName returns the display name of the repository raw names.
func repositoryName(raw string, names Names) string {
	if id, err := domain.ParseResticRepoID(raw); err == nil {
		if name := names.Repositories[id]; name != "" {
			return name
		}
	}
	return unknownRepository
}

// hasControl reports whether s contains a control character (which
// includes NUL and line breaks).
func hasControl(s string) bool { return strings.ContainsFunc(s, unicode.IsControl) }

// checkLocalPath checks an absolute path on the worker (a backup source, a
// restore target) and returns its normalized form, for comparisons and the
// spec; ok is false when a problem was recorded on field.
func checkLocalPath(v *domain.Validation, field, p string) (string, bool) {
	switch {
	case p == "":
		v.Add(field, "is required")
	case len(p) > maxLocalPathBytes:
		v.Add(field, fmt.Sprintf("must be at most %d bytes", maxLocalPathBytes))
	case hasControl(p):
		v.Add(field, "must not contain control characters")
	case !absoluteLocal(p):
		v.Add(field, `must be an absolute path on the worker, e.g. "/srv/data" or "C:\Data"`)
	default:
		// A Windows worker splits at backslashes too: reject ".." there
		// now, not when the job runs.
		if err := rejectDotDot(p, `/\`); err != nil {
			v.Add(field, pathReason(err))
			return "", false
		}
		key, err := backend.JoinLocal(p, "")
		if err != nil {
			v.Add(field, pathReason(err))
			return "", false
		}
		return key, true
	}
	return "", false
}

// checkExcludes checks --exclude patterns.
func checkExcludes(v *domain.Validation, field string, pats []string) {
	if len(pats) > maxResticExcludes {
		v.Add(field, fmt.Sprintf("must have at most %d patterns", maxResticExcludes))
		return
	}
	for i, p := range pats {
		f := fmt.Sprintf("%s[%d]", field, i)
		switch {
		case p == "":
			v.Add(f, "must not be empty")
		case len(p) > maxPatternBytes:
			v.Add(f, fmt.Sprintf("must be at most %d bytes", maxPatternBytes))
		case hasControl(p):
			v.Add(f, "must not contain control characters")
		}
	}
}

// checkTags checks restic tags (--tag, --keep-tag): restic splits a value
// at commas, so a tag cannot contain one.
func checkTags(v *domain.Validation, field string, tags []string) {
	if len(tags) > maxResticTags {
		v.Add(field, fmt.Sprintf("must have at most %d tags", maxResticTags))
		return
	}
	for i, t := range tags {
		f := fmt.Sprintf("%s[%d]", field, i)
		switch {
		case t == "":
			v.Add(f, "must not be empty")
		case len(t) > maxTagBytes:
			v.Add(f, fmt.Sprintf("must be at most %d bytes", maxTagBytes))
		case strings.Contains(t, ","):
			v.Add(f, "must not contain a comma")
		case hasControl(t):
			v.Add(f, "must not contain control characters")
		}
	}
}

// checkHost checks a --host value ("" is none).
func checkHost(v *domain.Validation, field, host string) {
	switch {
	case len(host) > maxHostBytes:
		v.Add(field, fmt.Sprintf("must be at most %d bytes", maxHostBytes))
	case hasControl(host):
		v.Add(field, "must not contain control characters")
	}
}

// treePathProblem returns what is wrong with a snapshot tree path ("" if
// nothing): it starts with "/", always uses forward slashes, and has no
// ".." segment or control character.
func treePathProblem(p string) string {
	switch {
	case !strings.HasPrefix(p, "/"):
		return `must be a snapshot path starting with "/"`
	case len(p) > maxLocalPathBytes:
		return fmt.Sprintf("must be at most %d bytes", maxLocalPathBytes)
	case hasControl(p):
		return "must not contain control characters"
	}
	for seg := range strings.SplitSeq(p, "/") {
		if seg == ".." {
			return `must not contain ".." segments`
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Backup
// ---------------------------------------------------------------------------

// backupField is the field path of a backup in a definition.
const backupField = "definition.restic_backup"

// ResticBackup is the "restic_backup" kind: back up local paths of the
// worker into one of the owner's repositories.
type ResticBackup struct{ resticBase }

var (
	_ Kind       = ResticBackup{}
	_ Keyed      = ResticBackup{}
	_ Requirer   = ResticBackup{}
	_ OSBound    = ResticBackup{}
	_ Labeled    = ResticBackup{}
	_ Summarizer = ResticBackup{}
)

// Name implements Kind.
func (ResticBackup) Name() string { return "restic_backup" }

// RequiredOS implements OSBound: use_fs_snapshot (VSS) needs a Windows
// worker; a worker on another system refuses the spec (INVALID_SPEC).
func (ResticBackup) RequiredOS(def *syncrv1.JobDefinition) OSRequirement {
	if !def.GetResticBackup().GetUseFsSnapshot() {
		return OSRequirement{}
	}
	return OSRequirement{OS: "windows", Field: backupField + ".use_fs_snapshot", What: "Windows VSS (use_fs_snapshot)"}
}

// Label implements Labeled.
func (ResticBackup) Label() string { return "Backup" }

// ConcurrencyKeys implements Keyed: the repository's key.
func (ResticBackup) ConcurrencyKeys(def *syncrv1.JobDefinition) []string {
	return repositoryKeys(def.GetResticBackup().GetRepositoryId())
}

// Check implements Kind: the repository ID, 1 to 100 absolute local paths
// without ".." segments or duplicates, the exclude patterns, the tags, the
// host and exclude_larger_than_bytes. The search terms are the paths, the
// tags and the host.
func (ResticBackup) Check(def *syncrv1.JobDefinition) (Refs, error) {
	op := def.GetResticBackup()
	var v domain.Validation
	if op == nil {
		v.Add(backupField, "is required")
		return Refs{}, v.Err()
	}
	refs := Refs{RepositoryID: checkRepositoryID(&v, backupField, op.GetRepositoryId())}
	// Backups of a remote are not built yet: refuse the field rather than
	// drop it and back up the paths alone.
	if op.GetRemoteSource() != nil {
		v.Add(backupField+".remote_source", "is not supported yet")
	}
	switch n := len(op.GetPaths()); {
	case n == 0:
		v.Add(backupField+".paths", "needs at least one path")
	case n > maxBackupPaths:
		v.Add(backupField+".paths", fmt.Sprintf("must have at most %d paths", maxBackupPaths))
	default:
		seen := make(map[string]bool, n)
		for i, p := range op.GetPaths() {
			f := fmt.Sprintf("%s.paths[%d]", backupField, i)
			key, ok := checkLocalPath(&v, f, p)
			if !ok {
				continue
			}
			if seen[key] {
				v.Add(f, "is listed twice")
			}
			seen[key] = true
		}
	}
	checkExcludes(&v, backupField+".excludes", op.GetExcludes())
	checkTags(&v, backupField+".tags", op.GetTags())
	checkHost(&v, backupField+".host", op.GetHost())
	if op.ExcludeLargerThanBytes != nil && op.GetExcludeLargerThanBytes() <= 0 {
		v.Add(backupField+".exclude_larger_than_bytes", "must be greater than 0")
	}
	if err := v.Err(); err != nil {
		return Refs{}, err
	}
	refs.SearchTerms = append(refs.SearchTerms, op.GetPaths()...)
	refs.SearchTerms = append(refs.SearchTerms, op.GetTags()...)
	if h := op.GetHost(); h != "" {
		refs.SearchTerms = append(refs.SearchTerms, h)
	}
	return refs, nil
}

// Build implements Kind: the backup of the definition's paths (normalized
// with backend.JoinLocal) into the located repository.
func (k ResticBackup) Build(def *syncrv1.JobDefinition, in Resolved) (*syncrv1.JobSpec, error) {
	if _, err := k.Check(def); err != nil {
		return nil, err
	}
	d := def.GetResticBackup()
	defs, repo, _, err := repositorySpec(k.Name(), d.GetRepositoryId(), in)
	if err != nil {
		return nil, err
	}
	op := &syncrv1.ResticBackup{
		Repository:    repo,
		Excludes:      append([]string(nil), d.GetExcludes()...),
		Tags:          append([]string(nil), d.GetTags()...),
		Host:          d.GetHost(),
		UseFsSnapshot: d.GetUseFsSnapshot(),
		ExcludeCaches: d.GetExcludeCaches(),
		OneFileSystem: d.GetOneFileSystem(),
		DryRun:        d.GetDryRun(),
	}
	if d.ExcludeLargerThanBytes != nil {
		op.ExcludeLargerThanBytes = proto.Int64(d.GetExcludeLargerThanBytes())
	}
	for _, p := range d.GetPaths() {
		local, err := backend.JoinLocal(p, "")
		if err != nil {
			return nil, fmt.Errorf("build %s: %w: a backup path is not valid", k.Name(), domain.ErrInvalidArgument)
		}
		op.Paths = append(op.Paths, local)
	}
	return &syncrv1.JobSpec{Remotes: defs, Operation: &syncrv1.JobSpec_ResticBackup{ResticBackup: op}}, nil
}

// Describe implements Kind: "Back up /srv/photos → Photos vault"; two
// paths are both named, more become "/srv/a + 2 more".
func (ResticBackup) Describe(def *syncrv1.JobDefinition, names Names) string {
	op := def.GetResticBackup()
	paths := op.GetPaths()
	var what string
	switch len(paths) {
	case 0:
		what = "nothing"
	case 1:
		what = paths[0]
	case 2:
		what = paths[0] + ", " + paths[1]
	default:
		what = fmt.Sprintf("%s + %d more", paths[0], len(paths)-1)
	}
	out := "Back up " + what + " → " + repositoryName(op.GetRepositoryId(), names)
	if op.GetDryRun() {
		out += " (dry run)"
	}
	return out
}

// Summarize implements Summarizer: the snapshot, the file and folder
// counts, the data added and the size processed.
func (ResticBackup) Summarize(_ *syncrv1.JobDefinition, out *syncrv1.JobOutput) []Detail {
	o := out.GetResticBackup()
	if o == nil {
		return nil
	}
	var ds []Detail
	if id := o.GetSnapshotId(); id != "" {
		ds = append(ds, Detail{"Snapshot", shortID(id)})
	}
	ds = append(ds,
		Detail{"Files", fmt.Sprintf("%s new, %s changed, %s unmodified", formatCount(o.GetFilesNew()), formatCount(o.GetFilesChanged()), formatCount(o.GetFilesUnmodified()))},
		Detail{"Folders", fmt.Sprintf("%s new, %s changed, %s unmodified", formatCount(o.GetDirsNew()), formatCount(o.GetDirsChanged()), formatCount(o.GetDirsUnmodified()))},
	)
	added := FormatBytes(o.GetDataAddedBytes())
	if o.GetDataAddedPackedBytes() > 0 {
		added += " (" + FormatBytes(o.GetDataAddedPackedBytes()) + " stored)"
	}
	ds = append(ds,
		Detail{"Data added", added},
		Detail{"Processed", FormatBytes(o.GetTotalBytesProcessed()) + " in " + countOf(o.GetTotalFilesProcessed(), "file")},
	)
	return ds
}

// ---------------------------------------------------------------------------
// Forget (retention)
// ---------------------------------------------------------------------------

// forgetField is the field path of a retention run in a definition.
const forgetField = "definition.restic_forget"

// ResticForget is the "restic_forget" kind: apply a retention policy to a
// repository's snapshots, optionally pruning afterwards.
type ResticForget struct{ resticBase }

var (
	_ Kind       = ResticForget{}
	_ Keyed      = ResticForget{}
	_ Requirer   = ResticForget{}
	_ Labeled    = ResticForget{}
	_ Summarizer = ResticForget{}
)

// Name implements Kind.
func (ResticForget) Name() string { return "restic_forget" }

// Label implements Labeled.
func (ResticForget) Label() string { return "Retention" }

// ConcurrencyKeys implements Keyed: the repository's key.
func (ResticForget) ConcurrencyKeys(def *syncrv1.JobDefinition) []string {
	return repositoryKeys(def.GetResticForget().GetRepositoryId())
}

// keepWithinPattern matches restic's duration syntax of --keep-within,
// e.g. "1y6m" or "14d".
var keepWithinPattern = regexp.MustCompile(`^([1-9][0-9]*[ymdh])+$`)

// keepRule is one keep_* count of a policy.
type keepRule struct {
	field, word string
	n           int32
}

// keepRules returns the counts of pol, in display order.
func keepRules(pol *syncrv1.ResticRetentionPolicy) []keepRule {
	return []keepRule{
		{"keep_last", "last", pol.GetKeepLast()},
		{"keep_hourly", "hourly", pol.GetKeepHourly()},
		{"keep_daily", "daily", pol.GetKeepDaily()},
		{"keep_weekly", "weekly", pol.GetKeepWeekly()},
		{"keep_monthly", "monthly", pol.GetKeepMonthly()},
		{"keep_yearly", "yearly", pol.GetKeepYearly()},
	}
}

// Check implements Kind: the repository ID, a policy with at least one
// rule (a keep count between 1 and 100000, keep_tags or keep_within), and
// the tag and host filters. A forget without a rule must never be able to
// remove every snapshot.
func (ResticForget) Check(def *syncrv1.JobDefinition) (Refs, error) {
	op := def.GetResticForget()
	var v domain.Validation
	if op == nil {
		v.Add(forgetField, "is required")
		return Refs{}, v.Err()
	}
	refs := Refs{RepositoryID: checkRepositoryID(&v, forgetField, op.GetRepositoryId())}
	pol := op.GetPolicy()
	rules := 0
	for _, r := range keepRules(pol) {
		switch {
		case r.n < 0 || r.n > maxKeep:
			v.Add(forgetField+".policy."+r.field, fmt.Sprintf("must be between 0 and %d", maxKeep))
		case r.n > 0:
			rules++
		}
	}
	checkTags(&v, forgetField+".policy.keep_tags", pol.GetKeepTags())
	rules += len(pol.GetKeepTags())
	if w := pol.GetKeepWithin(); w != "" {
		if !keepWithinPattern.MatchString(w) {
			v.Add(forgetField+".policy.keep_within", `must be a duration such as "1y6m" or "14d"`)
		}
		rules++
	}
	if rules == 0 {
		v.Add(forgetField+".policy", "needs at least one rule saying which snapshots to keep")
	}
	checkTags(&v, forgetField+".tags", op.GetTags())
	checkHost(&v, forgetField+".host", op.GetHost())
	if err := v.Err(); err != nil {
		return Refs{}, err
	}
	refs.SearchTerms = append(refs.SearchTerms, "retention")
	refs.SearchTerms = append(refs.SearchTerms, op.GetTags()...)
	if h := op.GetHost(); h != "" {
		refs.SearchTerms = append(refs.SearchTerms, h)
	}
	return refs, nil
}

// Build implements Kind.
func (k ResticForget) Build(def *syncrv1.JobDefinition, in Resolved) (*syncrv1.JobSpec, error) {
	if _, err := k.Check(def); err != nil {
		return nil, err
	}
	d := def.GetResticForget()
	defs, repo, _, err := repositorySpec(k.Name(), d.GetRepositoryId(), in)
	if err != nil {
		return nil, err
	}
	op := &syncrv1.ResticForget{
		Repository: repo,
		Policy:     proto.CloneOf(d.GetPolicy()),
		Prune:      d.GetPrune(),
		Tags:       append([]string(nil), d.GetTags()...),
		Host:       d.GetHost(),
		DryRun:     d.GetDryRun(),
	}
	return &syncrv1.JobSpec{Remotes: defs, Operation: &syncrv1.JobSpec_ResticForget{ResticForget: op}}, nil
}

// Describe implements Kind: "Retention on Photos vault: 7 daily, 4 weekly,
// 12 monthly, then prune".
func (ResticForget) Describe(def *syncrv1.JobDefinition, names Names) string {
	op := def.GetResticForget()
	pol := op.GetPolicy()
	var rules []string
	for _, r := range keepRules(pol) {
		if r.n > 0 {
			rules = append(rules, fmt.Sprintf("%d %s", r.n, r.word))
		}
	}
	if w := pol.GetKeepWithin(); w != "" {
		rules = append(rules, "within "+w)
	}
	if tags := pol.GetKeepTags(); len(tags) > 0 {
		rules = append(rules, "tags "+strings.Join(tags, ","))
	}
	out := "Retention on " + repositoryName(op.GetRepositoryId(), names)
	if len(rules) > 0 {
		out += ": " + strings.Join(rules, ", ")
	}
	if op.GetPrune() {
		out += ", then prune"
	}
	if op.GetDryRun() {
		out += " (dry run)"
	}
	return out
}

// Summarize implements Summarizer: the snapshots removed (or, for a dry
// run, that would be) and kept.
func (ResticForget) Summarize(def *syncrv1.JobDefinition, out *syncrv1.JobOutput) []Detail {
	o := out.GetResticForget()
	if o == nil {
		return nil
	}
	label := "Removed"
	if def.GetResticForget().GetDryRun() {
		label = "Would remove"
	}
	return []Detail{
		{label, countOf(int64(len(o.GetRemovedSnapshotIds())), "snapshot")},
		{"Kept", countOf(int64(o.GetKeptCount()), "snapshot")},
	}
}

// ---------------------------------------------------------------------------
// Prune
// ---------------------------------------------------------------------------

// pruneField is the field path of a prune in a definition.
const pruneField = "definition.restic_prune"

// ResticPrune is the "restic_prune" kind: remove unreferenced data from a
// repository.
type ResticPrune struct{ resticBase }

var (
	_ Kind     = ResticPrune{}
	_ Keyed    = ResticPrune{}
	_ Requirer = ResticPrune{}
	_ Labeled  = ResticPrune{}
)

// Name implements Kind.
func (ResticPrune) Name() string { return "restic_prune" }

// Label implements Labeled.
func (ResticPrune) Label() string { return "Prune" }

// ConcurrencyKeys implements Keyed: the repository's key.
func (ResticPrune) ConcurrencyKeys(def *syncrv1.JobDefinition) []string {
	return repositoryKeys(def.GetResticPrune().GetRepositoryId())
}

// Check implements Kind: the repository ID.
func (ResticPrune) Check(def *syncrv1.JobDefinition) (Refs, error) {
	op := def.GetResticPrune()
	var v domain.Validation
	if op == nil {
		v.Add(pruneField, "is required")
		return Refs{}, v.Err()
	}
	refs := Refs{RepositoryID: checkRepositoryID(&v, pruneField, op.GetRepositoryId()), SearchTerms: []string{"prune"}}
	if err := v.Err(); err != nil {
		return Refs{}, err
	}
	return refs, nil
}

// Build implements Kind.
func (k ResticPrune) Build(def *syncrv1.JobDefinition, in Resolved) (*syncrv1.JobSpec, error) {
	if _, err := k.Check(def); err != nil {
		return nil, err
	}
	d := def.GetResticPrune()
	defs, repo, _, err := repositorySpec(k.Name(), d.GetRepositoryId(), in)
	if err != nil {
		return nil, err
	}
	op := &syncrv1.ResticPrune{Repository: repo, DryRun: d.GetDryRun()}
	return &syncrv1.JobSpec{Remotes: defs, Operation: &syncrv1.JobSpec_ResticPrune{ResticPrune: op}}, nil
}

// Describe implements Kind: "Prune Photos vault".
func (ResticPrune) Describe(def *syncrv1.JobDefinition, names Names) string {
	op := def.GetResticPrune()
	out := "Prune " + repositoryName(op.GetRepositoryId(), names)
	if op.GetDryRun() {
		out += " (dry run)"
	}
	return out
}

// ---------------------------------------------------------------------------
// Check
// ---------------------------------------------------------------------------

// checkField is the field path of a check in a definition.
const checkField = "definition.restic_check"

// ResticCheck is the "restic_check" kind: verify a repository's integrity,
// optionally reading a share of its data.
type ResticCheck struct{ resticBase }

var (
	_ Kind       = ResticCheck{}
	_ Keyed      = ResticCheck{}
	_ Requirer   = ResticCheck{}
	_ Labeled    = ResticCheck{}
	_ Summarizer = ResticCheck{}
)

// Name implements Kind.
func (ResticCheck) Name() string { return "restic_check" }

// Label implements Labeled.
func (ResticCheck) Label() string { return "Check" }

// ConcurrencyKeys implements Keyed: the repository's key.
func (ResticCheck) ConcurrencyKeys(def *syncrv1.JobDefinition) []string {
	return repositoryKeys(def.GetResticCheck().GetRepositoryId())
}

// Check implements Kind: the repository ID and read_data_percent (0 to
// 100).
func (ResticCheck) Check(def *syncrv1.JobDefinition) (Refs, error) {
	op := def.GetResticCheck()
	var v domain.Validation
	if op == nil {
		v.Add(checkField, "is required")
		return Refs{}, v.Err()
	}
	refs := Refs{RepositoryID: checkRepositoryID(&v, checkField, op.GetRepositoryId()), SearchTerms: []string{"check"}}
	if p := op.GetReadDataPercent(); p < 0 || p > 100 {
		v.Add(checkField+".read_data_percent", "must be between 0 and 100")
	}
	if err := v.Err(); err != nil {
		return Refs{}, err
	}
	return refs, nil
}

// Build implements Kind.
func (k ResticCheck) Build(def *syncrv1.JobDefinition, in Resolved) (*syncrv1.JobSpec, error) {
	if _, err := k.Check(def); err != nil {
		return nil, err
	}
	d := def.GetResticCheck()
	defs, repo, _, err := repositorySpec(k.Name(), d.GetRepositoryId(), in)
	if err != nil {
		return nil, err
	}
	op := &syncrv1.ResticCheck{Repository: repo, ReadDataPercent: d.GetReadDataPercent()}
	return &syncrv1.JobSpec{Remotes: defs, Operation: &syncrv1.JobSpec_ResticCheck{ResticCheck: op}}, nil
}

// Describe implements Kind: "Check Photos vault", plus " (read 10% of the
// data)" or " (read all data)".
func (ResticCheck) Describe(def *syncrv1.JobDefinition, names Names) string {
	op := def.GetResticCheck()
	out := "Check " + repositoryName(op.GetRepositoryId(), names)
	switch p := op.GetReadDataPercent(); {
	case p >= 100:
		out += " (read all data)"
	case p > 0:
		out += fmt.Sprintf(" (read %d%% of the data)", p)
	}
	return out
}

// Summarize implements Summarizer: the result, the share of data read and
// restic's hints.
func (ResticCheck) Summarize(_ *syncrv1.JobDefinition, out *syncrv1.JobOutput) []Detail {
	o := out.GetResticCheck()
	if o == nil {
		return nil
	}
	result := "No errors found"
	if n := o.GetNumErrors(); n > 0 {
		result = countOf(n, "error")
	}
	ds := []Detail{{"Result", result}}
	if p := o.GetReadDataPercent(); p > 0 {
		ds = append(ds, Detail{"Data read", fmt.Sprintf("%d%%", p)})
	}
	if n := len(o.GetBrokenPacks()); n > 0 {
		ds = append(ds, Detail{"Broken packs", formatCount(int64(n))})
	}
	if o.GetSuggestRepairIndex() {
		ds = append(ds, Detail{"Hint", `Run "restic repair index"`})
	}
	if o.GetSuggestPrune() {
		ds = append(ds, Detail{"Hint", "Prune suggested"})
	}
	return ds
}

// ---------------------------------------------------------------------------
// Restore
// ---------------------------------------------------------------------------

// restoreField is the field path of a restore in a definition.
const restoreField = "definition.restic_restore"

// ResticRestore is the "restic_restore" kind: restore a snapshot, selected
// items of it or the contents of one of its folders into a local folder
// of the worker. Existing files are never replaced unless the definition
// asks for it.
type ResticRestore struct{ resticBase }

var (
	_ Kind               = ResticRestore{}
	_ Keyed              = ResticRestore{}
	_ Requirer           = ResticRestore{}
	_ DefinitionRequirer = ResticRestore{}
	_ Labeled            = ResticRestore{}
	_ Unschedulable      = ResticRestore{}
	_ Summarizer         = ResticRestore{}
)

// Name implements Kind.
func (ResticRestore) Name() string { return "restic_restore" }

// Label implements Labeled.
func (ResticRestore) Label() string { return "Restore" }

// Unschedulable implements Unschedulable.
func (ResticRestore) Unschedulable() string { return "restores cannot be scheduled" }

// ConcurrencyKeys implements Keyed: the repository's key.
func (ResticRestore) ConcurrencyKeys(def *syncrv1.JobDefinition) []string {
	return repositoryKeys(def.GetResticRestore().GetRepositoryId())
}

// snapshotIDPattern matches a snapshot ID as the API accepts it: 8 to 64
// hex digits.
var snapshotIDPattern = regexp.MustCompile(`^[0-9a-f]{8,64}$`)

// ValidSnapshotID reports whether s is a snapshot ID of 8 to 64 lower-case
// hex digits.
func ValidSnapshotID(s string) bool { return snapshotIDPattern.MatchString(s) }

// DefinitionFeatures implements DefinitionRequirer: selected items of a
// subfolder (subfolder together with paths) need a worker that restores
// them relative to it (Milestone 7); an older one refuses the combination.
func (ResticRestore) DefinitionFeatures(def *syncrv1.JobDefinition) []string {
	op := def.GetResticRestore()
	if op.GetSubfolder() != "" && len(op.GetPaths()) > 0 {
		return []string{link.FeatureResticRestoreSubfolderPaths}
	}
	return nil
}

// insideTree reports whether the snapshot tree path p lies strictly inside
// the folder sub (both absolute, with forward slashes).
func insideTree(p, sub string) bool {
	p, sub = pathpkg.Clean(p), pathpkg.Clean(sub)
	if sub == "/" {
		return p != "/"
	}
	return strings.HasPrefix(p, sub+"/")
}

// Check implements Kind: the repository ID, the snapshot ("latest" or 8
// to 64 hex digits), the absolute local target, at most 1000 selected
// tree paths, one subfolder, or selected paths inside one subfolder (they
// then land in the target as they were in the subfolder), the exclude
// patterns and the overwrite behavior.
func (ResticRestore) Check(def *syncrv1.JobDefinition) (Refs, error) {
	op := def.GetResticRestore()
	var v domain.Validation
	if op == nil {
		v.Add(restoreField, "is required")
		return Refs{}, v.Err()
	}
	refs := Refs{RepositoryID: checkRepositoryID(&v, restoreField, op.GetRepositoryId())}
	if id := op.GetSnapshotId(); id != "latest" && !ValidSnapshotID(id) {
		v.Add(restoreField+".snapshot_id", `must be "latest" or a snapshot ID of 8 to 64 hex digits`)
	}
	checkLocalPath(&v, restoreField+".target", op.GetTarget())
	switch paths := op.GetPaths(); {
	case len(paths) > maxRestorePaths:
		v.Add(restoreField+".paths", fmt.Sprintf("must have at most %d items", maxRestorePaths))
	default:
		sub := op.GetSubfolder()
		subOK := sub == "" || treePathProblem(sub) == ""
		for i, p := range paths {
			if msg := treePathProblem(p); msg != "" {
				v.Add(fmt.Sprintf("%s.paths[%d]", restoreField, i), msg)
			} else if sub != "" && subOK && !insideTree(p, sub) {
				v.Add(fmt.Sprintf("%s.paths[%d]", restoreField, i), "must be inside the subfolder")
			}
		}
	}
	if sub := op.GetSubfolder(); sub != "" {
		if msg := treePathProblem(sub); msg != "" {
			v.Add(restoreField+".subfolder", msg)
		}
	}
	checkExcludes(&v, restoreField+".excludes", op.GetExcludes())
	if _, ok := syncrv1.ResticRestoreOverwrite_name[int32(op.GetOverwrite())]; !ok {
		v.Add(restoreField+".overwrite", "is not a known overwrite behavior")
	}
	if err := v.Err(); err != nil {
		return Refs{}, err
	}
	refs.SearchTerms = []string{"restore", op.GetSnapshotId(), op.GetTarget()}
	if sub := op.GetSubfolder(); sub != "" {
		refs.SearchTerms = append(refs.SearchTerms, sub)
	}
	return refs, nil
}

// Build implements Kind. The selected paths stay literal tree paths: the
// worker turns them into include patterns for its operating system. An
// unspecified overwrite behavior is sent as NEVER, so a restore never
// replaces files unless asked to.
func (k ResticRestore) Build(def *syncrv1.JobDefinition, in Resolved) (*syncrv1.JobSpec, error) {
	if _, err := k.Check(def); err != nil {
		return nil, err
	}
	d := def.GetResticRestore()
	defs, repo, _, err := repositorySpec(k.Name(), d.GetRepositoryId(), in)
	if err != nil {
		return nil, err
	}
	target, err := backend.JoinLocal(d.GetTarget(), "")
	if err != nil {
		return nil, fmt.Errorf("build %s: %w: the target is not valid", k.Name(), domain.ErrInvalidArgument)
	}
	overwrite := d.GetOverwrite()
	if overwrite == syncrv1.ResticRestoreOverwrite_RESTIC_RESTORE_OVERWRITE_UNSPECIFIED {
		overwrite = syncrv1.ResticRestoreOverwrite_RESTIC_RESTORE_OVERWRITE_NEVER
	}
	op := &syncrv1.ResticRestore{
		Repository: repo,
		SnapshotId: d.GetSnapshotId(),
		Target:     target,
		Paths:      append([]string(nil), d.GetPaths()...),
		Subfolder:  d.GetSubfolder(),
		Excludes:   append([]string(nil), d.GetExcludes()...),
		Verify:     d.GetVerify(),
		Overwrite:  overwrite,
	}
	return &syncrv1.JobSpec{Remotes: defs, Operation: &syncrv1.JobSpec_ResticRestore{ResticRestore: op}}, nil
}

// Describe implements Kind, in four forms: "Restore 1a2b3c4d from Photos
// vault → /srv/restore" (the whole snapshot), "Restore 3 items from Photos
// vault @ 1a2b3c4d → /srv/restore" (selected items), "Restore 2 items of
// /srv/photos from Photos vault @ 1a2b3c4d → /srv/photos" (selected items
// of a subfolder) and "Restore /srv/photos/2025 from Photos vault @
// 1a2b3c4d → /srv/restore" (one folder, or one selected item).
func (ResticRestore) Describe(def *syncrv1.JobDefinition, names Names) string {
	op := def.GetResticRestore()
	repo := repositoryName(op.GetRepositoryId(), names)
	snap := op.GetSnapshotId()
	if snap != "latest" {
		snap = shortID(snap)
	}
	target := op.GetTarget()
	what := ""
	switch paths := op.GetPaths(); {
	case op.GetSubfolder() != "" && len(paths) > 1:
		what = fmt.Sprintf("%d items of %s", len(paths), op.GetSubfolder())
	case op.GetSubfolder() != "" && len(paths) == 0:
		what = op.GetSubfolder()
	case len(paths) == 1:
		what = paths[0]
	case len(paths) > 1:
		what = fmt.Sprintf("%d items", len(paths))
	}
	if what == "" {
		return "Restore " + snap + " from " + repo + " → " + target
	}
	return "Restore " + what + " from " + repo + " @ " + snap + " → " + target
}

// Summarize implements Summarizer: what was restored, skipped and deleted.
// restic counts folders along with files.
func (ResticRestore) Summarize(_ *syncrv1.JobDefinition, out *syncrv1.JobOutput) []Detail {
	o := out.GetResticRestore()
	if o == nil {
		return nil
	}
	ds := []Detail{
		{"Restored", fmt.Sprintf("%s (%s)", countOf(o.GetFilesRestored(), "file or folder"), FormatBytes(o.GetBytesRestored()))},
		{"Skipped", fmt.Sprintf("%s (%s)", countOf(o.GetFilesSkipped(), "file or folder"), FormatBytes(o.GetBytesSkipped()))},
	}
	if n := o.GetFilesDeleted(); n > 0 {
		ds = append(ds, Detail{"Deleted", formatCount(n)})
	}
	return ds
}

// ---------------------------------------------------------------------------
// Windows path forms
// ---------------------------------------------------------------------------

// TreeToLocal converts a snapshot tree path into the local path it was
// backed up from on a worker running goos ("windows", or anything else for
// POSIX systems), for "restore to the original location". On Windows the
// tree path "/C/Users/ada" is `C:\Users\ada`; elsewhere the tree path is
// the local path. ok is false when there is no such local path: a path
// that is not a tree path, or on Windows one that does not start with a
// drive letter (the root "/", a UNC share), which the user must type.
func TreeToLocal(tree, goos string) (local string, ok bool) {
	if treePathProblem(tree) != "" {
		return "", false
	}
	if goos != "windows" {
		return tree, true
	}
	segs := strings.FieldsFunc(tree, func(r rune) bool { return r == '/' })
	if len(segs) == 0 || len(segs[0]) != 1 {
		return "", false
	}
	if c := segs[0][0] | 0x20; c < 'a' || c > 'z' {
		return "", false
	}
	for _, s := range segs[1:] {
		if strings.ContainsAny(s, `\:`) {
			return "", false
		}
	}
	return strings.ToUpper(segs[0]) + `:\` + strings.Join(segs[1:], `\`), true
}

// ---------------------------------------------------------------------------
// Formatting
// ---------------------------------------------------------------------------

// shortID returns the first 8 characters of a snapshot ID, as restic
// shows it.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// FormatBytes formats n bytes with binary units, e.g. "1.5 MiB", exactly
// as notify.FormatBytes does (this package cannot import it; a test in the
// API package pins that they agree).
func FormatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		if n == 1 {
			return "1 byte"
		}
		return fmt.Sprintf("%d bytes", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 5; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// formatCount formats n with thousands separators, e.g. "1,204".
func formatCount(n int64) string {
	s := fmt.Sprintf("%d", n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// countOf formats a count of things, e.g. "1 file", "1,204 files"; a word
// containing " or " pluralizes both parts ("files or folders").
func countOf(n int64, word string) string {
	if n != 1 {
		if a, b, ok := strings.Cut(word, " or "); ok {
			word = a + "s and " + b + "s"
		} else {
			word += "s"
		}
	}
	return formatCount(n) + " " + word
}
