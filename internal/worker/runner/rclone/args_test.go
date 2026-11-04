package rclone

import (
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
)

const (
	modeCopy = syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_COPY
	modeSync = syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_SYNC
	modeMove = syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_MOVE
)

func ptr[T any](v T) *T { return &v }

func include(p string) *syncrv1.FilterRule {
	return &syncrv1.FilterRule{Action: syncrv1.FilterAction_FILTER_ACTION_INCLUDE, Pattern: p}
}

func exclude(p string) *syncrv1.FilterRule {
	return &syncrv1.FilterRule{Action: syncrv1.FilterAction_FILTER_ACTION_EXCLUDE, Pattern: p}
}

func TestBuildArgsDefaults(t *testing.T) {
	t.Parallel()
	for mode, cmd := range map[syncrv1.RcloneTransferMode]string{modeCopy: "copy", modeSync: "sync", modeMove: "move"} {
		got, err := buildArgs(mode, nil, "src:a", "/data/b")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{cmd, "--use-json-log", "--log-level=NOTICE", "--stats=1s", "--stats-log-level=NOTICE", "--", "src:a", "/data/b"}
		if !slices.Equal(got, want) {
			t.Errorf("buildArgs(%v) = %q, want %q", mode, got, want)
		}
	}
}

func TestBuildArgsAllOptions(t *testing.T) {
	t.Parallel()
	opts := &syncrv1.RcloneTransferOptions{
		Transfers:                    ptr[int32](8),
		Checkers:                     ptr[int32](16),
		BandwidthLimitBytesPerSecond: ptr[int64](1 << 20),
		DryRun:                       true,
		Checksum:                     true,
		IgnoreExisting:               true,
		Update:                       true,
		CreateEmptySrcDirs:           true,
		TrackRenames:                 true,
		FastList:                     true,
		MaxTransferBytes:             ptr[int64](5 << 30),
		MaxDuration:                  durationpb.New(90*time.Minute + 500*time.Millisecond),
		Retries:                      ptr[int32](1),
		LowLevelRetries:              ptr[int32](0),
		Filters:                      []*syncrv1.FilterRule{exclude("*.tmp"), include("/photos/**"), exclude("-rf --config x"), exclude("**")},
	}
	got, err := buildArgs(modeSync, opts, "a:", "b:")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"sync", "--use-json-log", "--log-level=NOTICE", "--stats=1s", "--stats-log-level=NOTICE",
		"--transfers=8", "--checkers=16", "--bwlimit=1048576B",
		"--dry-run", "--checksum", "--ignore-existing", "--update", "--create-empty-src-dirs", "--track-renames", "--fast-list",
		"--max-transfer=5368709120B", "--max-duration=1h30m0.5s", "--retries=1", "--low-level-retries=0",
		"--filter=- *.tmp", "--filter=+ /photos/**", "--filter=- -rf --config x", "--filter=- **",
		"--", "a:", "b:",
	}
	if !slices.Equal(got, want) {
		t.Errorf("buildArgs() =\n%q\nwant\n%q", got, want)
	}
}

func TestBuildArgsMoveOnlyAndUnlimited(t *testing.T) {
	t.Parallel()
	got, err := buildArgs(modeMove, &syncrv1.RcloneTransferOptions{
		DeleteEmptySrcDirs:           true,
		SizeOnly:                     true,
		BandwidthLimitBytesPerSecond: ptr[int64](0), // unlimited: no flag
	}, "a:x", "b:y")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got, "--delete-empty-src-dirs") || !slices.Contains(got, "--size-only") {
		t.Errorf("args = %q", got)
	}
	for _, a := range got {
		if strings.HasPrefix(a, "--bwlimit") {
			t.Errorf("bandwidth 0 produced %q", a)
		}
	}
}

func TestBuildArgsPathsFollowDoubleDash(t *testing.T) {
	t.Parallel()
	got, err := buildArgs(modeCopy, nil, "src:-rf", "dst:--config=/etc/x")
	if err != nil {
		t.Fatal(err)
	}
	i := slices.Index(got, "--")
	if i < 0 || !slices.Equal(got[i+1:], []string{"src:-rf", "dst:--config=/etc/x"}) {
		t.Errorf("args = %q", got)
	}
}

