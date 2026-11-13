package restic

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// replay feeds fixture lines to a new session of kind and returns it with
// the progress snapshots it emitted.
func replay(t *testing.T, kind string, files ...string) (*session, []runner.Progress) {
	t.Helper()
	var snaps []runner.Progress
	s := newSession(kind, func(p runner.Progress) { snaps = append(snaps, p) }, nil)
	for _, f := range files {
		for _, l := range fixture(t, f) {
			s.line(proc.Line{Data: []byte(l)})
		}
	}
	return s, snaps
}

func TestBackupFixture(t *testing.T) {
	t.Parallel()
	s, snaps := replay(t, kindBackup, "backup.out")
	if len(snaps) != 3 {
		t.Fatalf("emitted %d snapshots, want 3 (2 status + summary)", len(snaps))
	}
	last := snaps[len(snaps)-1]
	if last.Bytes != 24000006 || last.TotalBytes != 24000006 || last.Files != 2 || last.TotalFiles != 2 {
		t.Errorf("final progress = %+v", last)
	}
	out, err := s.output(nil)
	if err != nil {
		t.Fatal(err)
	}
	got := out.GetResticBackup()
	if got.GetSnapshotId() != "948693b6b1b380704c307d1746e8b2b7d40369f114b769b5137413675f86e5a2" ||
		got.GetDataAddedBytes() != 24002922 || got.GetFilesNew() != 2 || got.GetFilesChanged() != 0 || got.GetFilesUnmodified() != 0 {
		t.Errorf("output = %v", got)
	}
}

func TestBackupProgressIsCumulative(t *testing.T) {
	t.Parallel()
	var snaps []runner.Progress
	s := newSession(kindBackup, func(p runner.Progress) { snaps = append(snaps, p) }, nil)
	for _, l := range []string{
		`{"message_type":"status","total_files":3,"total_bytes":30}`, // scan only
		`{"message_type":"status","total_files":5,"files_done":1,"total_bytes":50,"bytes_done":10}`,
		`{"message_type":"status","total_files":5,"files_done":0,"total_bytes":40,"bytes_done":5}`, // regression: ignored
		`{"message_type":"status","total_files":5,"files_done":4,"total_bytes":50,"bytes_done":40}`,
	} {
		s.line(proc.Line{Data: []byte(l)})
	}
	want := []runner.Progress{
		{TotalFiles: 3, TotalBytes: 30},
		{TotalFiles: 5, TotalBytes: 50, Files: 1, Bytes: 10},
		{TotalFiles: 5, TotalBytes: 50, Files: 1, Bytes: 10},
		{TotalFiles: 5, TotalBytes: 50, Files: 4, Bytes: 40},
	}
	for i := range snaps {
		snaps[i].Elapsed = 0
	}
	if !slices.Equal(snaps, want) {
		t.Errorf("snapshots = %+v\nwant %+v", snaps, want)
	}
}

func TestBackupDryRunHasNoSnapshotID(t *testing.T) {
	t.Parallel()
	s, _ := replay(t, kindBackup, "backup_dry.out")
	out, err := s.output(nil)
	if err != nil {
		t.Fatal(err)
	}
	if id := out.GetResticBackup().GetSnapshotId(); id != "" {
		t.Errorf("dry-run snapshot ID = %q, want empty", id)
	}
	if out.GetResticBackup().GetFilesUnmodified() != 2 || out.GetResticBackup().GetDataAddedBytes() != 352 {
		t.Error("dry-run counters were dropped")
	}
}

func TestBackupWarningFixtureCountsEachItemOnce(t *testing.T) {
	t.Parallel()
	// The fixture reports the same directory in the "scan" and "archival"
	// phases: one problem.
	s, snaps := replay(t, kindBackup, "backup_warn.out", "backup_warn.stderr")
	if s.stats().Errors != 1 {
		t.Errorf("Errors = %d, want 1", s.stats().Errors)
	}
	if len(snaps) == 0 || snaps[len(snaps)-1].Errors != 1 {
		t.Errorf("snapshots = %+v", snaps)
	}
	if s.exit == nil || s.exit.Message == "" {
		t.Error("exit_error message not captured")
	}
}

