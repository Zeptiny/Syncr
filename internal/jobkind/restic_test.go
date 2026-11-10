package jobkind_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
)

const (
	testRepoID   = "0195f0a4-3b7c-7d1e-9f00-00000000aaaa"
	testResticID = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"
	testSnapshot = "1a2b3c4d5e6f"
)

func repoID(t *testing.T) domain.ResticRepoID {
	t.Helper()
	id, err := domain.ParseResticRepoID(testRepoID)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func builtin(t *testing.T) *jobkind.Registry {
	t.Helper()
	r, err := jobkind.NewRegistry(jobkind.Builtin()...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func backupDef(mutate ...func(*syncrv1.ResticBackupDefinition)) *syncrv1.JobDefinition {
	d := &syncrv1.ResticBackupDefinition{RepositoryId: testRepoID, Paths: []string{"/srv/photos"}}
	for _, m := range mutate {
		m(d)
	}
	return &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticBackup{ResticBackup: d}}
}

func forgetDef(mutate ...func(*syncrv1.ResticForgetDefinition)) *syncrv1.JobDefinition {
	d := &syncrv1.ResticForgetDefinition{RepositoryId: testRepoID, Policy: &syncrv1.ResticRetentionPolicy{KeepDaily: 7}}
	for _, m := range mutate {
		m(d)
	}
	return &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticForget{ResticForget: d}}
}

func pruneDef(dry bool) *syncrv1.JobDefinition {
	return &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticPrune{ResticPrune: &syncrv1.ResticPruneDefinition{RepositoryId: testRepoID, DryRun: dry}}}
}

func resticCheckDef(percent int32) *syncrv1.JobDefinition {
	return &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticCheck{ResticCheck: &syncrv1.ResticCheckDefinition{RepositoryId: testRepoID, ReadDataPercent: percent}}}
}

func restoreDef(mutate ...func(*syncrv1.ResticRestoreDefinition)) *syncrv1.JobDefinition {
	d := &syncrv1.ResticRestoreDefinition{RepositoryId: testRepoID, SnapshotId: testSnapshot, Target: "/srv/restore"}
	for _, m := range mutate {
		m(d)
	}
	return &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticRestore{ResticRestore: d}}
}

func deleteDataDef() *syncrv1.JobDefinition {
	return &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticDeleteData{ResticDeleteData: &syncrv1.ResticDeleteDataDefinition{RepositoryId: testRepoID}}}
}

func TestResticKindProperties(t *testing.T) {
	t.Parallel()
	r := builtin(t)
	const system = "is started by deleting the repository"
	tests := []struct {
		kind, label           string
		schedulable           bool
		scheduleWhy, startWhy string
		features              []string
		def                   *syncrv1.JobDefinition
	}{
		{kind: "restic_backup", label: "Backup", schedulable: true, features: []string{"restic.v2"}, def: backupDef()},
		{kind: "restic_forget", label: "Retention", schedulable: true, features: []string{"restic.v2"}, def: forgetDef()},
		{kind: "restic_prune", label: "Prune", schedulable: true, features: []string{"restic.v2"}, def: pruneDef(false)},
		{kind: "restic_check", label: "Check", schedulable: true, features: []string{"restic.v2"}, def: resticCheckDef(0)},
		{kind: "restic_restore", label: "Restore", scheduleWhy: "restores cannot be scheduled", features: []string{"restic.v2"}, def: restoreDef()},
		{kind: "restic_delete_data", label: "Delete data", scheduleWhy: system, startWhy: system,
			features: []string{"restic.v2", "restic.delete"}, def: deleteDataDef()},
	}
	key := "restic-repo:" + testRepoID
	for _, tt := range tests {
		if got := r.Label(tt.kind); got != tt.label {
			t.Errorf("Label(%s) = %q, want %q", tt.kind, got, tt.label)
		}
		if ok, why := r.Schedulable(tt.kind); ok != tt.schedulable || why != tt.scheduleWhy {
			t.Errorf("Schedulable(%s) = (%v, %q)", tt.kind, ok, why)
		}
		if ok, why := r.UserStartable(tt.kind); ok != (tt.startWhy == "") || why != tt.startWhy {
			t.Errorf("UserStartable(%s) = (%v, %q)", tt.kind, ok, why)
		}
		if got := r.RequiredFeatures(tt.kind); !slices.Equal(got, tt.features) {
			t.Errorf("RequiredFeatures(%s) = %q, want %q", tt.kind, got, tt.features)
		}
		kind, refs, err := r.Check(tt.def)
		if err != nil || kind != tt.kind {
			t.Errorf("Check(%s) = (%q, %v)", tt.kind, kind, err)
			continue
		}
		if refs.RepositoryID != repoID(t) || !slices.Equal(refs.ConcurrencyKeys, []string{key}) || len(refs.RemoteIDs) != 0 {
			t.Errorf("Check(%s) refs = %+v, want the repository and its key", tt.kind, refs)
		}
	}
	// 20 (init) and 26 (snapshots) are probe operations, not kinds.
	for _, k := range []string{"restic_init", "restic_snapshots"} {
		if _, ok := r.Get(k); ok {
			t.Errorf("%s is registered as a job kind", k)
		}
	}
}

