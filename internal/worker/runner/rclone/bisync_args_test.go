package rclone

import (
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
)

var testState = bisyncState{workDir: "/var/lib/syncr/bisync/k/work", filtersFile: "/var/lib/syncr/bisync/k/filters.txt"}

// bisyncFixed are the arguments every two-way sync starts with.
func bisyncFixed(st bisyncState) []string {
	return []string{
		"bisync", "--use-json-log", "--log-level=NOTICE", "--stats=1s", "--stats-log-level=NOTICE", "--color=NEVER",
		"--workdir=" + st.workDir, "--filters-file=" + st.filtersFile, "--recover", "--resilient",
	}
}

func TestBisyncArgs(t *testing.T) {
	t.Parallel()
	defaults := []string{"--conflict-resolve=none", "--conflict-loser=num", "--max-delete=50"}
	resync := testState
	resync.resync = true
	force := testState
	force.force = true
	tests := []struct {
		name  string
		opts  *syncrv1.RcloneBisyncOptions
		state bisyncState
		want  []string // after the fixed arguments, before "--"
	}{
		{"defaults", nil, testState, defaults},
		{"empty options", &syncrv1.RcloneBisyncOptions{}, testState, defaults},
		{"resync keeps the newer file", nil, resync, append([]string{"--resync", "--resync-mode=newer"}, defaults...)},
		{"resync mode", &syncrv1.RcloneBisyncOptions{ResyncMode: syncrv1.BisyncResyncMode_BISYNC_RESYNC_MODE_PATH2}, resync,
			append([]string{"--resync", "--resync-mode=path2"}, defaults...)},
		{"resync mode without a resync", &syncrv1.RcloneBisyncOptions{ResyncMode: syncrv1.BisyncResyncMode_BISYNC_RESYNC_MODE_PATH2}, testState, defaults},
		{"force", nil, force, append([]string{"--force"}, defaults...)},
		{"every option", &syncrv1.RcloneBisyncOptions{
			Transfers:                    ptr[int32](8),
			Checkers:                     ptr[int32](16),
			BandwidthLimitBytesPerSecond: ptr[int64](1 << 20),
			DryRun:                       true,
			Compare:                      []syncrv1.BisyncCompare{syncrv1.BisyncCompare_BISYNC_COMPARE_CHECKSUM, syncrv1.BisyncCompare_BISYNC_COMPARE_SIZE},
			ConflictResolve:              syncrv1.BisyncConflictResolve_BISYNC_CONFLICT_RESOLVE_LARGER,
			ConflictLoser:                syncrv1.BisyncConflictLoser_BISYNC_CONFLICT_LOSER_PATHNAME,
			ConflictSuffix:               "mine,theirs",
			MaxDeletePercent:             ptr[int32](25),
			CheckAccess:                  true,
			ResyncMode:                   syncrv1.BisyncResyncMode_BISYNC_RESYNC_MODE_SMALLER,
			CreateEmptySrcDirs:           true,
			LowLevelRetries:              ptr[int32](0),
			Filters:                      []*syncrv1.FilterRule{exclude("*.tmp")}, // written to the filters file, not passed
		}, bisyncState{workDir: testState.workDir, filtersFile: testState.filtersFile, resync: true, force: true}, []string{
			"--resync", "--force", "--resync-mode=smaller", "--dry-run", "--compare=size,checksum",
			"--conflict-resolve=larger", "--conflict-loser=pathname", "--conflict-suffix=mine,theirs", "--max-delete=25",
			"--check-access", "--create-empty-src-dirs", "--transfers=8", "--checkers=16", "--bwlimit=1048576B", "--low-level-retries=0",
		}},
		{"unlimited bandwidth", &syncrv1.RcloneBisyncOptions{BandwidthLimitBytesPerSecond: ptr[int64](0)}, testState, defaults},
		{"compare in rclone's order", &syncrv1.RcloneBisyncOptions{Compare: []syncrv1.BisyncCompare{
			syncrv1.BisyncCompare_BISYNC_COMPARE_CHECKSUM, syncrv1.BisyncCompare_BISYNC_COMPARE_MODTIME, syncrv1.BisyncCompare_BISYNC_COMPARE_SIZE,
		}}, testState, append([]string{"--compare=size,modtime,checksum"}, defaults...)},
		{"explicit defaults", &syncrv1.RcloneBisyncOptions{
			ConflictResolve:  syncrv1.BisyncConflictResolve_BISYNC_CONFLICT_RESOLVE_NONE,
			ConflictLoser:    syncrv1.BisyncConflictLoser_BISYNC_CONFLICT_LOSER_NUM,
			MaxDeletePercent: ptr[int32](50),
		}, testState, defaults},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := bisyncArgs(tt.opts, tt.state, "r1:bucket/x", "/data/a")
			if err != nil {
				t.Fatal(err)
			}
			want := append(append(bisyncFixed(tt.state), tt.want...), "--", "r1:bucket/x", "/data/a")
			if !slices.Equal(got, want) {
				t.Errorf("bisyncArgs() =\n%q\nwant\n%q", got, want)
			}
		})
	}
}

