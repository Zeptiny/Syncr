package link

import (
	"slices"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
)

func TestKindOf(t *testing.T) {
	t.Parallel()
	tests := []struct {
		spec *syncrv1.JobSpec
		want string
	}{
		{spec: nil, want: ""},
		{spec: &syncrv1.JobSpec{}, want: ""},
		{spec: &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_RcloneTransfer{RcloneTransfer: &syncrv1.RcloneTransfer{}}}, want: "rclone_transfer"},
		{spec: &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_ResticInit{ResticInit: &syncrv1.ResticInit{}}}, want: "restic_init"},
		{spec: &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_ResticBackup{ResticBackup: &syncrv1.ResticBackup{}}}, want: "restic_backup"},
		{spec: &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_ResticForget{ResticForget: &syncrv1.ResticForget{}}}, want: "restic_forget"},
		{spec: &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_ResticPrune{ResticPrune: &syncrv1.ResticPrune{}}}, want: "restic_prune"},
		{spec: &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_ResticCheck{ResticCheck: &syncrv1.ResticCheck{}}}, want: "restic_check"},
		{spec: &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_ResticRestore{ResticRestore: &syncrv1.ResticRestore{}}}, want: "restic_restore"},
		{spec: &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_ResticSnapshots{ResticSnapshots: &syncrv1.ResticSnapshots{}}}, want: "restic_snapshots"},
		{spec: &syncrv1.JobSpec{Operation: &syncrv1.JobSpec_ResticDeleteData{ResticDeleteData: &syncrv1.ResticDeleteData{}}}, want: "restic_delete_data"},
	}
	for _, tt := range tests {
		if got := KindOf(tt.spec); got != tt.want {
			t.Errorf("KindOf(%v) = %q, want %q", tt.spec, got, tt.want)
		}
	}
}

func TestKinds(t *testing.T) {
	t.Parallel()
	want := []string{
		"rclone_transfer",
		"restic_init", "restic_backup", "restic_forget", "restic_prune",
		"restic_check", "restic_restore", "restic_snapshots", "restic_delete_data",
	}
	if got := Kinds(); !slices.Equal(got, want) {
		t.Errorf("Kinds() = %q, want %q", got, want)
	}
	for _, k := range want {
		if !IsKind(k) {
			t.Errorf("IsKind(%q) = false", k)
		}
	}
	for _, k := range []string{"", "remotes", "max_runtime", "operation", "rclone", "RcloneTransfer"} {
		if IsKind(k) {
			t.Errorf("IsKind(%q) = true", k)
		}
	}
}