func TestResticCheckAccepts(t *testing.T) {
	t.Parallel()
	r := builtin(t)
	tests := []struct {
		name  string
		def   *syncrv1.JobDefinition
		terms []string
	}{
		{"backup", backupDef(func(d *syncrv1.ResticBackupDefinition) {
			d.Paths = []string{"/srv/photos", `C:\Users\ada`, `\\nas\share\x`}
			d.Excludes, d.Tags, d.Host = []string{"*.tmp", "cache/**"}, []string{"nightly"}, "nas-01"
			d.ExcludeLargerThanBytes = i64(1 << 30)
			d.UseFsSnapshot, d.ExcludeCaches, d.OneFileSystem, d.DryRun = true, true, true, true
		}), []string{"/srv/photos", `C:\Users\ada`, `\\nas\share\x`, "nightly", "nas-01"}},
		{"forget with every rule", forgetDef(func(d *syncrv1.ResticForgetDefinition) {
			d.Policy = &syncrv1.ResticRetentionPolicy{KeepLast: 1, KeepHourly: 2, KeepDaily: 3, KeepWeekly: 4, KeepMonthly: 5,
				KeepYearly: 100000, KeepTags: []string{"keep"}, KeepWithin: "1y6m14d12h"}
			d.Tags, d.Host, d.Prune = []string{"nightly"}, "nas-01", true
		}), []string{"retention", "nightly", "nas-01"}},
		{"forget by tag only", forgetDef(func(d *syncrv1.ResticForgetDefinition) {
			d.Policy = &syncrv1.ResticRetentionPolicy{KeepTags: []string{"keep"}}
		}), []string{"retention"}},
		{"forget within only", forgetDef(func(d *syncrv1.ResticForgetDefinition) {
			d.Policy = &syncrv1.ResticRetentionPolicy{KeepWithin: "30d"}
		}), []string{"retention"}},
		{"prune", pruneDef(true), []string{"prune"}},
		{"check structure", resticCheckDef(0), []string{"check"}},
		{"check all data", resticCheckDef(100), []string{"check"}},
		{"restore whole", restoreDef(), []string{"restore", testSnapshot, "/srv/restore"}},
		{"restore latest, selected", restoreDef(func(d *syncrv1.ResticRestoreDefinition) {
			d.SnapshotId, d.Paths = "latest", []string{"/srv/a b", "/srv/[x]*?.txt", "/C/Users/ada"}
			d.Excludes, d.Verify = []string{"*.tmp"}, true
			d.Overwrite = syncrv1.ResticRestoreOverwrite_RESTIC_RESTORE_OVERWRITE_IF_CHANGED
		}), []string{"restore", "latest", "/srv/restore"}},
		{"restore subfolder to windows", restoreDef(func(d *syncrv1.ResticRestoreDefinition) {
			d.SnapshotId, d.Subfolder, d.Target = strings.Repeat("ab", 32), "/C/Users/ada", `D:\Restore`
		}), []string{"restore", strings.Repeat("ab", 32), `D:\Restore`, "/C/Users/ada"}},
		{"delete data", deleteDataDef(), []string{"delete data"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, refs, err := r.Check(tt.def)
			if err != nil {
				t.Fatalf("Check = %v", err)
			}
			if !slices.Equal(refs.SearchTerms, tt.terms) {
				t.Errorf("SearchTerms = %q, want %q", refs.SearchTerms, tt.terms)
			}
		})
	}
}