func TestBuildArgsRejects(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		mode syncrv1.RcloneTransferMode
		opts *syncrv1.RcloneTransferOptions
	}{
		"unspecified mode":            {0, nil},
		"unknown mode":                {99, nil},
		"zero transfers":              {modeCopy, &syncrv1.RcloneTransferOptions{Transfers: ptr[int32](0)}},
		"too many transfers":          {modeCopy, &syncrv1.RcloneTransferOptions{Transfers: ptr[int32](maxTransfers + 1)}},
		"negative checkers":           {modeCopy, &syncrv1.RcloneTransferOptions{Checkers: ptr[int32](-1)}},
		"too many checkers":           {modeCopy, &syncrv1.RcloneTransferOptions{Checkers: ptr[int32](maxCheckers + 1)}},
		"negative bandwidth":          {modeCopy, &syncrv1.RcloneTransferOptions{BandwidthLimitBytesPerSecond: ptr[int64](-1)}},
		"checksum and size_only":      {modeCopy, &syncrv1.RcloneTransferOptions{Checksum: true, SizeOnly: true}},
		"delete_empty_src_dirs copy":  {modeCopy, &syncrv1.RcloneTransferOptions{DeleteEmptySrcDirs: true}},
		"delete_empty_src_dirs sync":  {modeSync, &syncrv1.RcloneTransferOptions{DeleteEmptySrcDirs: true}},
		"track_renames copy":          {modeCopy, &syncrv1.RcloneTransferOptions{TrackRenames: true}},
		"track_renames move":          {modeMove, &syncrv1.RcloneTransferOptions{TrackRenames: true}},
		"zero max transfer":           {modeCopy, &syncrv1.RcloneTransferOptions{MaxTransferBytes: ptr[int64](0)}},
		"negative max duration":       {modeCopy, &syncrv1.RcloneTransferOptions{MaxDuration: durationpb.New(-time.Second)}},
		"zero max duration":           {modeCopy, &syncrv1.RcloneTransferOptions{MaxDuration: durationpb.New(0)}},
		"huge max duration":           {modeCopy, &syncrv1.RcloneTransferOptions{MaxDuration: durationpb.New(maxDuration + time.Second)}},
		"invalid max duration":        {modeCopy, &syncrv1.RcloneTransferOptions{MaxDuration: &durationpb.Duration{Seconds: 1, Nanos: -1}}},
		"zero retries":                {modeCopy, &syncrv1.RcloneTransferOptions{Retries: ptr[int32](0)}},
		"too many retries":            {modeCopy, &syncrv1.RcloneTransferOptions{Retries: ptr[int32](maxRetries + 1)}},
		"negative low-level retries":  {modeCopy, &syncrv1.RcloneTransferOptions{LowLevelRetries: ptr[int32](-1)}},
		"too many low-level retries":  {modeCopy, &syncrv1.RcloneTransferOptions{LowLevelRetries: ptr[int32](maxLowLevelRetries + 1)}},
		"filter without action":       {modeCopy, &syncrv1.RcloneTransferOptions{Filters: []*syncrv1.FilterRule{{Pattern: "x"}}}},
		"filter with unknown action":  {modeCopy, &syncrv1.RcloneTransferOptions{Filters: []*syncrv1.FilterRule{{Action: 7, Pattern: "x"}}}},
		"empty filter pattern":        {modeCopy, &syncrv1.RcloneTransferOptions{Filters: []*syncrv1.FilterRule{include("  ")}}},
		"filter pattern with newline": {modeCopy, &syncrv1.RcloneTransferOptions{Filters: []*syncrv1.FilterRule{include("a\n- /etc")}}},
		"filter pattern with CR":      {modeCopy, &syncrv1.RcloneTransferOptions{Filters: []*syncrv1.FilterRule{include("a\rb")}}},
		"filter pattern with NUL":     {modeCopy, &syncrv1.RcloneTransferOptions{Filters: []*syncrv1.FilterRule{include("a\x00b")}}},
		"filter pattern too long":     {modeCopy, &syncrv1.RcloneTransferOptions{Filters: []*syncrv1.FilterRule{include(strings.Repeat("x", maxPatternBytes+1))}}},
		"too many filters":            {modeCopy, &syncrv1.RcloneTransferOptions{Filters: slices.Repeat([]*syncrv1.FilterRule{include("x")}, maxFilters+1)}},
	}
	for name, tt := range tests {
		if got, err := buildArgs(tt.mode, tt.opts, "a:", "b:"); err == nil {
			t.Errorf("%s: buildArgs() = %q, want an error", name, got)
		}
	}
}

func TestBuildArgsReportsAllProblems(t *testing.T) {
	t.Parallel()
	_, err := buildArgs(modeCopy, &syncrv1.RcloneTransferOptions{Transfers: ptr[int32](0), Retries: ptr[int32](0)}, "a:", "b:")
	if err == nil || !strings.Contains(err.Error(), "transfers") || !strings.Contains(err.Error(), "retries") {
		t.Errorf("err = %v, want both problems", err)
	}
}
