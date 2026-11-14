package jobkind_test

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
	"github.com/Zeptiny/Syncr/internal/link"
)

const (
	mirror = syncrv1.ResticRemoteSourceMethod_RESTIC_REMOTE_SOURCE_METHOD_MIRROR
	mount  = syncrv1.ResticRemoteSourceMethod_RESTIC_REMOTE_SOURCE_METHOD_MOUNT
)

// srcBackupDef is a backup of the folder "photos" of remote rid(5)
// (mirror) into the test repository.
func srcBackupDef(mutate ...func(*syncrv1.ResticBackupDefinition)) *syncrv1.JobDefinition {
	d := &syncrv1.ResticBackupDefinition{
		RepositoryId: testRepoID,
		RemoteSource: &syncrv1.ResticRemoteSourceDefinition{RemoteId: rid(5).String(), Path: "photos", Method: mirror},
	}
	for _, m := range mutate {
		m(d)
	}
	return &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticBackup{ResticBackup: d}}
}

// srcResolved resolves the test repository at repoPath on repoRemote,
// with the remotes rs.
func srcResolved(t *testing.T, repoRemote domain.RemoteID, repoPath string, rs ...jobkind.Remote) jobkind.Resolved {
	t.Helper()
	id := repoID(t)
	return jobkind.Resolved{
		Remotes: remoteMap(rs...),
		Repositories: map[domain.ResticRepoID]jobkind.Repository{
			id: {ID: id, RemoteID: repoRemote, Path: repoPath, Password: "repo-secret", ResticID: testResticID},
		},
	}
}

func TestRemoteSourceCheckAccepts(t *testing.T) {
	t.Parallel()
	r := builtin(t)
	for _, tt := range []struct {
		name string
		def  *syncrv1.JobDefinition
	}{
		{"mirror", srcBackupDef()},
		{"mount", srcBackupDef(func(d *syncrv1.ResticBackupDefinition) { d.RemoteSource.Method = mount })},
		{"the remote's root", srcBackupDef(func(d *syncrv1.ResticBackupDefinition) { d.RemoteSource.Path = "" })},
		{"every option", srcBackupDef(func(d *syncrv1.ResticBackupDefinition) {
			d.Tags, d.Host, d.ExcludeCaches, d.ExcludeLargerThanBytes, d.DryRun = slices.Repeat([]string{"t"}, 31), "nas", true, i64(1), true
			s := d.RemoteSource
			s.Filters = []*syncrv1.FilterRule{{Action: syncrv1.FilterAction_FILTER_ACTION_EXCLUDE, Pattern: "*.tmp"}}
			s.DiscardMirror, s.AllowEmpty, s.Transfers, s.BandwidthLimitBytesPerSecond, s.FastList = true, true, i32(64), i64(0), true
		})},
		{"mount with a bandwidth limit", srcBackupDef(func(d *syncrv1.ResticBackupDefinition) {
			d.RemoteSource.Method, d.RemoteSource.BandwidthLimitBytesPerSecond, d.RemoteSource.AllowEmpty = mount, i64(1<<20), true
		})},
		// Existing backups of paths keep their tags, whatever they are.
		{"a backup of paths may keep a syncr-remote tag", backupDef(func(d *syncrv1.ResticBackupDefinition) {
			d.Tags = []string{link.RemoteSourceTag(rid(5))}
		})},
	} {
		if _, _, err := r.Check(tt.def); err != nil {
			t.Errorf("%s: Check = %v", tt.name, err)
		}
	}
}