func TestResticCheckRejects(t *testing.T) {
	t.Parallel()
	r := builtin(t)
	many := func(n int, s string) []string { return slices.Repeat([]string{s}, n) }
	backup := func(m func(*syncrv1.ResticBackupDefinition)) *syncrv1.JobDefinition { return backupDef(m) }
	forget := func(m func(*syncrv1.ResticForgetDefinition)) *syncrv1.JobDefinition { return forgetDef(m) }
	restore := func(m func(*syncrv1.ResticRestoreDefinition)) *syncrv1.JobDefinition { return restoreDef(m) }
	tests := []struct {
		name  string
		def   *syncrv1.JobDefinition
		field string
	}{
		// The repository, for every kind.
		{"backup without a repository", backup(func(d *syncrv1.ResticBackupDefinition) { d.RepositoryId = "" }), "definition.restic_backup.repository_id"},
		{"backup with a bad repository", backup(func(d *syncrv1.ResticBackupDefinition) { d.RepositoryId = "nope" }), "definition.restic_backup.repository_id"},
		{"backup with the zero repository", backup(func(d *syncrv1.ResticBackupDefinition) {
			d.RepositoryId = "00000000-0000-0000-0000-000000000000"
		}), "definition.restic_backup.repository_id"},
		{"forget without a repository", forget(func(d *syncrv1.ResticForgetDefinition) { d.RepositoryId = "" }), "definition.restic_forget.repository_id"},
		{"prune without a repository", &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticPrune{ResticPrune: &syncrv1.ResticPruneDefinition{}}}, "definition.restic_prune.repository_id"},
		{"check without a repository", &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticCheck{ResticCheck: &syncrv1.ResticCheckDefinition{}}}, "definition.restic_check.repository_id"},
		{"restore without a repository", restore(func(d *syncrv1.ResticRestoreDefinition) { d.RepositoryId = "" }), "definition.restic_restore.repository_id"},
		{"delete data without a repository", &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticDeleteData{ResticDeleteData: &syncrv1.ResticDeleteDataDefinition{}}}, "definition.restic_delete_data.repository_id"},

		// Backup.
		{"no paths", backup(func(d *syncrv1.ResticBackupDefinition) { d.Paths = nil }), "definition.restic_backup.paths"},
		{"too many paths", backup(func(d *syncrv1.ResticBackupDefinition) { d.Paths = many(101, "/a") }), "definition.restic_backup.paths"},
		{"relative path", backup(func(d *syncrv1.ResticBackupDefinition) { d.Paths = []string{"/ok", "srv/photos"} }), "definition.restic_backup.paths[1]"},
		{"empty path", backup(func(d *syncrv1.ResticBackupDefinition) { d.Paths = []string{""} }), "definition.restic_backup.paths[0]"},
		{"dot-dot path", backup(func(d *syncrv1.ResticBackupDefinition) { d.Paths = []string{"/srv/../etc"} }), "definition.restic_backup.paths[0]"},
		{"dot-dot windows path", backup(func(d *syncrv1.ResticBackupDefinition) { d.Paths = []string{`C:\Users\..\Windows`} }), "definition.restic_backup.paths[0]"},
		{"long path", backup(func(d *syncrv1.ResticBackupDefinition) { d.Paths = []string{"/" + strings.Repeat("a", 4096)} }), "definition.restic_backup.paths[0]"},
		{"control character in a path", backup(func(d *syncrv1.ResticBackupDefinition) { d.Paths = []string{"/srv/a\tb"} }), "definition.restic_backup.paths[0]"},
		{"duplicate path", backup(func(d *syncrv1.ResticBackupDefinition) { d.Paths = []string{"/srv/a", "/srv//a/"} }), "definition.restic_backup.paths[1]"},
		{"too many excludes", backup(func(d *syncrv1.ResticBackupDefinition) { d.Excludes = many(1001, "x") }), "definition.restic_backup.excludes"},
		{"empty exclude", backup(func(d *syncrv1.ResticBackupDefinition) { d.Excludes = []string{"a", ""} }), "definition.restic_backup.excludes[1]"},
		{"long exclude", backup(func(d *syncrv1.ResticBackupDefinition) { d.Excludes = []string{strings.Repeat("a", 4097)} }), "definition.restic_backup.excludes[0]"},
		{"line break in an exclude", backup(func(d *syncrv1.ResticBackupDefinition) { d.Excludes = []string{"a\nb"} }), "definition.restic_backup.excludes[0]"},
		{"NUL in an exclude", backup(func(d *syncrv1.ResticBackupDefinition) { d.Excludes = []string{"a\x00b"} }), "definition.restic_backup.excludes[0]"},
		{"too many tags", backup(func(d *syncrv1.ResticBackupDefinition) { d.Tags = many(33, "t") }), "definition.restic_backup.tags"},
		{"empty tag", backup(func(d *syncrv1.ResticBackupDefinition) { d.Tags = []string{""} }), "definition.restic_backup.tags[0]"},
		{"long tag", backup(func(d *syncrv1.ResticBackupDefinition) { d.Tags = []string{strings.Repeat("t", 129)} }), "definition.restic_backup.tags[0]"},
		{"comma in a tag", backup(func(d *syncrv1.ResticBackupDefinition) { d.Tags = []string{"a,b"} }), "definition.restic_backup.tags[0]"},
		{"control character in a tag", backup(func(d *syncrv1.ResticBackupDefinition) { d.Tags = []string{"a\rb"} }), "definition.restic_backup.tags[0]"},
		{"long host", backup(func(d *syncrv1.ResticBackupDefinition) { d.Host = strings.Repeat("h", 256) }), "definition.restic_backup.host"},
		{"control character in the host", backup(func(d *syncrv1.ResticBackupDefinition) { d.Host = "a\nb" }), "definition.restic_backup.host"},
		{"zero size limit", backup(func(d *syncrv1.ResticBackupDefinition) { d.ExcludeLargerThanBytes = i64(0) }), "definition.restic_backup.exclude_larger_than_bytes"},
		{"negative size limit", backup(func(d *syncrv1.ResticBackupDefinition) { d.ExcludeLargerThanBytes = i64(-1) }), "definition.restic_backup.exclude_larger_than_bytes"},

		// Forget.
		{"no policy", forget(func(d *syncrv1.ResticForgetDefinition) { d.Policy = nil }), "definition.restic_forget.policy"},
		{"empty policy", forget(func(d *syncrv1.ResticForgetDefinition) { d.Policy = &syncrv1.ResticRetentionPolicy{} }), "definition.restic_forget.policy"},
		{"negative keep", forget(func(d *syncrv1.ResticForgetDefinition) { d.Policy.KeepLast = -1 }), "definition.restic_forget.policy.keep_last"},
		{"huge keep", forget(func(d *syncrv1.ResticForgetDefinition) { d.Policy.KeepYearly = 100001 }), "definition.restic_forget.policy.keep_yearly"},
		{"bad keep_within", forget(func(d *syncrv1.ResticForgetDefinition) { d.Policy.KeepWithin = "2 weeks" }), "definition.restic_forget.policy.keep_within"},
		{"zero keep_within", forget(func(d *syncrv1.ResticForgetDefinition) { d.Policy.KeepWithin = "0d" }), "definition.restic_forget.policy.keep_within"},
		{"comma in a keep tag", forget(func(d *syncrv1.ResticForgetDefinition) { d.Policy.KeepTags = []string{"a,b"} }), "definition.restic_forget.policy.keep_tags[0]"},
		{"bad filter tag", forget(func(d *syncrv1.ResticForgetDefinition) { d.Tags = []string{""} }), "definition.restic_forget.tags[0]"},
		{"bad filter host", forget(func(d *syncrv1.ResticForgetDefinition) { d.Host = "a\x00" }), "definition.restic_forget.host"},

		// Check.
		{"negative percent", resticCheckDef(-1), "definition.restic_check.read_data_percent"},
		{"percent over 100", resticCheckDef(101), "definition.restic_check.read_data_percent"},

		// Restore.
		{"no snapshot", restore(func(d *syncrv1.ResticRestoreDefinition) { d.SnapshotId = "" }), "definition.restic_restore.snapshot_id"},
		{"short snapshot", restore(func(d *syncrv1.ResticRestoreDefinition) { d.SnapshotId = "1a2b3c4" }), "definition.restic_restore.snapshot_id"},
		{"upper-case snapshot", restore(func(d *syncrv1.ResticRestoreDefinition) { d.SnapshotId = "1A2B3C4D" }), "definition.restic_restore.snapshot_id"},
		{"snapshot with a subfolder", restore(func(d *syncrv1.ResticRestoreDefinition) { d.SnapshotId = "1a2b3c4d:/etc" }), "definition.restic_restore.snapshot_id"},
		{"no target", restore(func(d *syncrv1.ResticRestoreDefinition) { d.Target = "" }), "definition.restic_restore.target"},
		{"relative target", restore(func(d *syncrv1.ResticRestoreDefinition) { d.Target = "restore" }), "definition.restic_restore.target"},
		{"dot-dot target", restore(func(d *syncrv1.ResticRestoreDefinition) { d.Target = "/srv/../etc" }), "definition.restic_restore.target"},
		{"too many items", restore(func(d *syncrv1.ResticRestoreDefinition) { d.Paths = many(1001, "/a") }), "definition.restic_restore.paths"},
		{"relative item", restore(func(d *syncrv1.ResticRestoreDefinition) { d.Paths = []string{"/a", "b"} }), "definition.restic_restore.paths[1]"},
		{"dot-dot item", restore(func(d *syncrv1.ResticRestoreDefinition) { d.Paths = []string{"/a/../b"} }), "definition.restic_restore.paths[0]"},
		{"control character in an item", restore(func(d *syncrv1.ResticRestoreDefinition) { d.Paths = []string{"/a\nb"} }), "definition.restic_restore.paths[0]"},
		{"items and a subfolder", restore(func(d *syncrv1.ResticRestoreDefinition) { d.Paths, d.Subfolder = []string{"/a"}, "/b" }), "definition.restic_restore.paths"},
		{"relative subfolder", restore(func(d *syncrv1.ResticRestoreDefinition) { d.Subfolder = "srv" }), "definition.restic_restore.subfolder"},
		{"dot-dot subfolder", restore(func(d *syncrv1.ResticRestoreDefinition) { d.Subfolder = "/srv/.." }), "definition.restic_restore.subfolder"},
		{"bad exclude", restore(func(d *syncrv1.ResticRestoreDefinition) { d.Excludes = []string{""} }), "definition.restic_restore.excludes[0]"},
		{"unknown overwrite", restore(func(d *syncrv1.ResticRestoreDefinition) { d.Overwrite = 99 }), "definition.restic_restore.overwrite"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := r.Check(tt.def)
			if got := fields(err); !slices.Contains(got, tt.field) {
				t.Errorf("Check = %v (fields %q), want a problem on %s", err, got, tt.field)
			}
		})
	}
}

