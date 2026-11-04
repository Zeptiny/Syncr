package version

import (
	"runtime/debug"
	"strings"
	"testing"
	"time"
)

func TestResolve(t *testing.T) {
	t.Parallel()

	noBuildInfo := func() (*debug.BuildInfo, bool) { return nil, false }
	withBuildInfo := func(mainVersion string, settings ...debug.BuildSetting) func() (*debug.BuildInfo, bool) {
		return func() (*debug.BuildInfo, bool) {
			return &debug.BuildInfo{Main: debug.Module{Version: mainVersion}, Settings: settings}, true
		}
	}

	tests := []struct {
		name          string
		version       string
		commit        string
		buildTime     string
		readBuildInfo func() (*debug.BuildInfo, bool)
		wantVersion   string
		wantCommit    string
		wantBuildTime time.Time
	}{
		{
			name:          "ldflags win",
			version:       "v2.0.0",
			commit:        "abc123",
			buildTime:     "2026-01-02T03:04:05Z",
			readBuildInfo: withBuildInfo("v9.9.9", debug.BuildSetting{Key: "vcs.revision", Value: "zzz"}),
			wantVersion:   "v2.0.0",
			wantCommit:    "abc123",
			wantBuildTime: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		},
		{
			name:          "nothing known",
			readBuildInfo: noBuildInfo,
			wantVersion:   DevVersion,
			wantCommit:    UnknownCommit,
		},
		{
			name:          "devel module version is ignored",
			readBuildInfo: withBuildInfo("(devel)"),
			wantVersion:   DevVersion,
			wantCommit:    UnknownCommit,
		},
		{
			name: "falls back to build info",
			readBuildInfo: withBuildInfo("v2.1.0",
				debug.BuildSetting{Key: "vcs.revision", Value: "deadbeef"},
				debug.BuildSetting{Key: "vcs.modified", Value: "true"},
			),
			wantVersion: "v2.1.0",
			wantCommit:  "deadbeef-dirty",
		},
		{
			name:          "invalid build time is ignored",
			buildTime:     "yesterday",
			readBuildInfo: noBuildInfo,
			wantVersion:   DevVersion,
			wantCommit:    UnknownCommit,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := resolve(tt.version, tt.commit, tt.buildTime, tt.readBuildInfo)
			if got.Version != tt.wantVersion {
				t.Errorf("Version = %q, want %q", got.Version, tt.wantVersion)
			}
			if got.Commit != tt.wantCommit {
				t.Errorf("Commit = %q, want %q", got.Commit, tt.wantCommit)
			}
			if !got.BuildTime.Equal(tt.wantBuildTime) {
				t.Errorf("BuildTime = %v, want %v", got.BuildTime, tt.wantBuildTime)
			}
			if got.GoVersion == "" {
				t.Error("GoVersion is empty")
			}
		})
	}
}

func TestInfoString(t *testing.T) {
	t.Parallel()
	info := Info{Version: "v2.0.0", Commit: "abc", BuildTime: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), GoVersion: "go1.27.3"}
	want := "v2.0.0 (commit abc, built 2026-01-02T03:04:05Z, go1.27.3)"
	if got := info.String(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if got := (Info{Version: "dev", Commit: "unknown", GoVersion: "go1.27.3"}).String(); !strings.Contains(got, "built unknown") {
		t.Errorf("String() = %q, want unknown build time", got)
	}
}