func TestRemoteSourceCheckRejects(t *testing.T) {
	t.Parallel()
	r := builtin(t)
	const f = "definition.restic_backup."
	const sf = f + "remote_source."
	src := func(m func(*syncrv1.ResticRemoteSourceDefinition)) *syncrv1.JobDefinition {
		return srcBackupDef(func(d *syncrv1.ResticBackupDefinition) { m(d.RemoteSource) })
	}
	backup := func(m func(*syncrv1.ResticBackupDefinition)) *syncrv1.JobDefinition { return srcBackupDef(m) }
	tests := []struct {
		name, field, message string
		def                  *syncrv1.JobDefinition
	}{
		{"paths and a remote source", f + "paths", "must be empty when backing up a folder of a remote (remote_source)",
			backup(func(d *syncrv1.ResticBackupDefinition) { d.Paths = []string{"/srv"} })},
		{"excludes", f + "excludes", "cannot be used when backing up a folder of a remote: leave things out with its filters instead",
			backup(func(d *syncrv1.ResticBackupDefinition) { d.Excludes = []string{"*.tmp"} })},
		{"one_file_system", f + "one_file_system", "cannot be used when backing up a folder of a remote",
			backup(func(d *syncrv1.ResticBackupDefinition) { d.OneFileSystem = true })},
		{"use_fs_snapshot", f + "use_fs_snapshot", "cannot be used when backing up a folder of a remote",
			backup(func(d *syncrv1.ResticBackupDefinition) { d.UseFsSnapshot = true })},
		{"32 tags", f + "tags", "must have at most 31 tags when backing up a folder of a remote (Syncr adds one)",
			backup(func(d *syncrv1.ResticBackupDefinition) { d.Tags = slices.Repeat([]string{"t"}, 32) })},
		{"a tag of Syncr's", f + "tags[1]", `must not start with "syncr-remote:": Syncr adds that tag itself`,
			backup(func(d *syncrv1.ResticBackupDefinition) { d.Tags = []string{"ok", "syncr-remote:x"} })},
		{"a bad tag", f + "tags[0]", "must not contain a comma", backup(func(d *syncrv1.ResticBackupDefinition) { d.Tags = []string{"a,b"} })},
		{"bad host", f + "host", "must not contain control characters", backup(func(d *syncrv1.ResticBackupDefinition) { d.Host = "a\nb" })},
		{"zero size limit", f + "exclude_larger_than_bytes", "must be greater than 0",
			backup(func(d *syncrv1.ResticBackupDefinition) { d.ExcludeLargerThanBytes = i64(0) })},
		{"no repository", f + "repository_id", "is required", backup(func(d *syncrv1.ResticBackupDefinition) { d.RepositoryId = "" })},
		{"no remote", sf + "remote_id", "is required", src(func(s *syncrv1.ResticRemoteSourceDefinition) { s.RemoteId = "" })},
		{"bad remote", sf + "remote_id", "is not a valid remote ID", src(func(s *syncrv1.ResticRemoteSourceDefinition) { s.RemoteId = "nope" })},
		{"dot-dot path", sf + "path", `path must not contain ".." segments`, src(func(s *syncrv1.ResticRemoteSourceDefinition) { s.Path = "a/../b" })},
		{"dot-dot windows path", sf + "path", `path must not contain ".." segments`, src(func(s *syncrv1.ResticRemoteSourceDefinition) { s.Path = `a\..\b` })},
		{"line break in the path", sf + "path", "", src(func(s *syncrv1.ResticRemoteSourceDefinition) { s.Path = "a\nb" })},
		{"tab in the path", sf + "path", "must not contain control characters", src(func(s *syncrv1.ResticRemoteSourceDefinition) { s.Path = "a\tb" })},
		{"no method", sf + "method", "must be mirror or mount", src(func(s *syncrv1.ResticRemoteSourceDefinition) { s.Method = 0 })},
		{"unknown method", sf + "method", "must be mirror or mount", src(func(s *syncrv1.ResticRemoteSourceDefinition) { s.Method = 9 })},
		{"discard_mirror with mount", sf + "discard_mirror", "is only available with the mirror method",
			src(func(s *syncrv1.ResticRemoteSourceDefinition) { s.Method, s.DiscardMirror = mount, true })},
		{"transfers with mount", sf + "transfers", "is only available with the mirror method",
			src(func(s *syncrv1.ResticRemoteSourceDefinition) { s.Method, s.Transfers = mount, i32(4) })},
		{"fast_list with mount", sf + "fast_list", "is only available with the mirror method",
			src(func(s *syncrv1.ResticRemoteSourceDefinition) { s.Method, s.FastList = mount, true })},
		{"zero transfers", sf + "transfers", "must be between 1 and 64", src(func(s *syncrv1.ResticRemoteSourceDefinition) { s.Transfers = i32(0) })},
		{"too many transfers", sf + "transfers", "must be between 1 and 64", src(func(s *syncrv1.ResticRemoteSourceDefinition) { s.Transfers = i32(65) })},
		{"negative bandwidth", sf + "bandwidth_limit_bytes_per_second", "must not be negative",
			src(func(s *syncrv1.ResticRemoteSourceDefinition) { s.BandwidthLimitBytesPerSecond = i64(-1) })},
		{"filter without an action", sf + "filters[0].action", "must be include or exclude",
			src(func(s *syncrv1.ResticRemoteSourceDefinition) { s.Filters = []*syncrv1.FilterRule{{Pattern: "a"}} })},
		{"blank filter", sf + "filters[0].pattern", "is required", src(func(s *syncrv1.ResticRemoteSourceDefinition) {
			s.Filters = []*syncrv1.FilterRule{{Action: syncrv1.FilterAction_FILTER_ACTION_INCLUDE, Pattern: " "}}
		})},
		{"too many filters", sf + "filters", "must have at most 1000 rules", src(func(s *syncrv1.ResticRemoteSourceDefinition) {
			s.Filters = slices.Repeat([]*syncrv1.FilterRule{{Action: syncrv1.FilterAction_FILTER_ACTION_INCLUDE, Pattern: "a"}}, 1001)
		})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := r.Check(tt.def)
			var verr *domain.ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("Check = %v, want a validation error", err)
			}
			i := slices.IndexFunc(verr.Fields, func(fe domain.FieldError) bool { return fe.Field == tt.field })
			if i < 0 {
				t.Fatalf("Check = %v (fields %q), want a problem on %s", err, fields(err), tt.field)
			}
			if tt.message != "" && verr.Fields[i].Message != tt.message {
				t.Errorf("message = %q, want %q", verr.Fields[i].Message, tt.message)
			}
		})
	}
}