// resolved returns the repository at path on remote, with that remote.
func resolved(t *testing.T, remote jobkind.Remote, path string) jobkind.Resolved {
	t.Helper()
	id := repoID(t)
	return jobkind.Resolved{
		Remotes: remoteMap(remote),
		Repositories: map[domain.ResticRepoID]jobkind.Repository{
			id: {ID: id, RemoteID: remote.ID, Path: path, Password: "repo-secret", ResticID: testResticID},
		},
	}
}

func TestResticBuild(t *testing.T) {
	t.Parallel()
	r := builtin(t)
	S3, LCL := s3Remote(rid(1), "bucket"), localRemote(rid(2), "/srv/backups")
	onS3 := &syncrv1.ResticRepository{Location: sloc("r1", "bucket/restic/photos"), Password: "repo-secret"}
	onDisk := &syncrv1.ResticRepository{Location: sloc("", "/srv/backups/restic/photos"), Password: "repo-secret"}
	s3Defs := []*syncrv1.RemoteDefinition{rdef("r1", S3)}
	const never = syncrv1.ResticRestoreOverwrite_RESTIC_RESTORE_OVERWRITE_NEVER

	tests := []struct {
		name string
		def  *syncrv1.JobDefinition
		in   jobkind.Resolved
		want *syncrv1.JobSpec
	}{
		{"backup to a remote", backupDef(func(d *syncrv1.ResticBackupDefinition) {
			d.Paths = []string{"/srv//photos/", `C:\Users\ada`}
			d.Excludes, d.Tags, d.Host = []string{"*.tmp"}, []string{"nightly"}, "nas-01"
			d.ExcludeLargerThanBytes = i64(5)
			d.UseFsSnapshot, d.ExcludeCaches, d.OneFileSystem, d.DryRun = true, true, true, true
		}), resolved(t, S3, "restic/photos"), &syncrv1.JobSpec{Remotes: s3Defs, Operation: &syncrv1.JobSpec_ResticBackup{ResticBackup: &syncrv1.ResticBackup{
			Repository: onS3, Paths: []string{"/srv/photos", `C:\Users\ada`}, Excludes: []string{"*.tmp"}, Tags: []string{"nightly"}, Host: "nas-01",
			ExcludeLargerThanBytes: i64(5), UseFsSnapshot: true, ExcludeCaches: true, OneFileSystem: true, DryRun: true,
		}}}},
		{"backup to a local remote", backupDef(), resolved(t, LCL, "restic/photos"), &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_ResticBackup{ResticBackup: &syncrv1.ResticBackup{
			Repository: onDisk, Paths: []string{"/srv/photos"},
		}}}},
		{"forget", forgetDef(func(d *syncrv1.ResticForgetDefinition) {
			d.Policy = &syncrv1.ResticRetentionPolicy{KeepDaily: 7, KeepTags: []string{"keep"}, KeepWithin: "1y"}
			d.Prune, d.Tags, d.Host, d.DryRun = true, []string{"nightly"}, "nas-01", true
		}), resolved(t, S3, "restic/photos"), &syncrv1.JobSpec{Remotes: s3Defs, Operation: &syncrv1.JobSpec_ResticForget{ResticForget: &syncrv1.ResticForget{
			Repository: onS3, Policy: &syncrv1.ResticRetentionPolicy{KeepDaily: 7, KeepTags: []string{"keep"}, KeepWithin: "1y"},
			Prune: true, Tags: []string{"nightly"}, Host: "nas-01", DryRun: true,
		}}}},
		{"prune", pruneDef(true), resolved(t, LCL, "restic/photos"), &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_ResticPrune{ResticPrune: &syncrv1.ResticPrune{
			Repository: onDisk, DryRun: true,
		}}}},
		{"check", resticCheckDef(10), resolved(t, S3, "restic/photos"), &syncrv1.JobSpec{Remotes: s3Defs, Operation: &syncrv1.JobSpec_ResticCheck{ResticCheck: &syncrv1.ResticCheck{
			Repository: onS3, ReadDataPercent: 10,
		}}}},
		{"restore defaults to never overwrite", restoreDef(), resolved(t, S3, "restic/photos"), &syncrv1.JobSpec{Remotes: s3Defs, Operation: &syncrv1.JobSpec_ResticRestore{ResticRestore: &syncrv1.ResticRestore{
			Repository: onS3, SnapshotId: testSnapshot, Target: "/srv/restore", Overwrite: never,
		}}}},
		{"restore selected items", restoreDef(func(d *syncrv1.ResticRestoreDefinition) {
			d.Paths, d.Excludes, d.Verify = []string{"/srv/a b", "/srv/[x]*?.txt"}, []string{"*.tmp"}, true
			d.Overwrite = syncrv1.ResticRestoreOverwrite_RESTIC_RESTORE_OVERWRITE_ALWAYS
		}), resolved(t, LCL, "restic/photos"), &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_ResticRestore{ResticRestore: &syncrv1.ResticRestore{
			Repository: onDisk, SnapshotId: testSnapshot, Target: "/srv/restore", Paths: []string{"/srv/a b", "/srv/[x]*?.txt"},
			Excludes: []string{"*.tmp"}, Verify: true, Overwrite: syncrv1.ResticRestoreOverwrite_RESTIC_RESTORE_OVERWRITE_ALWAYS,
		}}}},
		{"restore a subfolder", restoreDef(func(d *syncrv1.ResticRestoreDefinition) {
			d.Subfolder, d.Target = "/C/Users/ada", `D:/Restore`
		}), resolved(t, S3, "restic/photos"), &syncrv1.JobSpec{Remotes: s3Defs, Operation: &syncrv1.JobSpec_ResticRestore{ResticRestore: &syncrv1.ResticRestore{
			Repository: onS3, SnapshotId: testSnapshot, Target: `D:\Restore`, Subfolder: "/C/Users/ada", Overwrite: never,
		}}}},
		{"delete data in a folder", deleteDataDef(), resolved(t, S3, "restic/photos"), &syncrv1.JobSpec{Remotes: s3Defs, Operation: &syncrv1.JobSpec_ResticDeleteData{ResticDeleteData: &syncrv1.ResticDeleteData{
			Repository: onS3, ExpectedResticId: testResticID, RemoveDirectory: true,
		}}}},
		{"delete data at the remote's root", deleteDataDef(), resolved(t, S3, ""), &syncrv1.JobSpec{Remotes: s3Defs, Operation: &syncrv1.JobSpec_ResticDeleteData{ResticDeleteData: &syncrv1.ResticDeleteData{
			Repository: &syncrv1.ResticRepository{Location: sloc("r1", "bucket"), Password: "repo-secret"}, ExpectedResticId: testResticID,
		}}}},
		{"delete data on a local remote", deleteDataDef(), resolved(t, LCL, "restic/photos"), &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_ResticDeleteData{ResticDeleteData: &syncrv1.ResticDeleteData{
			Repository: onDisk, ExpectedResticId: testResticID, RemoveDirectory: true,
		}}}},
		{"delete data again after an attempt that ran", &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticDeleteData{ResticDeleteData: &syncrv1.ResticDeleteDataDefinition{
			RepositoryId: testRepoID, Resume: true,
		}}}, resolved(t, S3, "restic/photos"), &syncrv1.JobSpec{Remotes: s3Defs, Operation: &syncrv1.JobSpec_ResticDeleteData{ResticDeleteData: &syncrv1.ResticDeleteData{
			Repository: onS3, ExpectedResticId: testResticID, RemoveDirectory: true, Resume: true,
		}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := r.Build(tt.def, tt.in)
			if err != nil {
				t.Fatalf("Build = %v", err)
			}
			if !proto.Equal(got, tt.want) {
				t.Errorf("Build =\n%v\nwant\n%v", got, tt.want)
			}
		})
	}
}

