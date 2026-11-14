package jobkind

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/link"
)

// Backups of a folder of a remote (ResticBackupDefinition.remote_source):
// the checks, keys, spec and texts the backup kind adds for them. A
// backup of worker paths never reaches anything here.

// RemoteSourceKeyPrefix starts the concurrency key of the folder a backup
// of a remote reads: every such job holds RemoteSourceKeyPrefix plus 40 hex
// digits of a hash of the remote's ID and the folder's path
// (ResticBackup.ConcurrencyKeys), so two backups of one folder, on any
// worker, run one at a time.
const RemoteSourceKeyPrefix = "remote-source:"

// Versions of the hashed encodings of a remote source. A new encoding
// takes a new version, so that it can never produce an older encoding's
// key for another input.
const (
	remoteSourceKeyVersion      = "syncr-remote-source-v1"
	remoteSourceLocationVersion = "syncr-remote-source-location-v1"
)

// remoteSourceField is the field path of a backup's remote source in a
// definition.
const remoteSourceField = backupField + ".remote_source"

// maxRemoteSourceTags is how many tags a backup of a remote may set: the
// controller adds link.RemoteSourceTag to restic's limit of 32.
const maxRemoteSourceTags = maxResticTags - 1

// Messages of the checks of a backup of a remote, for people.
const (
	// RemoteSourceSingleWorkerReason is why a schedule of a backup of a
	// remote cannot target a label (on "target.worker_label").
	RemoteSourceSingleWorkerReason = "a backup of a remote keeps its mirror or mount folder on one worker; choose a worker"
	// RemoteSourceLocalMessage is the field error of a source remote of
	// the Local directory type (on "...remote_source.remote_id").
	RemoteSourceLocalMessage = "this remote is a folder on a worker; back up that folder instead"
	// RemoteSourceIsRepository, RemoteSourceHoldsRepository and
	// RemoteSourceInsideRepository are the field errors of a source that
	// overlaps the backup's repository (on "...remote_source.path").
	RemoteSourceIsRepository     = "is the repository's folder: a backup cannot read its own repository"
	RemoteSourceHoldsRepository  = "holds the repository: a backup cannot read its own repository; choose a folder beside it"
	RemoteSourceInsideRepository = "is inside the repository: a backup cannot read its own repository"
	// NoSnapshotEmptySource is the "Snapshot" detail of a backup of a
	// remote that succeeded without a snapshot: the folder was empty and
	// allowed to be (allow_empty), and restic makes no empty snapshot.
	NoSnapshotEmptySource = "No snapshot: the remote folder was empty"
	// mirrorOnly is the field error of a mirror-only option with MOUNT.
	mirrorOnly = "is only available with the mirror method"
	// notWithRemoteSource is the field error of a backup option that a
	// backup of a remote cannot use.
	notWithRemoteSource = "cannot be used when backing up a folder of a remote"
)

// RemoteSourceOverlap returns the field error of a backup whose source
// folder srcPath and repository folder repoPath are on the same remote
// (both relative to its root, or both joined with it; "" or "/" is the
// root): RemoteSourceIsRepository, RemoteSourceHoldsRepository or
// RemoteSourceInsideRepository, or "" when neither holds the other.
// Paths compare by whole folders, ignoring leading, trailing and repeated
// "/" and "." segments ("data2" is not inside "data").
func RemoteSourceOverlap(srcPath, repoPath string) string {
	src, repo := folderSegments(srcPath), folderSegments(repoPath)
	switch {
	case slices.Equal(src, repo):
		return RemoteSourceIsRepository
	case len(src) < len(repo) && slices.Equal(repo[:len(src)], src):
		return RemoteSourceHoldsRepository
	case len(repo) < len(src) && slices.Equal(src[:len(repo)], repo):
		return RemoteSourceInsideRepository
	}
	return ""
}

// folderSegments returns the folders of a path on a remote, without empty
// and "." segments.
func folderSegments(p string) []string {
	var out []string
	for seg := range strings.SplitSeq(p, "/") {
		if seg != "" && seg != "." {
			out = append(out, seg)
		}
	}
	return out
}

// sourceLocation is the remote source of a backup as a definition
// location, for checkLocation and specRemotes.location.
func sourceLocation(src *syncrv1.ResticRemoteSourceDefinition) *syncrv1.JobLocation {
	return &syncrv1.JobLocation{RemoteId: src.GetRemoteId(), Path: src.GetPath()}
}

// isMirror reports whether m is the mirror method.
func isMirror(m syncrv1.ResticRemoteSourceMethod) bool {
	return m == syncrv1.ResticRemoteSourceMethod_RESTIC_REMOTE_SOURCE_METHOD_MIRROR
}