func TestRemoteSourceRefsAndKeys(t *testing.T) {
	t.Parallel()
	r := builtin(t)
	kind, refs, err := r.Check(srcBackupDef(func(d *syncrv1.ResticBackupDefinition) { d.Tags, d.Host = []string{"daily"}, "nas" }))
	if err != nil {
		t.Fatal(err)
	}
	if kind != "restic_backup" || refs.RepositoryID != repoID(t) || !slices.Equal(refs.RemoteIDs, []domain.RemoteID{rid(5)}) {
		t.Errorf("Check = %s, %+v", kind, refs)
	}
	if !slices.Equal(refs.SearchTerms, []string{"photos", "daily", "nas"}) {
		t.Errorf("SearchTerms = %q", refs.SearchTerms)
	}
	// Sorted: "remote-source:…" before "restic-repo:…".
	if len(refs.ConcurrencyKeys) != 2 || refs.ConcurrencyKeys[1] != jobkind.ResticRepoKey(repoID(t)) ||
		!strings.HasPrefix(refs.ConcurrencyKeys[0], jobkind.RemoteSourceKeyPrefix) || len(refs.ConcurrencyKeys[0]) != len(jobkind.RemoteSourceKeyPrefix)+40 {
		t.Fatalf("ConcurrencyKeys = %q", refs.ConcurrencyKeys)
	}
	srcKey := func(def *syncrv1.JobDefinition) string {
		t.Helper()
		_, refs, err := r.Check(def)
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range refs.ConcurrencyKeys {
			if strings.HasPrefix(k, jobkind.RemoteSourceKeyPrefix) {
				return k
			}
		}
		t.Fatalf("no remote-source key in %q", refs.ConcurrencyKeys)
		return ""
	}
	base := srcKey(srcBackupDef())
	// The key is the folder's: the method, the options and the repository
	// do not change it; "/" around the path does not either.
	for name, def := range map[string]*syncrv1.JobDefinition{
		"mount":              srcBackupDef(func(d *syncrv1.ResticBackupDefinition) { d.RemoteSource.Method = mount }),
		"slashes":            srcBackupDef(func(d *syncrv1.ResticBackupDefinition) { d.RemoteSource.Path = "/photos/" }),
		"options":            srcBackupDef(func(d *syncrv1.ResticBackupDefinition) { d.RemoteSource.AllowEmpty, d.DryRun = true, true }),
		"another repository": srcBackupDef(func(d *syncrv1.ResticBackupDefinition) { d.RepositoryId = domain.NewResticRepoID().String() }),
	} {
		if got := srcKey(def); got != base {
			t.Errorf("%s: key %s, want %s", name, got, base)
		}
	}
	for name, def := range map[string]*syncrv1.JobDefinition{
		"another folder": srcBackupDef(func(d *syncrv1.ResticBackupDefinition) { d.RemoteSource.Path = "photos2" }),
		"the root":       srcBackupDef(func(d *syncrv1.ResticBackupDefinition) { d.RemoteSource.Path = "" }),
		"another remote": srcBackupDef(func(d *syncrv1.ResticBackupDefinition) { d.RemoteSource.RemoteId = rid(6).String() }),
	} {
		if got := srcKey(def); got == base {
			t.Errorf("%s: same key %s", name, got)
		}
	}
	// A backup of paths keeps only its repository's key.
	_, refs, err = r.Check(backupDef())
	if err != nil || !slices.Equal(refs.ConcurrencyKeys, []string{jobkind.ResticRepoKey(repoID(t))}) || refs.RemoteIDs != nil {
		t.Errorf("backup of paths: %v, %+v", err, refs)
	}
}