func TestResticBuildRejects(t *testing.T) {
	t.Parallel()
	r := builtin(t)
	S3 := s3Remote(rid(1), "bucket")
	good := resolved(t, S3, "restic/photos")
	other := domain.NewResticRepoID()
	noPassword := resolved(t, S3, "restic/photos")
	noPassword.Repositories[repoID(t)] = jobkind.Repository{ID: repoID(t), RemoteID: S3.ID}

	tests := []struct {
		name string
		def  *syncrv1.JobDefinition
		in   jobkind.Resolved
		want error
	}{
		{"an invalid definition", backupDef(func(d *syncrv1.ResticBackupDefinition) { d.Paths = nil }), good, domain.ErrInvalidArgument},
		{"no repository resolved", backupDef(), jobkind.Resolved{Remotes: good.Remotes}, domain.ErrNotFound},
		{"another repository resolved", resticCheckDef(0), jobkind.Resolved{Remotes: good.Remotes,
			Repositories: map[domain.ResticRepoID]jobkind.Repository{other: {ID: other, RemoteID: S3.ID, Password: "repo-secret"}}}, domain.ErrNotFound},
		{"resolved under another ID", pruneDef(false), jobkind.Resolved{Remotes: good.Remotes,
			Repositories: map[domain.ResticRepoID]jobkind.Repository{repoID(t): {ID: other, RemoteID: S3.ID, Password: "repo-secret"}}}, domain.ErrInvalidArgument},
		{"no password", forgetDef(), noPassword, domain.ErrInvalidArgument},
		{"its remote is not resolved", restoreDef(), jobkind.Resolved{Repositories: good.Repositories}, domain.ErrNotFound},
		{"delete data without the remote", deleteDataDef(), jobkind.Resolved{Repositories: good.Repositories}, domain.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := r.Build(tt.def, tt.in)
			if !errors.Is(err, tt.want) {
				t.Errorf("Build = %v, want %v", err, tt.want)
			}
			if err != nil && strings.Contains(err.Error(), "repo-secret") {
				t.Errorf("the error contains the password: %v", err)
			}
		})
	}
}