// methodName returns the lower-case name of a method ("mirror", "mount"),
// or "" for one that is not.
func methodName(m syncrv1.ResticRemoteSourceMethod) string {
	switch m {
	case syncrv1.ResticRemoteSourceMethod_RESTIC_REMOTE_SOURCE_METHOD_MIRROR:
		return "mirror"
	case syncrv1.ResticRemoteSourceMethod_RESTIC_REMOTE_SOURCE_METHOD_MOUNT:
		return "mount"
	}
	return ""
}

// RemoteSourceMethod returns the method of def's remote source when def is
// a backup of a folder of a remote (ResticBackupDefinition.remote_source);
// ok is false for any other definition.
func RemoteSourceMethod(def *syncrv1.JobDefinition) (m syncrv1.ResticRemoteSourceMethod, ok bool) {
	src := def.GetResticBackup().GetRemoteSource()
	if src == nil {
		return 0, false
	}
	return src.GetMethod(), true
}

// RemoteSourceOf returns the remote and the folder (relative to the
// remote's root, as given) a backup of a remote reads; ok is false for any
// other definition, or one whose remote ID is not valid. The app layer
// checks them against the repository and the remote's type
// (restic.Service.CheckRemoteSource).
func RemoteSourceOf(def *syncrv1.JobDefinition) (remote domain.RemoteID, path string, ok bool) {
	src := def.GetResticBackup().GetRemoteSource()
	if src == nil {
		return domain.RemoteID{}, "", false
	}
	id, err := domain.ParseRemoteID(src.GetRemoteId())
	if err != nil || id.IsZero() {
		return domain.RemoteID{}, "", false
	}
	return id, src.GetPath(), true
}

// RemoteSourceUnavailable says why a worker that reported support (its
// Hello.remote_sources; nil for none) cannot read a folder of a remote with
// method, for people, after the worker's name: "cannot keep a mirror of a
// remote: <its reason>" or "cannot mount a remote: <its reason>"; "" when
// it can. The API refuses such a worker for RunJob (a field error on the
// worker) and the dispatcher fails the job with tool_unavailable.
func RemoteSourceUnavailable(method syncrv1.ResticRemoteSourceMethod, support *syncrv1.RemoteSourceSupport) string {
	because := func(reason string) string {
		if reason == "" {
			return "it gave no reason"
		}
		return reason
	}
	switch method {
	case syncrv1.ResticRemoteSourceMethod_RESTIC_REMOTE_SOURCE_METHOD_MIRROR:
		if !support.GetMirror() {
			return "cannot keep a mirror of a remote: " + because(support.GetMirrorReason())
		}
	case syncrv1.ResticRemoteSourceMethod_RESTIC_REMOTE_SOURCE_METHOD_MOUNT:
		if !support.GetMount() {
			return "cannot mount a remote: " + because(support.GetMountReason())
		}
	}
	return ""
}

// checkBackupRemoteSource checks a backup of a remote (op.remote_source is
// set): the backup options it cannot use (paths, excludes,
// one_file_system, use_fs_snapshot), the folder (checkLocation, with a
// required remote_id), the method, the options (mirror-only ones with
// MOUNT, ranges, filters) and the tags (at most 31, none starting with
// link.RemoteSourceTagPrefix). It returns the checked folder.
func checkBackupRemoteSource(v *domain.Validation, op *syncrv1.ResticBackupDefinition) location {
	src := op.GetRemoteSource()
	if len(op.GetPaths()) > 0 {
		v.Add(backupField+".paths", "must be empty when backing up a folder of a remote (remote_source)")
	}
	if len(op.GetExcludes()) > 0 {
		v.Add(backupField+".excludes", notWithRemoteSource+": leave things out with its filters instead")
	}
	if op.GetOneFileSystem() {
		v.Add(backupField+".one_file_system", notWithRemoteSource)
	}
	if op.GetUseFsSnapshot() {
		v.Add(backupField+".use_fs_snapshot", notWithRemoteSource)
	}

	var loc location
	if src.GetRemoteId() == "" {
		v.Add(remoteSourceField+".remote_id", "is required")
	} else {
		loc = checkLocation(v, remoteSourceField, sourceLocation(src))
		if loc.ok && hasControl(loc.path) {
			v.Add(remoteSourceField+".path", "must not contain control characters")
			loc.ok = false
		}
	}
	method := src.GetMethod()
	if methodName(method) == "" {
		v.Add(remoteSourceField+".method", "must be mirror or mount")
	} else if !isMirror(method) {
		if src.GetDiscardMirror() {
			v.Add(remoteSourceField+".discard_mirror", mirrorOnly)
		}
		if src.Transfers != nil {
			v.Add(remoteSourceField+".transfers", mirrorOnly)
		}
		if src.GetFastList() {
			v.Add(remoteSourceField+".fast_list", mirrorOnly)
		}
	}
	if p := src.Transfers; p != nil && (*p < 1 || *p > maxTransfers) {
		v.Add(remoteSourceField+".transfers", fmt.Sprintf("must be between 1 and %d", maxTransfers))
	}
	if p := src.BandwidthLimitBytesPerSecond; p != nil && *p < 0 {
		v.Add(remoteSourceField+".bandwidth_limit_bytes_per_second", "must not be negative")
	}
	checkFilterRules(v, remoteSourceField, src.GetFilters())

	tags := op.GetTags()
	if len(tags) > maxRemoteSourceTags {
		v.Add(backupField+".tags", fmt.Sprintf("must have at most %d tags when backing up a folder of a remote (Syncr adds one)", maxRemoteSourceTags))
	} else {
		checkTags(v, backupField+".tags", tags)
		for i, t := range tags {
			if strings.HasPrefix(t, link.RemoteSourceTagPrefix) {
				v.Add(fmt.Sprintf("%s.tags[%d]", backupField, i), fmt.Sprintf("must not start with %q: Syncr adds that tag itself", link.RemoteSourceTagPrefix))
			}
		}
	}
	return loc
}