func TestItemErrorsAreBounded(t *testing.T) {
	t.Parallel()
	s := newSession(kindBackup, func(runner.Progress) {}, nil)
	report := func(item string) {
		line := fmt.Sprintf(`{"message_type":"error","error":{"message":"permission denied"},"during":"archival","item":%q}`, item)
		s.line(proc.Line{Data: []byte(line)})
	}
	const n = maxErrItems + 500
	for i := range n {
		report(fmt.Sprintf("/src/f%d", i))
	}
	for i := range 10 {
		report(fmt.Sprintf("/src/f%d", i)) // seen before: not counted again
	}
	if len(s.errItems) != maxErrItems {
		t.Errorf("remembered %d failing paths, want at most %d", len(s.errItems), maxErrItems)
	}
	if got := s.stats().Errors; got != n {
		t.Errorf("Errors = %d, want %d", got, n)
	}
	if len(s.itemErrors) != maxItemErrors {
		t.Errorf("kept %d messages, want %d", len(s.itemErrors), maxItemErrors)
	}
}

func TestRestoreFixture(t *testing.T) {
	t.Parallel()
	s, snaps := replay(t, kindRestore, "restore.out")
	if len(snaps) != 2 {
		t.Fatalf("emitted %d snapshots, want 2", len(snaps))
	}
	if first := snaps[0]; first.Files != 1 || first.Bytes != 11 || first.TotalFiles != 6 || first.TotalBytes != 24000011 {
		t.Errorf("status progress = %+v", first)
	}
	if last := snaps[1]; last.Files != 6 || last.Bytes != 24000011 {
		t.Errorf("summary progress = %+v", last)
	}
	out, err := s.output(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := out.GetResticRestore(); got.GetTotalFiles() != 6 || got.GetFilesRestored() != 6 ||
		got.GetTotalBytes() != 24000011 || got.GetBytesRestored() != 24000011 || got.GetFilesSkipped() != 0 {
		t.Errorf("output = %v", got)
	}
}

func TestRestoreCountsSkippedAsProcessed(t *testing.T) {
	t.Parallel()
	s := newSession(kindRestore, func(runner.Progress) {}, nil)
	s.line(proc.Line{Data: []byte(`{"message_type":"summary","total_files":10,"files_restored":4,"files_skipped":6,"total_bytes":100,"bytes_restored":40,"bytes_skipped":60}`)})
	if st := s.stats(); st.Files != 10 || st.Bytes != 100 || st.TotalFiles != 10 {
		t.Errorf("stats = %+v", st)
	}
}

func TestForgetFixture(t *testing.T) {
	t.Parallel()
	for _, f := range []string{"forget.out", "forget_dry.out"} {
		t.Run(f, func(t *testing.T) {
			t.Parallel()
			s, snaps := replay(t, kindForget, f)
			if len(snaps) != 0 {
				t.Errorf("forget emitted progress: %+v", snaps)
			}
			out, err := s.output(nil)
			if err != nil {
				t.Fatal(err)
			}
			ids := out.GetResticForget().GetRemovedSnapshotIds()
			if len(ids) != 1 || ids[0] != "948693b6b1b380704c307d1746e8b2b7d40369f114b769b5137413675f86e5a2" {
				t.Errorf("removed = %v, want the first snapshot", ids)
			}
		})
	}
}

func TestForgetTruncatedKeepsDecodedGroups(t *testing.T) {
	t.Parallel()
	s := newSession(kindForget, func(runner.Progress) {}, nil)
	whole := `[{"host":"a","remove":[{"id":"aaaa"},{"id":"bbbb"}],"keep":[]},{"host":"b","remove":[{"id":"cc`
	s.line(proc.Line{Data: []byte(whole), Truncated: true})
	out, err := s.output(nil)
	if err != nil {
		t.Fatal(err)
	}
	if ids := out.GetResticForget().GetRemovedSnapshotIds(); !slices.Equal(ids, []string{"aaaa", "bbbb"}) {
		t.Errorf("removed = %v", ids)
	}

	// Without truncation the same garbage is an error.
	s = newSession(kindForget, func(runner.Progress) {}, nil)
	s.line(proc.Line{Data: []byte(whole)})
	if _, err := s.output(nil); err == nil {
		t.Error("malformed forget output accepted")
	}
}

func TestForgetNothingRemoved(t *testing.T) {
	t.Parallel()
	s := newSession(kindForget, func(runner.Progress) {}, nil)
	s.line(proc.Line{Data: []byte(`[{"host":"a","keep":[{"id":"x"}],"remove":null}]`)})
	out, err := s.output(nil)
	if err != nil || len(out.GetResticForget().GetRemovedSnapshotIds()) != 0 {
		t.Errorf("output = %v, %v", out, err)
	}
}

func TestSnapshotsFixture(t *testing.T) {
	t.Parallel()
	s, _ := replay(t, kindSnapshots, "snapshots.out")
	out, err := s.output(nil)
	if err != nil {
		t.Fatal(err)
	}
	snaps := out.GetResticSnapshots().GetSnapshots()
	if len(snaps) != 2 {
		t.Fatalf("got %d snapshots, want 2", len(snaps))
	}
	sn := snaps[0]
	want := time.Date(2026, 11, 4, 4, 4, 49, 468435653, time.UTC)
	if sn.GetId() != "948693b6b1b380704c307d1746e8b2b7d40369f114b769b5137413675f86e5a2" ||
		!sn.GetTime().AsTime().Equal(want) ||
		sn.GetHost() != "laptop" ||
		!slices.Equal(sn.GetPaths(), []string{"/srv/data/s2"}) ||
		!slices.Equal(sn.GetTags(), []string{"nightly"}) {
		t.Errorf("snapshot = %v", sn)
	}
}

func TestSnapshotsEmptyAndTruncated(t *testing.T) {
	t.Parallel()
	s, _ := replay(t, kindSnapshots, "snapshots_empty.out")
	out, err := s.output(nil)
	if err != nil || out.GetResticSnapshots() == nil || len(out.GetResticSnapshots().GetSnapshots()) != 0 {
		t.Errorf("empty listing: %v, %v", out, err)
	}

	s = newSession(kindSnapshots, func(runner.Progress) {}, nil)
	s.line(proc.Line{Data: []byte(`[{"id":"aaaa","time":"2026-01-01T00:00:00Z"},{"id":"bb`), Truncated: true})
	_, err = s.output(nil)
	if err == nil || !strings.Contains(err.Error(), "line limit") {
		t.Errorf("truncated listing err = %v, want a line limit error", err)
	}
}

func TestSnapshotsMissingResultIsError(t *testing.T) {
	t.Parallel()
	s := newSession(kindSnapshots, func(runner.Progress) {}, nil)
	if _, err := s.output(nil); err == nil {
		t.Error("no result accepted")
	}
}

func TestCheckFixtureAndFailure(t *testing.T) {
	t.Parallel()
	s, _ := replay(t, kindCheck, "check.out")
	if s.checkErrors == nil || *s.checkErrors != 0 {
		t.Errorf("checkErrors = %v, want 0", s.checkErrors)
	}

	s = newSession(kindCheck, func(runner.Progress) {}, nil)
	s.line(proc.Line{Data: []byte(`{"message_type":"summary","num_errors":3,"broken_packs":["ab"],"suggest_repair_index":true}`)})
	if s.stats().Errors != 3 {
		t.Errorf("Errors = %d, want 3", s.stats().Errors)
	}
	if d := s.detail(); !strings.Contains(d, "3 error(s)") {
		t.Errorf("detail = %q", d)
	}
}

func TestDetailAddsRcloneErrors(t *testing.T) {
	t.Parallel()
	const (
		fatal  = `{"message_type":"exit_error","code":1,"message":"Fatal: create repository at rclone:r1:b failed: unexpected HTTP response (500): 500 Internal Server Error"}`
		headed = "Fatal: create repository at rclone:r1:b failed: unexpected HTTP response (500): 500 Internal Server Error"
	)
	tests := []struct {
		name   string
		stdout []string
		stderr []string
		want   string
	}{
		{"error without its timestamp", nil, []string{
			"rclone: 2026/11/13 03:02:24 ERROR : Create repo failed to Mkdir: operation error S3: CreateBucket, https response error StatusCode: 403, api error AccessDenied: Access Denied.",
			fatal,
		}, headed + "; rclone: Create repo failed to Mkdir: operation error S3: CreateBucket, https response error StatusCode: 403, api error AccessDenied: Access Denied."},
		{"critical, fractional seconds, no timestamp, CRLF", nil, []string{
			`rclone: 2026/11/13 03:07:17 CRITICAL: Failed to create file system for "r1:b": didn't find backend called "nope"` + "\r",
			"rclone: 2026/11/13 03:07:17.123456 ERROR : a: second",
			"rclone: ERROR : third",
			fatal,
		}, headed + "; rclone: a: second; rclone: third"},
		{"other levels and restic's own lines", nil, []string{
			"rclone: 2026/11/13 03:02:24 NOTICE: Config file \"\" not found - using defaults",
			"rclone: 2026/11/13 03:02:24 INFO  : copied",
			"rclone: 2026/11/13 03:02:24 DEBUG : head config",
			"ERROR : not from rclone",
			"Save(<data/ab>) returned error, retrying after 1s: unexpected HTTP response (500)",
			fatal,
		}, headed},
		{"retries are named once", nil, []string{
			"rclone: 2026/11/13 03:02:24 ERROR : data/ab: put: 403",
			"rclone: 2026/11/13 03:02:25 ERROR : data/ab: put: 403",
			"rclone: 2026/11/13 03:02:27 ERROR : data/ab: put: 403",
			fatal,
		}, headed + "; rclone: data/ab: put: 403"},
		{"standard output is not rclone's", []string{
			"rclone: 2026/11/13 03:02:24 ERROR : on stdout",
		}, []string{fatal}, headed},
		{"without an exit_error the diagnostic lines already hold them", nil, []string{
			"rclone: 2026/11/13 03:02:24 ERROR : once",
		}, "rclone: 2026/11/13 03:02:24 ERROR : once"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newSession(kindInit, func(runner.Progress) {}, nil)
			for _, l := range tc.stdout {
				s.stdout(proc.Line{Data: []byte(l)})
			}
			for _, l := range tc.stderr {
				s.stderr(proc.Line{Data: []byte(l)})
			}
			if got := s.detail(); got != tc.want {
				t.Errorf("detail =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

func TestRcloneErrorsAreBounded(t *testing.T) {
	t.Parallel()
	s := newSession(kindInit, func(runner.Progress) {}, nil)
	for i := range maxRcloneErrors + 3 {
		s.stderr(proc.Line{Data: fmt.Appendf(nil, "rclone: 2026/11/13 03:02:24 ERROR : error %d", i)})
	}
	want := make([]string, 0, maxRcloneErrors)
	for i := 3; i < maxRcloneErrors+3; i++ {
		want = append(want, fmt.Sprintf("rclone: error %d", i))
	}
	if !slices.Equal(s.rcloneErrors, want) {
		t.Errorf("rcloneErrors = %q, want the last %d: %q", s.rcloneErrors, maxRcloneErrors, want)
	}
}

func TestInitAndPruneFixturesAreAccepted(t *testing.T) {
	t.Parallel()
	s, snaps := replay(t, kindInit, "init.out")
	if s.initialized == nil || s.initialized.ID == "" || len(snaps) != 0 {
		t.Errorf("init: %+v, %d snapshots", s.initialized, len(snaps))
	}
	// prune prints plain text in 0.19: nothing is parsed and nothing breaks.
	s, snaps = replay(t, kindPrune, "prune.out")
	if len(snaps) != 0 || len(s.tail) == 0 {
		t.Errorf("prune: %d snapshots, tail %v", len(snaps), s.tail)
	}
	if out, err := s.output(nil); out != nil || err != nil {
		t.Errorf("prune output = %v, %v", out, err)
	}
}

func TestUnknownAndMalformedMessagesAreIgnored(t *testing.T) {
	t.Parallel()
	s := newSession(kindBackup, func(runner.Progress) {}, nil)
	for _, l := range []string{
		``,
		`   `,
		`{"message_type":"from_the_future","bytes_done":"not a number","x":{"y":[1,2,3]}}`,
		`{"message_type":"verbose_status","action":"new","item":"/a","duration":0.1,"data_size":1}`,
		`{"no_message_type":true}`,
		`{"message_type":"status","brand_new_field":123,"bytes_done":7,"files_done":1}`,
		`{truncated`,
		`plain text`,
		`123`,
		`null`,
		`"string"`,
		`{"message_type":"summary","snapshot_id":"abc","files_new":1,"new_counter":5}`,
	} {
		s.line(proc.Line{Data: []byte(l)})
	}
	if st := s.stats(); st.Bytes != 7 || st.Files != 1 {
		t.Errorf("stats = %+v, want the one valid status", st)
	}
	out, err := s.output(nil)
	if err != nil || out.GetResticBackup().GetSnapshotId() != "abc" {
		t.Errorf("output = %v, %v", out, err)
	}
}

func TestCRLFLinesAreParsed(t *testing.T) {
	t.Parallel()
	s := newSession(kindBackup, func(runner.Progress) {}, nil)
	s.line(proc.Line{Data: []byte("{\"message_type\":\"status\",\"bytes_done\":5}\r")})
	if s.stats().Bytes != 5 {
		t.Errorf("stats = %+v", s.stats())
	}
}

func TestEmitIsCalledFromRunOnly(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	te.sup.exited(0).stdout = fixture(t, "backup.out")
	res, snaps, err := te.run(backupSpec(remoteRepo(), &syncrv1.ResticBackup{Paths: []string{te.local("a")}}))
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 3 {
		t.Errorf("emitted %d, want 3", len(snaps))
	}
	if res.Stats.Bytes != 24000006 || res.Stats.Elapsed < 0 {
		t.Errorf("stats = %+v", res.Stats)
	}
	if res.Output.GetResticBackup().GetSnapshotId() == "" {
		t.Error("no output")
	}
}
