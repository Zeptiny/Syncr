package restic

import (
	"slices"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

func restoreSpec(te *testEnv, op *syncrv1.ResticRestore) *syncrv1.JobSpec {
	op.Repository = remoteRepo()
	if op.Target == "" {
		op.Target = te.local("restore")
	}
	return remoteSpec(func(s *syncrv1.JobSpec) { s.Operation = &syncrv1.JobSpec_ResticRestore{ResticRestore: op} })
}

func TestRestoreArgs(t *testing.T) {
	t.Parallel()
	never := syncrv1.ResticRestoreOverwrite_RESTIC_RESTORE_OVERWRITE_NEVER
	tests := []struct {
		name string
		goos string
		op   *syncrv1.ResticRestore
		want []string // after --target
	}{
		{"overwrite never", "linux", &syncrv1.ResticRestore{SnapshotId: "latest", Overwrite: never},
			[]string{"--overwrite=never", "--", "latest"}},
		{"overwrite always", "linux", &syncrv1.ResticRestore{SnapshotId: "latest", Overwrite: syncrv1.ResticRestoreOverwrite_RESTIC_RESTORE_OVERWRITE_ALWAYS},
			[]string{"--overwrite=always", "--", "latest"}},
		{"overwrite if changed", "linux", &syncrv1.ResticRestore{SnapshotId: "latest", Overwrite: syncrv1.ResticRestoreOverwrite_RESTIC_RESTORE_OVERWRITE_IF_CHANGED},
			[]string{"--overwrite=if-changed", "--", "latest"}},
		{"overwrite if newer", "linux", &syncrv1.ResticRestore{SnapshotId: "latest", Overwrite: syncrv1.ResticRestoreOverwrite_RESTIC_RESTORE_OVERWRITE_IF_NEWER},
			[]string{"--overwrite=if-newer", "--", "latest"}},
		{"unspecified adds no flag", "linux", &syncrv1.ResticRestore{SnapshotId: "latest"},
			[]string{"--", "latest"}},
		{"subfolder", "linux", &syncrv1.ResticRestore{SnapshotId: "1a2b3c4d", Subfolder: "/srv/photos/2025/", Overwrite: never},
			[]string{"--overwrite=never", "--", "1a2b3c4d:/srv/photos/2025"}},
		{"windows subfolder keeps the tree form", "windows", &syncrv1.ResticRestore{SnapshotId: "1a2b3c4d", Subfolder: "/C/Users/ada"},
			[]string{"--", "1a2b3c4d:/C/Users/ada"}},
		{"selected paths", "linux", &syncrv1.ResticRestore{
			SnapshotId: "1a2b3c4d", Paths: []string{"/srv/docs", "/srv/[x]*?.txt", `/srv/back\slash`}, Excludes: []string{"*.log"}, Verify: true,
		}, []string{"--include=/srv/docs", "--include=/srv/[[]x][*][?].txt", `--include=/srv/back[\\]slash`, "--exclude=*.log", "--verify", "--", "1a2b3c4d"}},
		{"selected paths on windows", "windows", &syncrv1.ResticRestore{SnapshotId: "1a2b3c4d", Paths: []string{"/C/Users/ada/a*b"}},
			[]string{"--include=/C/Users/ada/a[*]b", "--", "1a2b3c4d"}},
		{"selected paths of a subfolder", "linux", &syncrv1.ResticRestore{
			SnapshotId: "1a2b3c4d", Subfolder: "/srv/photos/", Paths: []string{"/srv/photos/2026/01", "/srv/photos/[x]*?.txt", `/srv/photos/back\slash`}, Overwrite: never,
		}, []string{"--overwrite=never", "--include=/2026/01", "--include=/[[]x][*][?].txt", `--include=/back[\\]slash`, "--", "1a2b3c4d:/srv/photos"}},
		{"selected paths of the root", "linux", &syncrv1.ResticRestore{SnapshotId: "1a2b3c4d", Subfolder: "/", Paths: []string{"/srv", "/home/ada"}},
			[]string{"--include=/srv", "--include=/home/ada", "--", "1a2b3c4d:/"}},
		{"selected paths of a windows subfolder", "windows", &syncrv1.ResticRestore{
			SnapshotId: "1a2b3c4d", Subfolder: "/C/Users/ada", Paths: []string{"/C/Users/ada/Documents/a*b", "/C/Users/ada/notes.txt"},
		}, []string{"--include=/Documents/a[*]b", "--include=/notes.txt", "--", "1a2b3c4d:/C/Users/ada"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			te := newTestEnv(t)
			setGOOS(t, te, tc.goos)
			te.sup.exited(0)
			if _, _, err := te.run(restoreSpec(te, tc.op)); err != nil {
				t.Fatal(err)
			}
			want := append(append(remoteGlobals(true), "restore", "--target="+te.local("restore")), tc.want...)
			if !slices.Equal(te.sup.spec.Args, want) {
				t.Errorf("args =\n  %q\nwant\n  %q", te.sup.spec.Args, want)
			}
		})
	}
}

func TestRestoreRejectsBadInput(t *testing.T) {
	t.Parallel()
	for name, op := range map[string]*syncrv1.ResticRestore{
		"path outside the subfolder":         {SnapshotId: "latest", Subfolder: "/srv", Paths: []string{"/srvx/a"}},
		"path that is the subfolder":         {SnapshotId: "latest", Subfolder: "/srv", Paths: []string{"/srv/"}},
		"path climbing out of the subfolder": {SnapshotId: "latest", Subfolder: "/srv", Paths: []string{"/srv/../etc"}},
		"relative subfolder":                 {SnapshotId: "latest", Subfolder: "srv"},
		"subfolder with ..":                  {SnapshotId: "latest", Subfolder: "/srv/../etc"},
		"relative path":                      {SnapshotId: "latest", Paths: []string{"a"}},
		"path with ..":                       {SnapshotId: "latest", Paths: []string{"/a/../b"}},
		"path with a line break":             {SnapshotId: "latest", Paths: []string{"/a\nb"}},
		"unknown overwrite value":            {SnapshotId: "latest", Overwrite: 99},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			te := newTestEnv(t)
			_, _, err := te.run(restoreSpec(te, op))
			mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC)
			if te.sup.called != 0 {
				t.Error("restic started for a rejected spec")
			}
		})
	}
}