func TestResticBuildDoesNotAlias(t *testing.T) {
	t.Parallel()
	r := builtin(t)
	def := forgetDef(func(d *syncrv1.ResticForgetDefinition) { d.Policy.KeepTags = []string{"keep"} })
	spec, err := r.Build(def, resolved(t, s3Remote(rid(1), "bucket"), "p"))
	if err != nil {
		t.Fatal(err)
	}
	spec.GetResticForget().GetPolicy().KeepTags[0] = "mutated"
	spec.GetResticForget().GetPolicy().KeepDaily = 99
	if p := def.GetResticForget().GetPolicy(); p.GetKeepTags()[0] != "keep" || p.GetKeepDaily() != 7 {
		t.Error("the spec aliases the definition's policy")
	}
}

func TestResticDescribe(t *testing.T) {
	t.Parallel()
	r := builtin(t)
	names := jobkind.Names{Repositories: map[domain.ResticRepoID]string{repoID(t): "Photos vault"}}
	tests := []struct {
		def  *syncrv1.JobDefinition
		want string
	}{
		{backupDef(), "Back up /srv/photos → Photos vault"},
		{backupDef(func(d *syncrv1.ResticBackupDefinition) { d.Paths = []string{"/srv/a", "/srv/b"} }), "Back up /srv/a, /srv/b → Photos vault"},
		{backupDef(func(d *syncrv1.ResticBackupDefinition) { d.Paths = []string{"/srv/a", "/srv/b", "/srv/c"} }), "Back up /srv/a + 2 more → Photos vault"},
		{backupDef(func(d *syncrv1.ResticBackupDefinition) { d.DryRun = true }), "Back up /srv/photos → Photos vault (dry run)"},
		{forgetDef(func(d *syncrv1.ResticForgetDefinition) {
			d.Policy, d.Prune = &syncrv1.ResticRetentionPolicy{KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 12}, true
		}), "Retention on Photos vault: 7 daily, 4 weekly, 12 monthly, then prune"},
		{forgetDef(func(d *syncrv1.ResticForgetDefinition) {
			d.Policy = &syncrv1.ResticRetentionPolicy{KeepLast: 1, KeepHourly: 2, KeepDaily: 3, KeepWeekly: 4, KeepMonthly: 5, KeepYearly: 6,
				KeepWithin: "1y", KeepTags: []string{"x", "y"}}
			d.DryRun = true
		}), "Retention on Photos vault: 1 last, 2 hourly, 3 daily, 4 weekly, 5 monthly, 6 yearly, within 1y, tags x,y (dry run)"},
		{pruneDef(false), "Prune Photos vault"},
		{pruneDef(true), "Prune Photos vault (dry run)"},
		{resticCheckDef(0), "Check Photos vault"},
		{resticCheckDef(10), "Check Photos vault (read 10% of the data)"},
		{resticCheckDef(100), "Check Photos vault (read all data)"},
		{restoreDef(), "Restore 1a2b3c4d from Photos vault → /srv/restore"},
		{restoreDef(func(d *syncrv1.ResticRestoreDefinition) { d.SnapshotId = "latest" }), "Restore latest from Photos vault → /srv/restore"},
		{restoreDef(func(d *syncrv1.ResticRestoreDefinition) { d.Paths = []string{"/a", "/b", "/c"} }), "Restore 3 items from Photos vault @ 1a2b3c4d → /srv/restore"},
		{restoreDef(func(d *syncrv1.ResticRestoreDefinition) { d.Paths = []string{"/srv/photos/a.jpg"} }), "Restore /srv/photos/a.jpg from Photos vault @ 1a2b3c4d → /srv/restore"},
		{restoreDef(func(d *syncrv1.ResticRestoreDefinition) { d.Subfolder = "/srv/photos/2025" }), "Restore /srv/photos/2025 from Photos vault @ 1a2b3c4d → /srv/restore"},
		{deleteDataDef(), "Delete the data of Photos vault"},
	}
	for _, tt := range tests {
		if got := r.Describe(tt.def, names); got != tt.want {
			t.Errorf("Describe = %q, want %q", got, tt.want)
		}
	}
	for def, want := range map[*syncrv1.JobDefinition]string{
		backupDef():     "Back up /srv/photos → Unknown repository",
		deleteDataDef(): "Delete the data of Unknown repository",
		backupDef(func(d *syncrv1.ResticBackupDefinition) { d.RepositoryId = "nope" }): "Back up /srv/photos → Unknown repository",
	} {
		if got := r.Describe(def, jobkind.Names{}); got != want {
			t.Errorf("Describe without names = %q, want %q", got, want)
		}
	}
}