func TestRemoteSourceRequirements(t *testing.T) {
	t.Parallel()
	r := builtin(t)
	if got := r.DefinitionFeatures(srcBackupDef()); !slices.Equal(got, []string{link.FeatureResticRemoteSource}) {
		t.Errorf("DefinitionFeatures(remote) = %q", got)
	}
	if got := r.DefinitionFeatures(backupDef()); got != nil {
		t.Errorf("DefinitionFeatures(paths) = %q", got)
	}
	for _, m := range []syncrv1.ResticRemoteSourceMethod{mirror, mount} {
		def := srcBackupDef(func(d *syncrv1.ResticBackupDefinition) { d.RemoteSource.Method = m })
		if ok, why := r.LabelTargetAllowed(def); ok || why != jobkind.RemoteSourceSingleWorkerReason {
			t.Errorf("LabelTargetAllowed(%v) = (%v, %q)", m, ok, why)
		}
		if got, ok := jobkind.RemoteSourceMethod(def); !ok || got != m {
			t.Errorf("RemoteSourceMethod = %v, %v", got, ok)
		}
	}
	if ok, why := r.LabelTargetAllowed(backupDef()); !ok {
		t.Errorf("LabelTargetAllowed(paths) = (false, %q)", why)
	}
	if _, ok := jobkind.RemoteSourceMethod(backupDef()); ok {
		t.Error("RemoteSourceMethod(paths) is ok")
	}
	if id, path, ok := jobkind.RemoteSourceOf(srcBackupDef()); !ok || id != rid(5) || path != "photos" {
		t.Errorf("RemoteSourceOf = %v, %q, %v", id, path, ok)
	}
	if _, _, ok := jobkind.RemoteSourceOf(backupDef()); ok {
		t.Error("RemoteSourceOf(paths) is ok")
	}
	if req := r.RequiredOS(srcBackupDef()); req.OS != "" {
		t.Errorf("RequiredOS = %+v", req)
	}
}