func TestIncludePattern(t *testing.T) {
	t.Parallel()
	tests := []struct{ tree, goos, want string }{
		{"/srv/photos", "linux", "/srv/photos"},
		{"/srv/photos/", "linux", "/srv/photos"},
		{"/srv/a b/ü.txt", "linux", "/srv/a b/ü.txt"},
		{"/srv/[x]*?.txt", "linux", "/srv/[[]x][*][?].txt"},
		{`/srv/a\b`, "linux", `/srv/a[\\]b`},
		{`/srv/a\b`, "darwin", `/srv/a[\\]b`},
		{"/C/Users/ada/Documents", "windows", "/C/Users/ada/Documents"},
		{"/C/Users/[ada]", "windows", "/C/Users/[[]ada]"},
		{"/", "linux", "/"},
	}
	for _, tc := range tests {
		if got := includePattern(tc.tree, tc.goos); got != tc.want {
			t.Errorf("includePattern(%q, %s) = %q, want %q", tc.tree, tc.goos, got, tc.want)
		}
	}
}

func forgetSpec(pol *syncrv1.ResticRetentionPolicy) *syncrv1.JobSpec {
	return remoteSpec(func(s *syncrv1.JobSpec) {
		s.Operation = &syncrv1.JobSpec_ResticForget{ResticForget: &syncrv1.ResticForget{Repository: remoteRepo(), Policy: pol}}
	})
}

func TestForgetKeepWithin(t *testing.T) {
	t.Parallel()
	// keep_within counts as a rule on its own.
	te := newTestEnv(t)
	te.sup.exited(0).stdout = fixture(t, "forget_keep.out")
	res, _, err := te.run(forgetSpec(&syncrv1.ResticRetentionPolicy{KeepWithin: "1y6m", KeepLast: 1}))
	if err != nil {
		t.Fatal(err)
	}
	if want := append(remoteGlobals(true), "forget", "--keep-last=1", "--keep-within=1y6m"); !slices.Equal(te.sup.spec.Args, want) {
		t.Errorf("args = %q\nwant %q", te.sup.spec.Args, want)
	}
	if out := res.Output.GetResticForget(); out.GetKeptCount() != 2 || len(out.GetRemovedSnapshotIds()) != 0 {
		t.Errorf("output = %v, want 2 kept and none removed", out)
	}

	te = newTestEnv(t)
	te.sup.exited(0).stdout = []string{"[]"}
	if _, _, err := te.run(forgetSpec(&syncrv1.ResticRetentionPolicy{KeepWithin: "14d"})); err != nil {
		t.Errorf("keep_within alone: %v", err)
	}

	for _, bad := range []string{"14", "d", "1w", "1y 6m", "0d", "--prune", "1d\n"} {
		te := newTestEnv(t)
		_, _, err := te.run(forgetSpec(&syncrv1.ResticRetentionPolicy{KeepWithin: bad}))
		mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC)
		if te.sup.called != 0 {
			t.Errorf("restic started for keep_within %q", bad)
		}
	}
}