func TestResticSummarize(t *testing.T) {
	t.Parallel()
	r := builtin(t)
	d := func(kv ...string) []jobkind.Detail {
		var out []jobkind.Detail
		for i := 0; i < len(kv); i += 2 {
			out = append(out, jobkind.Detail{Label: kv[i], Value: kv[i+1]})
		}
		return out
	}
	tests := []struct {
		name string
		kind string
		def  *syncrv1.JobDefinition
		out  *syncrv1.JobOutput
		want []jobkind.Detail
	}{
		{"backup", "restic_backup", backupDef(), &syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticBackup{ResticBackup: &syncrv1.ResticBackupOutput{
			SnapshotId: "1a2b3c4d5e6f7a8b", FilesNew: 12, FilesChanged: 3, FilesUnmodified: 1204, DirsNew: 1, DirsChanged: 4, DirsUnmodified: 80,
			DataAddedBytes: 1288490189, DataAddedPackedBytes: 880803840, TotalFilesProcessed: 1219, TotalBytesProcessed: 51646976819,
		}}}, d("Snapshot", "1a2b3c4d", "Files", "12 new, 3 changed, 1,204 unmodified", "Folders", "1 new, 4 changed, 80 unmodified",
			"Data added", "1.2 GiB (840.0 MiB stored)", "Processed", "48.1 GiB in 1,219 files")},
		{"backup dry run", "restic_backup", backupDef(), &syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticBackup{ResticBackup: &syncrv1.ResticBackupOutput{
			FilesNew: 1, TotalFilesProcessed: 1, TotalBytesProcessed: 10, DataAddedBytes: 10,
		}}}, d("Files", "1 new, 0 changed, 0 unmodified", "Folders", "0 new, 0 changed, 0 unmodified", "Data added", "10 bytes", "Processed", "10 bytes in 1 file")},
		{"retention", "restic_forget", forgetDef(), &syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticForget{ResticForget: &syncrv1.ResticForgetOutput{
			RemovedSnapshotIds: []string{"a", "b", "c", "d", "e"}, KeptCount: 23,
		}}}, d("Removed", "5 snapshots", "Kept", "23 snapshots")},
		{"retention dry run", "restic_forget", forgetDef(func(d *syncrv1.ResticForgetDefinition) { d.DryRun = true }), &syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticForget{ResticForget: &syncrv1.ResticForgetOutput{
			RemovedSnapshotIds: []string{"a"}, KeptCount: 1,
		}}}, d("Would remove", "1 snapshot", "Kept", "1 snapshot")},
		{"clean check", "restic_check", resticCheckDef(10), &syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticCheck{ResticCheck: &syncrv1.ResticCheckOutput{ReadDataPercent: 10}}},
			d("Result", "No errors found", "Data read", "10%")},
		{"check with hints", "restic_check", resticCheckDef(0), &syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticCheck{ResticCheck: &syncrv1.ResticCheckOutput{
			NumErrors: 2, SuggestRepairIndex: true, SuggestPrune: true, BrokenPacks: []string{"p1"},
		}}}, d("Result", "2 errors", "Broken packs", "1", "Hint", `Run "restic repair index"`, "Hint", "Prune suggested")},
		{"restore", "restic_restore", restoreDef(), &syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticRestore{ResticRestore: &syncrv1.ResticRestoreOutput{
			FilesRestored: 1204, BytesRestored: 2254857830, FilesSkipped: 12, BytesSkipped: 41943040,
		}}}, d("Restored", "1,204 files and folders (2.1 GiB)", "Skipped", "12 files and folders (40.0 MiB)")},
		{"restore with deletions", "restic_restore", restoreDef(), &syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticRestore{ResticRestore: &syncrv1.ResticRestoreOutput{
			FilesRestored: 1, FilesDeleted: 3,
		}}}, d("Restored", "1 file or folder (0 bytes)", "Skipped", "0 files and folders (0 bytes)", "Deleted", "3")},
		{"delete data", "restic_delete_data", deleteDataDef(), &syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticDeleteData{ResticDeleteData: &syncrv1.ResticDeleteDataOutput{
			DeletedObjects: 48213, RemovedParts: []string{"locks", "snapshots", "index", "data", "keys", "config"}, DirectoryRemoved: true,
		}}}, d("Deleted", "48,213 objects", "Removed", "locks, snapshots, index, data, keys, config", "Folder", "removed")},
		{"delete data, folder kept", "restic_delete_data", deleteDataDef(), &syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticDeleteData{ResticDeleteData: &syncrv1.ResticDeleteDataOutput{
			DeletedObjects: 1, RemovedParts: []string{"config"},
		}}}, d("Deleted", "1 object", "Removed", "config", "Folder", "kept (other files remain, or it is the remote's root)")},
		{"delete data, already gone", "restic_delete_data", deleteDataDef(), &syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticDeleteData{ResticDeleteData: &syncrv1.ResticDeleteDataOutput{AlreadyGone: true}}},
			d("Result", "Already gone")},
		{"no output", "restic_backup", backupDef(), nil, nil},
		{"another kind's output", "restic_check", resticCheckDef(0), &syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticBackup{ResticBackup: &syncrv1.ResticBackupOutput{}}}, nil},
		{"prune has no details", "restic_prune", pruneDef(false), &syncrv1.JobOutput{}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var raw []byte
			if tt.out != nil {
				var err error
				if raw, err = proto.Marshal(tt.out); err != nil {
					t.Fatal(err)
				}
			}
			if got := r.Summarize(tt.kind, tt.def, raw); !slices.Equal(got, tt.want) {
				t.Errorf("Summarize =\n%v\nwant\n%v", got, tt.want)
			}
		})
	}
}