// remoteSourceLocationKey is the concurrency key of a checked source
// folder (RemoteSourceKeyPrefix): the remote ID and the path on it,
// without leading or trailing "/". It depends on nothing else (not the
// worker, the method or the remote's settings), so every backup that reads
// the folder holds it.
func remoteSourceLocationKey(l location) string {
	e := keyEncoder{}
	e.field(remoteSourceLocationVersion)
	e.field(l.remote.String())
	e.field(strings.Trim(l.key, "/"))
	sum := sha256.Sum256(e.b)
	return RemoteSourceKeyPrefix + hex.EncodeToString(sum[:])[:bisyncKeyDigits]
}

// sourceKey is the folder name the worker keeps a source under
// (ResticBackupRemoteSource.source_key): link.SourceKeyLen lower-case hex
// digits of the SHA-256 of remoteSourceKeyVersion, the source remote
// (stateRemote: its ID, type and non-secret options, and its union
// members) and the joined path on it (root and path). A rotated secret
// keeps the key, so the mirror is reused; any other change of the remote
// (a bucket in its root, an endpoint) or of the path starts a new folder.
func (s *specRemotes) sourceKey(r Remote, path string) (string, error) {
	e := &keyEncoder{}
	e.field(remoteSourceKeyVersion)
	if err := s.stateRemote(e, r); err != nil {
		return "", err
	}
	e.field(path)
	sum := sha256.Sum256(e.b)
	return hex.EncodeToString(sum[:])[:link.SourceKeyLen], nil
}