func TestRemoteSourceBuild(t *testing.T) {
	t.Parallel()
	r := builtin(t)
	REPO, SRC := s3Remote(rid(1), "vault"), sftpRemote(rid(5), "/home/me")
	tag := link.RemoteSourceTag(rid(5))
	in := srcResolved(t, REPO.ID, "restic/photos", REPO, SRC)
	repo := &syncrv1.ResticRepository{Location: sloc("r1", "vault/restic/photos"), Password: "repo-secret"}

	def := srcBackupDef(func(d *syncrv1.ResticBackupDefinition) {
		d.Tags, d.Host, d.ExcludeCaches, d.ExcludeLargerThanBytes, d.DryRun = []string{"daily"}, "nas", true, i64(9), true
		s := d.RemoteSource
		s.Filters = []*syncrv1.FilterRule{{Action: syncrv1.FilterAction_FILTER_ACTION_EXCLUDE, Pattern: "*.tmp"}}
		s.DiscardMirror, s.AllowEmpty, s.Transfers, s.BandwidthLimitBytesPerSecond, s.FastList = true, true, i32(8), i64(1000), true
	})
	got, err := r.Build(def, in)
	if err != nil {
		t.Fatal(err)
	}
	key := got.GetResticBackup().GetRemoteSource().GetSourceKey()
	if !link.ValidSourceKey(key) {
		t.Fatalf("source_key %q is not valid", key)
	}
	want := &syncrv1.JobSpec{Remotes: []*syncrv1.RemoteDefinition{rdef("r1", REPO), rdef("r2", SRC)}, Operation: &syncrv1.JobSpec_ResticBackup{ResticBackup: &syncrv1.ResticBackup{
		Repository: repo, Tags: []string{"daily", tag}, Host: "nas", ExcludeCaches: true, ExcludeLargerThanBytes: i64(9), DryRun: true,
		RemoteSource: &syncrv1.ResticBackupRemoteSource{
			Remote: "r2", Path: "/home/me/photos", Method: mirror, SourceKey: key,
			Filters:       []*syncrv1.FilterRule{{Action: syncrv1.FilterAction_FILTER_ACTION_EXCLUDE, Pattern: "*.tmp"}},
			DiscardMirror: true, AllowEmpty: true, Transfers: i32(8), BandwidthLimitBytesPerSecond: i64(1000), FastList: true,
		},
	}}}
	if !proto.Equal(got, want) {
		t.Errorf("Build =\n%v\nwant\n%v", got, want)
	}
	// The spec does not alias the definition.
	got.GetResticBackup().Tags[0] = "mutated"
	got.GetResticBackup().GetRemoteSource().GetFilters()[0].Pattern = "mutated"
	if def.GetResticBackup().GetTags()[0] != "daily" || def.GetResticBackup().GetRemoteSource().GetFilters()[0].GetPattern() != "*.tmp" {
		t.Error("the spec aliases the definition")
	}

	// The source on the repository's remote, beside the repository: one
	// alias for both.
	same, err := r.Build(srcBackupDef(func(d *syncrv1.ResticBackupDefinition) { d.RemoteSource.RemoteId = rid(1).String() }), in)
	if err != nil {
		t.Fatal(err)
	}
	if rs := same.GetRemotes(); len(rs) != 1 || rs[0].GetAlias() != "r1" {
		t.Errorf("remotes = %v", rs)
	}
	if s := same.GetResticBackup().GetRemoteSource(); s.GetRemote() != "r1" || s.GetPath() != "vault/photos" {
		t.Errorf("remote source = %v", s)
	}
	if tags := same.GetResticBackup().GetTags(); !slices.Equal(tags, []string{link.RemoteSourceTag(rid(1))}) {
		t.Errorf("tags = %q", tags)
	}

	// A repository on a local remote: the source is r1.
	LCL := localRemote(rid(2), "/srv/backups")
	local, err := r.Build(srcBackupDef(), srcResolved(t, LCL.ID, "restic", LCL, SRC))
	if err != nil {
		t.Fatal(err)
	}
	if s := local.GetResticBackup(); s.GetRepository().GetLocation().GetPath() != "/srv/backups/restic" || s.GetRemoteSource().GetRemote() != "r1" {
		t.Errorf("spec = %v", local)
	}

	// The mount method, with nothing else set.
	m, err := r.Build(srcBackupDef(func(d *syncrv1.ResticBackupDefinition) { d.RemoteSource.Method = mount }), in)
	if err != nil {
		t.Fatal(err)
	}
	if s := m.GetResticBackup().GetRemoteSource(); s.GetMethod() != mount || s.GetSourceKey() != key || s.Transfers != nil || s.BandwidthLimitBytesPerSecond != nil {
		t.Errorf("mount source = %v (the key should be the mirror's, %s)", s, key)
	}
}