func TestBisyncArgsEnums(t *testing.T) {
	t.Parallel()
	for v, want := range map[syncrv1.BisyncResyncMode]string{
		syncrv1.BisyncResyncMode_BISYNC_RESYNC_MODE_UNSPECIFIED: "newer", syncrv1.BisyncResyncMode_BISYNC_RESYNC_MODE_PATH1: "path1",
		syncrv1.BisyncResyncMode_BISYNC_RESYNC_MODE_PATH2: "path2", syncrv1.BisyncResyncMode_BISYNC_RESYNC_MODE_NEWER: "newer",
		syncrv1.BisyncResyncMode_BISYNC_RESYNC_MODE_OLDER: "older", syncrv1.BisyncResyncMode_BISYNC_RESYNC_MODE_LARGER: "larger",
		syncrv1.BisyncResyncMode_BISYNC_RESYNC_MODE_SMALLER: "smaller",
	} {
		if got, err := resyncMode(v); got != want || err != nil {
			t.Errorf("resyncMode(%v) = %q, %v; want %q", v, got, err, want)
		}
	}
	for v, want := range map[syncrv1.BisyncConflictResolve]string{
		syncrv1.BisyncConflictResolve_BISYNC_CONFLICT_RESOLVE_UNSPECIFIED: "none", syncrv1.BisyncConflictResolve_BISYNC_CONFLICT_RESOLVE_NONE: "none",
		syncrv1.BisyncConflictResolve_BISYNC_CONFLICT_RESOLVE_PATH1: "path1", syncrv1.BisyncConflictResolve_BISYNC_CONFLICT_RESOLVE_PATH2: "path2",
		syncrv1.BisyncConflictResolve_BISYNC_CONFLICT_RESOLVE_NEWER: "newer", syncrv1.BisyncConflictResolve_BISYNC_CONFLICT_RESOLVE_OLDER: "older",
		syncrv1.BisyncConflictResolve_BISYNC_CONFLICT_RESOLVE_LARGER: "larger", syncrv1.BisyncConflictResolve_BISYNC_CONFLICT_RESOLVE_SMALLER: "smaller",
	} {
		if got, err := conflictResolve(v); got != want || err != nil {
			t.Errorf("conflictResolve(%v) = %q, %v; want %q", v, got, err, want)
		}
	}
	for v, want := range map[syncrv1.BisyncConflictLoser]string{
		syncrv1.BisyncConflictLoser_BISYNC_CONFLICT_LOSER_UNSPECIFIED: "num", syncrv1.BisyncConflictLoser_BISYNC_CONFLICT_LOSER_NUM: "num",
		syncrv1.BisyncConflictLoser_BISYNC_CONFLICT_LOSER_PATHNAME: "pathname",
	} {
		if got, err := conflictLoser(v); got != want || err != nil {
			t.Errorf("conflictLoser(%v) = %q, %v; want %q", v, got, err, want)
		}
	}
	// Every enum value has a flag, so a value added to the proto fails
	// here until the runner maps it.
	for v := range syncrv1.BisyncResyncMode_name {
		if _, err := resyncMode(syncrv1.BisyncResyncMode(v)); err != nil {
			t.Errorf("resync mode %v: %v", v, err)
		}
	}
	for v := range syncrv1.BisyncConflictResolve_name {
		if _, err := conflictResolve(syncrv1.BisyncConflictResolve(v)); err != nil {
			t.Errorf("conflict resolve %v: %v", v, err)
		}
	}
	for v := range syncrv1.BisyncConflictLoser_name {
		if _, err := conflictLoser(syncrv1.BisyncConflictLoser(v)); err != nil {
			t.Errorf("conflict loser %v: %v", v, err)
		}
	}
}

