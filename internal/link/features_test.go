package link

import (
	"slices"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
)

func TestFeatures(t *testing.T) {
	t.Parallel()
	want := []string{"probe", "restic.v2", "restic.delete", "job.time_limit", "job.hooks", "rclone.bisync"}
	if got := Features(); !slices.Equal(got, want) {
		t.Errorf("Features() = %q, want %q", got, want)
	}
	if FeatureProbe != want[0] || FeatureResticV2 != want[1] || FeatureResticDelete != want[2] || FeatureJobTimeLimit != want[3] ||
		FeatureJobHooks != want[4] || FeatureRcloneBisync != want[5] {
		t.Error("feature constants changed: they are part of the wire contract")
	}
}

func TestProbeOpOf(t *testing.T) {
	t.Parallel()
	tests := []struct {
		spec *syncrv1.ProbeSpec
		want string
	}{
		{spec: nil, want: ""},
		{spec: &syncrv1.ProbeSpec{}, want: ""},
		{spec: &syncrv1.ProbeSpec{Op: &syncrv1.ProbeSpec_ListLocal{ListLocal: &syncrv1.ProbeListLocal{}}}, want: "list_local"},
		{spec: &syncrv1.ProbeSpec{Op: &syncrv1.ProbeSpec_ListRemote{ListRemote: &syncrv1.ProbeListRemote{}}}, want: "list_remote"},
		{spec: &syncrv1.ProbeSpec{Op: &syncrv1.ProbeSpec_About{About: &syncrv1.ProbeAbout{}}}, want: "about"},
		{spec: &syncrv1.ProbeSpec{Op: &syncrv1.ProbeSpec_ResticSnapshots{ResticSnapshots: &syncrv1.ProbeResticSnapshots{}}}, want: "restic_snapshots"},
		{spec: &syncrv1.ProbeSpec{Op: &syncrv1.ProbeSpec_ResticLs{ResticLs: &syncrv1.ProbeResticLs{}}}, want: "restic_ls"},
		{spec: &syncrv1.ProbeSpec{Op: &syncrv1.ProbeSpec_ResticInit{ResticInit: &syncrv1.ProbeResticInit{}}}, want: "restic_init"},
		{spec: &syncrv1.ProbeSpec{Op: &syncrv1.ProbeSpec_ResticConfig{ResticConfig: &syncrv1.ProbeResticConfig{}}}, want: "restic_config"},
		{spec: &syncrv1.ProbeSpec{Op: &syncrv1.ProbeSpec_SftpHostKeys{SftpHostKeys: &syncrv1.ProbeSftpHostKeys{}}}, want: "sftp_host_keys"},
	}
	for _, tt := range tests {
		if got := ProbeOpOf(tt.spec); got != tt.want {
			t.Errorf("ProbeOpOf(%v) = %q, want %q", tt.spec, got, tt.want)
		}
	}
}

func TestProbeOps(t *testing.T) {
	t.Parallel()
	want := []string{"list_local", "list_remote", "about", "restic_snapshots", "restic_ls", "restic_init", "restic_config", "sftp_host_keys"}
	if got := ProbeOps(); !slices.Equal(got, want) {
		t.Errorf("ProbeOps() = %q, want %q", got, want)
	}
	for _, op := range want {
		if !IsProbeOp(op) {
			t.Errorf("IsProbeOp(%q) = false", op)
		}
	}
	for _, op := range []string{"", "remotes", "op", "list", "ListLocal", "rclone_transfer", "ProbeSftpHostKeys"} {
		if IsProbeOp(op) {
			t.Errorf("IsProbeOp(%q) = true", op)
		}
	}
}