// TestRemoteSourceKey: the source key follows the remote's non-secret
// settings and the path, not its secrets.
func TestRemoteSourceKey(t *testing.T) {
	t.Parallel()
	r := builtin(t)
	REPO := s3Remote(rid(1), "vault")
	keyOf := func(src jobkind.Remote, path string) string {
		t.Helper()
		spec, err := r.Build(srcBackupDef(func(d *syncrv1.ResticBackupDefinition) {
			d.RemoteSource.RemoteId, d.RemoteSource.Path = src.ID.String(), path
		}), srcResolved(t, REPO.ID, "restic", REPO, src))
		if err != nil {
			t.Fatal(err)
		}
		return spec.GetResticBackup().GetRemoteSource().GetSourceKey()
	}
	src := s3Remote(rid(5), "photos-bucket")
	base := keyOf(src, "2024")

	rotated := src
	rotated.Rclone.Secrets = maps.Clone(src.Rclone.Secrets)
	rotated.Rclone.Secrets["secret_access_key"] = "rotated"
	if got := keyOf(rotated, "2024"); got != base {
		t.Errorf("a rotated secret changed the key: %s, want %s", got, base)
	}
	if got := keyOf(src, "/2024/"); got != base {
		t.Errorf("slashes changed the key: %s, want %s", got, base)
	}
	bucket := src
	bucket.Rclone.Root = "other-bucket"
	endpoint := src
	endpoint.Rclone.Options = maps.Clone(src.Rclone.Options)
	endpoint.Rclone.Options["endpoint"] = "https://elsewhere.example"
	other := s3Remote(rid(6), "photos-bucket")
	for name, k := range map[string]string{
		"another bucket":   keyOf(bucket, "2024"),
		"another endpoint": keyOf(endpoint, "2024"),
		"another folder":   keyOf(src, "2025"),
		"another remote":   keyOf(other, "2024"),
	} {
		if k == base {
			t.Errorf("%s kept the key %s", name, k)
		}
		if !link.ValidSourceKey(k) {
			t.Errorf("%s: key %q is not valid", name, k)
		}
	}
	// Pinned: a change of the encoding would orphan every worker's mirrors.
	if base != pinnedSourceKey {
		t.Errorf("source key = %s, want %s (the encoding changed: bump remoteSourceKeyVersion on purpose only)", base, pinnedSourceKey)
	}
}

// pinnedSourceKey is the source key of s3Remote(rid(5), "photos-bucket")
// and the folder "2024".
const pinnedSourceKey = "9b98998c224daac28a0f32ee14319c2a"