func TestTreeToLocal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		tree, goos, want string
		ok               bool
	}{
		{"/srv/photos", "linux", "/srv/photos", true},
		{"/", "linux", "/", true},
		{"/srv/a b/[x]*?", "darwin", "/srv/a b/[x]*?", true},
		{"/C/Users/ada/Documents", "windows", `C:\Users\ada\Documents`, true},
		{"/c/Users", "windows", `C:\Users`, true},
		{"/D", "windows", `D:\`, true},
		{"/D/", "windows", `D:\`, true},
		{"/", "windows", "", false},               // the list of drives
		{"/server/share/x", "windows", "", false}, // a UNC snapshot: the user types the target
		{"/1/x", "windows", "", false},            // not a drive letter
		{"/C/a:b", "windows", "", false},          // not a Windows file name
		{"srv/photos", "linux", "", false},        // not a tree path
		{"/srv/../etc", "linux", "", false},       // ".."
		{"/C/Users/../Windows", "windows", "", false},
		{"", "linux", "", false},
	}
	for _, tt := range tests {
		got, ok := jobkind.TreeToLocal(tt.tree, tt.goos)
		if got != tt.want || ok != tt.ok {
			t.Errorf("TreeToLocal(%q, %s) = (%q, %v), want (%q, %v)", tt.tree, tt.goos, got, ok, tt.want, tt.ok)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	t.Parallel()
	for n, want := range map[int64]string{0: "0 bytes", 1: "1 byte", 1023: "1023 bytes", 1024: "1.0 KiB", 1536: "1.5 KiB", 5 << 30: "5.0 GiB", 1 << 62: "4.0 EiB"} {
		if got := jobkind.FormatBytes(n); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestValidSnapshotID(t *testing.T) {
	t.Parallel()
	for s, want := range map[string]bool{
		"1a2b3c4d": true, strings.Repeat("f", 64): true, "1a2b3c4": false, strings.Repeat("f", 65): false,
		"latest": false, "1A2B3C4D": false, "": false, "1a2b3c4d\n": false,
	} {
		if got := jobkind.ValidSnapshotID(s); got != want {
			t.Errorf("ValidSnapshotID(%q) = %v, want %v", s, got, want)
		}
	}
}

// The patterns are compiled once, not on every check.
func TestPatternsAreCompiledOnce(t *testing.T) {
	r, err := jobkind.NewRegistry(jobkind.ResticForget{})
	if err != nil {
		t.Fatal(err)
	}
	def := &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_ResticForget{ResticForget: &syncrv1.ResticForgetDefinition{
		RepositoryId: testRepoID, Policy: &syncrv1.ResticRetentionPolicy{KeepWithin: "1y6m"},
	}}}
	checks := map[string]func(){
		"ValidSnapshotID": func() { jobkind.ValidSnapshotID("1a2b3c4d") },
		"keep_within":     func() { _, _, _ = r.Check(def) },
	}
	for name, check := range checks {
		allocs := testing.AllocsPerRun(50, check)
		// Compiling a pattern costs dozens of allocations; the check without
		// it a few at most.
		if allocs > 15 {
			t.Errorf("%s allocates %.0f times per call: is a pattern compiled per call?", name, allocs)
		}
	}
}