func TestBisyncArgsRejectsInvalidOptions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		opts *syncrv1.RcloneBisyncOptions
		want string
	}{
		{"transfers 0", &syncrv1.RcloneBisyncOptions{Transfers: ptr[int32](0)}, "transfers"},
		{"transfers 65", &syncrv1.RcloneBisyncOptions{Transfers: ptr[int32](65)}, "transfers"},
		{"checkers 257", &syncrv1.RcloneBisyncOptions{Checkers: ptr[int32](257)}, "checkers"},
		{"negative bandwidth", &syncrv1.RcloneBisyncOptions{BandwidthLimitBytesPerSecond: ptr[int64](-1)}, "bandwidth"},
		{"low-level retries 1001", &syncrv1.RcloneBisyncOptions{LowLevelRetries: ptr[int32](1001)}, "low-level-retries"},
		{"negative low-level retries", &syncrv1.RcloneBisyncOptions{LowLevelRetries: ptr[int32](-1)}, "low-level-retries"},
		{"max delete 0", &syncrv1.RcloneBisyncOptions{MaxDeletePercent: ptr[int32](0)}, "max-delete"},
		{"max delete 101", &syncrv1.RcloneBisyncOptions{MaxDeletePercent: ptr[int32](101)}, "max-delete"},
		{"compare twice", &syncrv1.RcloneBisyncOptions{Compare: []syncrv1.BisyncCompare{2, 1, 2}}, "more than once"},
		{"compare unspecified", &syncrv1.RcloneBisyncOptions{Compare: []syncrv1.BisyncCompare{0}}, "compare entry 0"},
		{"compare unknown", &syncrv1.RcloneBisyncOptions{Compare: []syncrv1.BisyncCompare{1, 9}}, "compare entry 9"},
		{"conflict resolve unknown", &syncrv1.RcloneBisyncOptions{ConflictResolve: 99}, "conflict_resolve"},
		// rclone's "delete" loser has no enum value; whatever number it
		// would get is refused.
		{"conflict loser unknown", &syncrv1.RcloneBisyncOptions{ConflictLoser: 3}, "conflict_loser"},
		{"resync mode unknown", &syncrv1.RcloneBisyncOptions{ResyncMode: 7}, "resync_mode"},
		{"suffix with three parts", &syncrv1.RcloneBisyncOptions{ConflictSuffix: "a,b,c"}, "more than two"},
		{"suffix with an empty part", &syncrv1.RcloneBisyncOptions{ConflictSuffix: "a,"}, "1 to 32"},
		{"suffix too long", &syncrv1.RcloneBisyncOptions{ConflictSuffix: strings.Repeat("a", 33)}, "1 to 32"},
		{"suffix with a space", &syncrv1.RcloneBisyncOptions{ConflictSuffix: "a b"}, "only letters"},
		{"suffix with a slash", &syncrv1.RcloneBisyncOptions{ConflictSuffix: "../x"}, "only letters"},
		{"suffix with a flag", &syncrv1.RcloneBisyncOptions{ConflictSuffix: "x --config=y"}, "only letters"},
		{"suffix not ASCII", &syncrv1.RcloneBisyncOptions{ConflictSuffix: "é"}, "only letters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := bisyncArgs(tt.opts, testState, "r1:x", "/data/a")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("bisyncArgs() error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
	// Several problems are reported together.
	_, err := bisyncOptionFlags(&syncrv1.RcloneBisyncOptions{Transfers: ptr[int32](0), MaxDeletePercent: ptr[int32](0)}, false)
	if err == nil || !strings.Contains(err.Error(), "transfers") || !strings.Contains(err.Error(), "max-delete") {
		t.Errorf("combined error = %v", err)
	}
}

func TestFilterLines(t *testing.T) {
	t.Parallel()
	got, err := filterLines([]*syncrv1.FilterRule{exclude("*.tmp"), include("/photos/**"), exclude("-rf --config x")})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"- *.tmp", "+ /photos/**", "- -rf --config x"}; !slices.Equal(got, want) {
		t.Errorf("filterLines() = %q, want %q", got, want)
	}
	if got, err := filterLines(nil); len(got) != 0 || err != nil {
		t.Errorf("filterLines(nil) = %q, %v", got, err)
	}
	for _, bad := range [][]*syncrv1.FilterRule{
		{exclude("a\nb")}, {exclude("a\rb")}, {exclude("a\x00")}, {exclude("  ")}, {{Pattern: "x"}},
	} {
		if _, err := filterLines(bad); err == nil {
			t.Errorf("filterLines(%v) succeeded", bad)
		}
	}
}

func TestCheckPair(t *testing.T) {
	t.Parallel()
	long := func(alias string, n int) string { return alias + ":" + strings.Repeat("x", n-len(alias)-1) }
	tests := []struct {
		name         string
		path1, path2 string
		want         string // "" means accepted
	}{
		{"local and remote", "/data/a", "r1:bucket/a", ""},
		{"two remotes", "r1:bucket/a", "r2:bucket/a", ""},
		{"siblings", "/data/a", "/data/b", ""},
		{"sibling with a common prefix", "/data/ab", "/data/a", ""},
		{"remote siblings", "r1:bucket/ab", "r1:bucket/a", ""},
		{"200 bytes", long("r1", 100), long("r2", 100), ""},
		{"201 bytes", long("r1", 101), long("r2", 100), "too long for a two-way sync"},
		{"equal local", "/data/a", "/data/a", "one is inside the other"},
		{"nested local", "/data/a", "/data/a/b", "one is inside the other"},
		{"nested local reversed", "/data/a/b/c", "/data/a", "one is inside the other"},
		{"equal remote", "r1:bucket/a", "r1:bucket/a/", "one is inside the other"},
		{"nested remote", "r1:bucket", "r1:bucket/a", "one is inside the other"},
		{"remote root", "r1:", "r1:bucket", "one is inside the other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := checkPair(fromSlash(tt.path1), fromSlash(tt.path2))
			switch {
			case tt.want == "" && err != nil:
				t.Errorf("checkPair() = %v, want nil", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Errorf("checkPair() = %v, want %q", err, tt.want)
			}
		})
	}
}

// fromSlash turns a slash-separated absolute test path into an absolute
// local path of this OS (on a C: drive on Windows); remote paths stay.
func fromSlash(p string) string {
	if !strings.HasPrefix(p, "/") {
		return p
	}
	if runtime.GOOS == "windows" {
		return `C:` + filepath.FromSlash(p)
	}
	return p
}