func TestRemoteSourceBuildRejects(t *testing.T) {
	t.Parallel()
	r := builtin(t)
	REPO, SRC, LCL := s3Remote(rid(1), "vault"), sftpRemote(rid(5), ""), localRemote(rid(5), "/srv/photos")
	onRepoRemote := func(path string) *syncrv1.JobDefinition {
		return srcBackupDef(func(d *syncrv1.ResticBackupDefinition) {
			d.RemoteSource.RemoteId, d.RemoteSource.Path = rid(1).String(), path
		})
	}
	const pathField = "definition.restic_backup.remote_source.path"
	tests := []struct {
		name          string
		def           *syncrv1.JobDefinition
		in            jobkind.Resolved
		want          error
		field, field2 string
	}{
		{"a local remote", srcBackupDef(), srcResolved(t, REPO.ID, "restic", REPO, LCL), domain.ErrInvalidArgument,
			"definition.restic_backup.remote_source.remote_id", jobkind.RemoteSourceLocalMessage},
		{"the repository's folder", onRepoRemote("restic/"), srcResolved(t, REPO.ID, "restic", REPO), domain.ErrInvalidArgument,
			pathField, jobkind.RemoteSourceIsRepository},
		{"a folder holding the repository", onRepoRemote("a"), srcResolved(t, REPO.ID, "a/b/restic", REPO), domain.ErrInvalidArgument,
			pathField, jobkind.RemoteSourceHoldsRepository},
		{"the remote's root holds the repository", onRepoRemote(""), srcResolved(t, REPO.ID, "restic", REPO), domain.ErrInvalidArgument,
			pathField, jobkind.RemoteSourceHoldsRepository},
		{"a folder inside the repository", onRepoRemote("restic/data"), srcResolved(t, REPO.ID, "restic", REPO), domain.ErrInvalidArgument,
			pathField, jobkind.RemoteSourceInsideRepository},
		{"the source remote is not resolved", srcBackupDef(), srcResolved(t, REPO.ID, "restic", REPO), domain.ErrNotFound, "", ""},
		{"the repository's remote is not resolved", srcBackupDef(), srcResolved(t, REPO.ID, "restic", SRC), domain.ErrNotFound, "", ""},
		{"no repository", srcBackupDef(), jobkind.Resolved{Remotes: remoteMap(REPO, SRC)}, domain.ErrNotFound, "", ""},
		{"an invalid definition", srcBackupDef(func(d *syncrv1.ResticBackupDefinition) { d.RemoteSource.Method = 0 }),
			srcResolved(t, REPO.ID, "restic", REPO, SRC), domain.ErrInvalidArgument, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := r.Build(tt.def, tt.in)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Build = %v, want %v", err, tt.want)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Errorf("the error contains a secret: %v", err)
			}
			if tt.field != "" {
				var verr *domain.ValidationError
				if !errors.As(err, &verr) || len(verr.Fields) != 1 || verr.Fields[0].Field != tt.field || verr.Fields[0].Message != tt.field2 {
					t.Errorf("Build = %v, want %s: %s", err, tt.field, tt.field2)
				}
			}
		})
	}
	// A folder beside the repository is fine ("restic2" is not inside
	// "restic").
	if _, err := r.Build(onRepoRemote("restic2"), srcResolved(t, REPO.ID, "restic", REPO)); err != nil {
		t.Errorf("beside the repository: %v", err)
	}
}

func TestRemoteSourceOverlap(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ src, repo, want string }{
		{"a", "a", jobkind.RemoteSourceIsRepository},
		{"/a/./b/", "a//b", jobkind.RemoteSourceIsRepository},
		{"", "", jobkind.RemoteSourceIsRepository},
		{"", "a", jobkind.RemoteSourceHoldsRepository},
		{"a", "a/b", jobkind.RemoteSourceHoldsRepository},
		{"a/b", "a", jobkind.RemoteSourceInsideRepository},
		{"a/b", "", jobkind.RemoteSourceInsideRepository},
		{"a", "ab", ""},
		{"data2", "data", ""},
		{"x/y", "x/z", ""},
	} {
		if got := jobkind.RemoteSourceOverlap(tt.src, tt.repo); got != tt.want {
			t.Errorf("RemoteSourceOverlap(%q, %q) = %q, want %q", tt.src, tt.repo, got, tt.want)
		}
	}
}

func TestRemoteSourceDescribe(t *testing.T) {
	t.Parallel()
	r := builtin(t)
	names := jobkind.Names{
		Remotes:      map[domain.RemoteID]string{rid(5): "B2 photos"},
		Repositories: map[domain.ResticRepoID]string{repoID(t): "Photos vault"},
	}
	for _, tt := range []struct {
		def  *syncrv1.JobDefinition
		want string
	}{
		{srcBackupDef(func(d *syncrv1.ResticBackupDefinition) { d.RemoteSource.Path = "/Pictures/" }), "Back up B2 photos/Pictures (mirror) → Photos vault"},
		{srcBackupDef(func(d *syncrv1.ResticBackupDefinition) { d.RemoteSource.Path, d.RemoteSource.Method = "", mount }), "Back up B2 photos (mount) → Photos vault"},
		{srcBackupDef(func(d *syncrv1.ResticBackupDefinition) { d.DryRun = true }), "Back up B2 photos/photos (mirror) → Photos vault (dry run)"},
		{srcBackupDef(func(d *syncrv1.ResticBackupDefinition) { d.RemoteSource.RemoteId = rid(9).String() }), "Back up Unknown remote/photos (mirror) → Photos vault"},
		// Unchanged for paths.
		{backupDef(), "Back up /srv/photos → Photos vault"},
	} {
		if got := r.DescribeOperation(tt.def, names); got != tt.want {
			t.Errorf("Describe = %q, want %q", got, tt.want)
		}
	}
}