// buildRemoteSource builds the spec of a checked backup of a remote: the
// repository's remote and the source remote aliased together (the
// repository's first, so it is "r1" as for a backup of paths), the
// repository, the source with its key, and the definition's tags followed
// by link.RemoteSourceTag. It refuses (field errors, wrapping
// domain.ErrInvalidArgument) a source remote of the Local directory type
// and a source that is, holds or lies inside the repository's folder on
// the same remote.
func (k ResticBackup) buildRemoteSource(d *syncrv1.ResticBackupDefinition, refs Refs, in Resolved) (*syncrv1.JobSpec, error) {
	repo, err := resolveRepository(k.Name(), d.GetRepositoryId(), in)
	if err != nil {
		return nil, err
	}
	if len(refs.RemoteIDs) != 1 {
		return nil, fmt.Errorf("build %s: %w: a backup of a remote needs exactly one source remote", k.Name(), domain.ErrInvalidArgument)
	}
	srcID := refs.RemoteIDs[0]
	if repo.RemoteID.IsZero() {
		return nil, fmt.Errorf("build %s: repository %s: %w: no remote", k.Name(), repo.ID, domain.ErrInvalidArgument)
	}
	rs, err := newSpecRemotes([]domain.RemoteID{repo.RemoteID, srcID}, in.Remotes)
	if err != nil {
		return nil, fmt.Errorf("build %s: %w", k.Name(), err)
	}
	srcRemote, err := rs.remote(srcID)
	if err != nil {
		return nil, fmt.Errorf("build %s: %w", k.Name(), err)
	}
	if isLocal(srcRemote) {
		return nil, fmt.Errorf("build %s: %w", k.Name(), invalid(remoteSourceField+".remote_id", RemoteSourceLocalMessage))
	}
	repoLoc, err := rs.on(rs.remotes[repo.RemoteID], repo.Path)
	if err != nil {
		return nil, fmt.Errorf("build %s: repository %s: %w", k.Name(), repo.ID, err)
	}
	src := d.GetRemoteSource()
	srcLoc, err := rs.location(remoteSourceField, sourceLocation(src))
	if err != nil {
		return nil, fmt.Errorf("build %s: %w", k.Name(), err)
	}
	if repoLoc.GetRemote() == srcLoc.GetRemote() {
		if msg := RemoteSourceOverlap(srcLoc.GetPath(), repoLoc.GetPath()); msg != "" {
			return nil, fmt.Errorf("build %s: %w", k.Name(), invalid(remoteSourceField+".path", msg))
		}
	}
	key, err := rs.sourceKey(srcRemote, srcLoc.GetPath())
	if err != nil {
		return nil, fmt.Errorf("build %s: %w", k.Name(), err)
	}
	defs, err := rs.definitions()
	if err != nil {
		return nil, fmt.Errorf("build %s: %w", k.Name(), err)
	}
	rsrc := &syncrv1.ResticBackupRemoteSource{
		Remote:        srcLoc.GetRemote(),
		Path:          srcLoc.GetPath(),
		Method:        src.GetMethod(),
		SourceKey:     key,
		DiscardMirror: src.GetDiscardMirror(),
		AllowEmpty:    src.GetAllowEmpty(),
		FastList:      src.GetFastList(),
	}
	for _, f := range src.GetFilters() {
		rsrc.Filters = append(rsrc.Filters, proto.CloneOf(f))
	}
	if src.Transfers != nil {
		rsrc.Transfers = proto.Int32(src.GetTransfers())
	}
	if src.BandwidthLimitBytesPerSecond != nil {
		rsrc.BandwidthLimitBytesPerSecond = proto.Int64(src.GetBandwidthLimitBytesPerSecond())
	}
	op := &syncrv1.ResticBackup{
		Repository:    &syncrv1.ResticRepository{Location: repoLoc, Password: repo.Password},
		Tags:          append(slices.Clone(d.GetTags()), link.RemoteSourceTag(srcID)),
		Host:          d.GetHost(),
		ExcludeCaches: d.GetExcludeCaches(),
		DryRun:        d.GetDryRun(),
		RemoteSource:  rsrc,
	}
	if d.ExcludeLargerThanBytes != nil {
		op.ExcludeLargerThanBytes = proto.Int64(d.GetExcludeLargerThanBytes())
	}
	return &syncrv1.JobSpec{Remotes: defs, Operation: &syncrv1.JobSpec_ResticBackup{ResticBackup: op}}, nil
}

// describeRemoteSource names the source of a backup of a remote for
// people: the remote's name and the folder, then the method, e.g. "B2
// photos/Pictures (mirror)".
func describeRemoteSource(src *syncrv1.ResticRemoteSourceDefinition, names Names) string {
	out := describeLocation(sourceLocation(src), names.Remotes)
	if p := strings.Trim(src.GetPath(), "/"); p != "" {
		out += "/" + p
	}
	if m := methodName(src.GetMethod()); m != "" {
		out += " (" + m + ")"
	}
	return out
}

// summarizeRemoteSource returns the details of the source of a finished
// backup of a remote: how it was read and, for a mirror, what the mirror's
// refresh did ("Mirrored": "1.2 GiB, 40 files, 3 deleted") and whether the
// copy was deleted after the run.
func summarizeRemoteSource(s *syncrv1.RemoteSourceStats) []Detail {
	if s == nil {
		return nil
	}
	switch s.GetMethod() {
	case syncrv1.ResticRemoteSourceMethod_RESTIC_REMOTE_SOURCE_METHOD_MIRROR:
		mirrored := fmt.Sprintf("%s, %s, %s deleted", FormatBytes(s.GetMirroredBytes()), countOf(s.GetMirroredFiles(), "file"), formatCount(s.GetDeletedFiles()))
		if d := s.GetMirrorElapsed(); d != nil && d.CheckValid() == nil && d.AsDuration() > 0 {
			mirrored += " in " + formatDuration(d.AsDuration())
		}
		ds := []Detail{{"Read through", "Mirror"}, {"Mirrored", mirrored}}
		if s.GetMirrorDiscarded() {
			ds = append(ds, Detail{"Mirror", "Deleted after the run"})
		}
		return ds
	case syncrv1.ResticRemoteSourceMethod_RESTIC_REMOTE_SOURCE_METHOD_MOUNT:
		return []Detail{{"Read through", "Mount"}}
	}
	return nil
}