func TestBackupSummaryFixture(t *testing.T) {
	t.Parallel()
	s, _ := replay(t, kindBackup, "backup_summary.out")
	out, err := s.output(nil)
	if err != nil {
		t.Fatal(err)
	}
	got := out.GetResticBackup()
	if got.GetFilesNew() != 0 || got.GetFilesChanged() != 1 || got.GetFilesUnmodified() != 3 ||
		got.GetDirsNew() != 0 || got.GetDirsChanged() == 0 || got.GetDirsUnmodified() != 1 ||
		got.GetDataAddedBytes() == 0 || got.GetDataAddedPackedBytes() == 0 || got.GetDataAddedPackedBytes() >= got.GetDataAddedBytes() ||
		got.GetTotalFilesProcessed() != 4 || got.GetTotalBytesProcessed() != 10 || got.GetSnapshotId() == "" {
		t.Errorf("output = %v", got)
	}
}

func TestRestoreSummaryFixture(t *testing.T) {
	t.Parallel()
	s, _ := replay(t, kindRestore, "restore_summary.out")
	out, err := s.output(nil)
	if err != nil {
		t.Fatal(err)
	}
	want := &syncrv1.ResticRestoreOutput{TotalFiles: 5, FilesRestored: 5, FilesSkipped: 1, TotalBytes: 6, BytesRestored: 6, BytesSkipped: 4}
	got := out.GetResticRestore()
	if got.GetTotalFiles() != want.GetTotalFiles() || got.GetFilesRestored() != want.GetFilesRestored() || got.GetFilesSkipped() != want.GetFilesSkipped() ||
		got.GetTotalBytes() != want.GetTotalBytes() || got.GetBytesRestored() != want.GetBytesRestored() || got.GetBytesSkipped() != want.GetBytesSkipped() {
		t.Errorf("output = %v, want %v", got, want)
	}
}

func TestRestoreWithoutSummaryHasNoOutput(t *testing.T) {
	t.Parallel()
	s := newSession(kindRestore, func(runner.Progress) {}, nil)
	if out, err := s.output(nil); err != nil || out != nil {
		t.Errorf("output = %v, %v", out, err)
	}
}

func TestCheckSummaryFixture(t *testing.T) {
	t.Parallel()
	s, _ := replay(t, kindCheck, "check_summary.out")
	spec := &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_ResticCheck{ResticCheck: &syncrv1.ResticCheck{ReadDataPercent: 100}}}
	out, err := s.output(spec)
	if err != nil {
		t.Fatal(err)
	}
	got := out.GetResticCheck()
	if got == nil || got.GetNumErrors() != 0 || got.GetSuggestPrune() || got.GetSuggestRepairIndex() || len(got.GetBrokenPacks()) != 0 || got.GetReadDataPercent() != 100 {
		t.Errorf("output = %v", got)
	}

	s = newSession(kindCheck, func(runner.Progress) {}, nil)
	s.line(proc.Line{Data: []byte(`{"message_type":"summary","num_errors":0,"broken_packs":["aa","bb"],"suggest_repair_index":true,"suggest_prune":true}`)})
	out, _ = s.output(spec)
	if got := out.GetResticCheck(); !got.GetSuggestPrune() || !got.GetSuggestRepairIndex() || !slices.Equal(got.GetBrokenPacks(), []string{"aa", "bb"}) {
		t.Errorf("hints = %v", got)
	}
}

func TestRepositoryConfigParsing(t *testing.T) {
	t.Parallel()
	feed := func(lines ...string) *session {
		s := newSession(kindProbeConfig, func(runner.Progress) {}, nil)
		s.collect = true
		for _, l := range lines {
			s.stdout(proc.Line{Data: []byte(l)})
		}
		return s
	}
	cfg, err := feed(fixture(t, "cat_config.out")...).repositoryConfig()
	if err != nil || cfg.GetVersion() != 2 || cfg.GetId() != expectedResticID {
		t.Errorf("config = %v, %v", cfg, err)
	}
	for name, lines := range map[string][]string{
		"empty":      nil,
		"not json":   {"Fatal: nope"},
		"no id":      {`{"version":2}`},
		"short id":   {`{"version":2,"id":"abcd"}`},
		"exit error": {`{"message_type":"exit_error","code":12,"message":"Fatal: wrong password"}`},
	} {
		if cfg, err := feed(lines...).repositoryConfig(); err == nil {
			t.Errorf("%s: accepted as %v", name, cfg)
		}
	}
	// Output past the bound is refused, not parsed in part.
	big := feed(`{"version":2,"id":"`+expectedResticID+`",`, `"pad":"`+string(make([]byte, maxRawBytes))+`"}`)
	if _, err := big.repositoryConfig(); err == nil || !big.rawTruncated {
		t.Errorf("oversized config accepted (truncated=%v)", big.rawTruncated)
	}
}