func TestRemoteSourceSummarize(t *testing.T) {
	t.Parallel()
	r := builtin(t)
	mirrorStats := &syncrv1.RemoteSourceStats{
		Method: mirror, MirroredBytes: 1288490189, MirroredFiles: 40, DeletedFiles: 3, MirrorElapsed: durationpb.New(125 * time.Second),
	}
	encode := func(o *syncrv1.ResticBackupOutput) []byte {
		b, err := proto.Marshal(&syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticBackup{ResticBackup: o}})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	restic := func(rs *syncrv1.RemoteSourceStats) *syncrv1.ResticBackupOutput {
		return &syncrv1.ResticBackupOutput{SnapshotId: "1a2b3c4d5e6f", FilesNew: 2, FilesUnmodified: 38, DataAddedBytes: 2048,
			TotalFilesProcessed: 40, TotalBytesProcessed: 1288490189, RemoteSource: rs}
	}
	resticRows := []jobkind.Detail{
		{Label: "Files", Value: "2 new, 0 changed, 38 unmodified"},
		{Label: "Folders", Value: "0 new, 0 changed, 0 unmodified"},
		{Label: "Data added", Value: "2.0 KiB"},
		{Label: "Processed", Value: "1.2 GiB in 40 files"},
	}
	snapshot := jobkind.Detail{Label: "Snapshot", Value: "1a2b3c4d"}
	mirrored := []jobkind.Detail{{Label: "Read through", Value: "Mirror"}, {Label: "Mirrored", Value: "1.2 GiB, 40 files, 3 deleted in 2m5s"}}
	discarded := proto.CloneOf(mirrorStats)
	discarded.MirrorDiscarded = true

	for _, tt := range []struct {
		name string
		def  *syncrv1.JobDefinition
		out  []byte
		want []jobkind.Detail
	}{
		{"mirror", srcBackupDef(), encode(restic(mirrorStats)), slices.Concat([]jobkind.Detail{snapshot}, mirrored, resticRows)},
		{"discarded mirror", srcBackupDef(), encode(restic(discarded)),
			slices.Concat([]jobkind.Detail{snapshot}, mirrored, []jobkind.Detail{{Label: "Mirror", Value: "Deleted after the run"}}, resticRows)},
		{"mount", srcBackupDef(), encode(restic(&syncrv1.RemoteSourceStats{Method: mount})),
			slices.Concat([]jobkind.Detail{snapshot, {Label: "Read through", Value: "Mount"}}, resticRows)},
		{"empty folder allowed", srcBackupDef(), encode(&syncrv1.ResticBackupOutput{RemoteSource: &syncrv1.RemoteSourceStats{Method: mirror, DeletedFiles: 3}}),
			[]jobkind.Detail{{Label: "Snapshot", Value: "No snapshot: the remote folder was empty"}, {Label: "Read through", Value: "Mirror"},
				{Label: "Mirrored", Value: "0 bytes, 0 files, 3 deleted"}}},
		{"dry run", srcBackupDef(func(d *syncrv1.ResticBackupDefinition) { d.DryRun = true }),
			encode(&syncrv1.ResticBackupOutput{RemoteSource: &syncrv1.RemoteSourceStats{Method: mount}}),
			slices.Concat([]jobkind.Detail{{Label: "Read through", Value: "Mount"}}, []jobkind.Detail{
				{Label: "Files", Value: "0 new, 0 changed, 0 unmodified"}, {Label: "Folders", Value: "0 new, 0 changed, 0 unmodified"},
				{Label: "Data added", Value: "0 bytes"}, {Label: "Processed", Value: "0 bytes in 0 files"},
			})},
		// Unchanged for a backup of paths.
		{"paths", backupDef(), encode(restic(nil)), slices.Concat([]jobkind.Detail{snapshot}, resticRows)},
	} {
		if got := r.Summarize("restic_backup", tt.def, tt.out); !slices.Equal(got, tt.want) {
			t.Errorf("%s: Summarize =\n%q\nwant\n%q", tt.name, got, tt.want)
		}
	}
}
